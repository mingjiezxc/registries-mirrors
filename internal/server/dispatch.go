// Package server assembles the HTTP surface: one dispatcher per listener, the
// API root that docker login probes, the merged catalog, and health endpoints.
package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/watsons/rms/internal/auth"
	"github.com/watsons/rms/internal/config"
	"github.com/watsons/rms/internal/namespace"
	"github.com/watsons/rms/internal/registry"
)

// Observability is the optional metrics/tracing hook. A zero value is fine.
type Observability struct {
	// Middleware wraps the final handler, for example to add tracing.
	Middleware func(http.Handler) http.Handler
	// Recorder receives request and lifecycle events. May be nil.
	Recorder Recorder
	// Metrics serves the configured metrics path ("" disables it).
	Metrics http.Handler
	// Pprof serves /debug/pprof when non-nil.
	Pprof http.Handler
}

// Recorder receives events the server observes.
type Recorder interface {
	registry.Recorder
	// Request records a completed request.
	Request(listener, ns, method string, status int, d time.Duration)
	// CacheHit and CacheMiss record whether a cached object was served.
	CacheHit(ns string)
	CacheMiss(ns string)
}

// Server owns the listeners and the namespace engines.
type Server struct {
	cfg   *config.Config
	log   *slog.Logger
	table *namespace.Table
	auth  *auth.Authenticator
	mgr   *registry.Manager
	obs   Observability

	mu sync.Mutex
	// bound holds the listeners that were successfully bound.
	bound []bound
}

// New validates the wiring and builds every namespace engine.
func New(cfg *config.Config, log *slog.Logger, table *namespace.Table, a *auth.Authenticator, mgr *registry.Manager, obs Observability) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{cfg: cfg, log: log, table: table, auth: a, mgr: mgr, obs: obs}
}

// Handler returns the dispatcher for one listener.
//
// listenerNS is the namespace the socket is bound to, or "" for the
// multi-namespace primary listener.
func (s *Server) Handler(listenerNS string) http.Handler {
	var h http.Handler = &dispatcher{
		log:        s.log,
		table:      s.table,
		auth:       s.auth,
		mgr:        s.mgr,
		rec:        s.obs.Recorder,
		listenerNS: listenerNS,
		storageFor: s.cfg.NamespaceRoot,
	}
	if s.obs.Middleware != nil {
		h = s.obs.Middleware(h)
	}
	return h
}

// dispatcher routes one listener's requests to a namespace engine.
type dispatcher struct {
	log        *slog.Logger
	table      *namespace.Table
	auth       *auth.Authenticator
	mgr        *registry.Manager
	rec        Recorder
	listenerNS string
	storageFor func(string) string
}

func (d *dispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Out-of-band endpoints first: they live outside the registry path space.
	switch r.URL.Path {
	case "/healthz":
		d.serveHealthz(w)
		return
	case "/readyz":
		d.serveReadyz(w)
		return
	}
	res, err := d.table.Resolve(d.listenerNS, r.URL.Path)
	if err != nil {
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", fmt.Sprintf("not a registry v2 path: %s", r.URL.Path))
		return
	}

	id, ok, present := d.auth.Authenticate(r)
	if denied := d.auth.Decide(r, res, id, ok, present); denied != nil {
		if denied.Challenge {
			d.auth.Challenge(w)
		}
		writeError(w, denied.Status, denied.Code, denied.Message)
		return
	}

	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

	switch res.Kind {
	case namespace.KindBase:
		// GET /v2/ is the API version probe docker login uses. A 200 here means
		// the supplied credentials (if any) were accepted.
		rec.Header().Set("Content-Type", "application/json; charset=utf-8")
		rec.Header().Set("Docker-Distribution-Api-Version", registryAPIVersion)
		rec.WriteHeader(http.StatusOK)
		_, _ = rec.Write([]byte("{}"))

	case namespace.KindCatalog:
		d.serveCatalog(rec, r)

	default:
		engine, found := d.mgr.Handler(res.Namespace)
		if !found {
			writeError(rec, http.StatusServiceUnavailable, "UNAVAILABLE",
				fmt.Sprintf("namespace %q is temporarily unavailable", res.Namespace))
			break
		}

		if d.rec != nil {
			d.recordCacheOutcome(r, res)
		}

		// Hand the engine the path it expects, leaving the original request
		// untouched for the audit log.
		inner := r.Clone(r.Context())
		inner.URL.Path = res.EnginePath
		inner.URL.RawPath = ""
		engine.ServeHTTP(rec, inner)
	}

	if d.rec != nil {
		d.rec.Request(labelOr(d.listenerNS, multiNamespace), labelOr(res.Namespace, "none"),
			r.Method, rec.status, time.Since(start))
	}
	if rec.status >= http.StatusInternalServerError {
		d.log.Error("request failed",
			"method", r.Method, "path", r.URL.Path, "namespace", res.Namespace, "status", rec.status)
	}
}

// recordCacheOutcome classifies a proxy-namespace read as a cache hit or miss by
// looking at the local storage before the engine serves it.
func (d *dispatcher) recordCacheOutcome(r *http.Request, res namespace.Resolution) {
	if res.Mode != config.ModeProxy {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return
	}

	root := d.storageFor(res.Namespace)
	cached, known := classifyCache(root, res)
	if !known {
		return
	}
	if cached {
		d.rec.CacheHit(res.Namespace)
		return
	}
	d.rec.CacheMiss(res.Namespace)
}

func (d *dispatcher) serveHealthz(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (d *dispatcher) serveReadyz(w http.ResponseWriter) {
	degraded := d.mgr.Degraded()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if len(degraded) > 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "degraded namespaces: %s\n", strings.Join(degraded, ", "))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}

// multiNamespace labels the request metric for the listener that serves every
// namespace.
const multiNamespace = "<multi>"

func labelOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// statusRecorder captures the response status for metrics and logs.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	s.wroteHeader = true
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.WriteHeader(http.StatusOK)
	}
	return s.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer so streaming blob reads are not
// buffered.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// registryAPIVersion is the value clients expect in the API version header.
const registryAPIVersion = "registry/2.0"

// registryError is the error envelope defined by the registry v2 API.
type registryError struct {
	Errors []registryErrorItem `json:"errors"`
}

type registryErrorItem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Docker-Distribution-Api-Version", registryAPIVersion)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(registryError{
		Errors: []registryErrorItem{{Code: code, Message: message}},
	})
}
