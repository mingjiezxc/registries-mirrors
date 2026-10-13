// Package server assembles the HTTP surface: one dispatcher per listener, the
// API root that docker login probes, the merged catalog, and health endpoints.
package server

import (
	"encoding/json"
	"errors"
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
	"github.com/watsons/rms/internal/selection"
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
	// selection.Observer receives the selection's decision and probe outcomes,
	// so the racing and balancing it does are visible in metrics rather than
	// only in the decision log.
	selection.Observer
	// Request records a completed request.
	Request(listener, ns, method string, status int, d time.Duration)
	// CacheHit and CacheMiss record whether a cached object was served.
	CacheHit(ns string)
	CacheMiss(ns string)
}

// Server owns the listeners and the namespace engines.
type Server struct {
	cfg     *config.Config
	log     *slog.Logger
	table   *namespace.Table
	auth    *auth.Authenticator
	mgr     *registry.Manager
	chooser *selection.Chooser
	obs     Observability

	mu sync.Mutex
	// bound holds the listeners that were successfully bound.
	bound []bound
}

// New validates the wiring and builds every namespace engine.
func New(cfg *config.Config, log *slog.Logger, table *namespace.Table, a *auth.Authenticator, mgr *registry.Manager, chooser *selection.Chooser, obs Observability) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{cfg: cfg, log: log, table: table, auth: a, mgr: mgr, chooser: chooser, obs: obs}
}

// Handler returns the dispatcher for one listener.
//
// listenerNS is the namespace the socket is bound to, or "" for the primary
// listener, which serves every namespace and decides unqualified requests
// through the selector.
func (s *Server) Handler(listenerNS string) http.Handler {
	var h http.Handler = s.newDispatcher(listenerNS)
	if s.obs.Middleware != nil {
		h = s.obs.Middleware(h)
	}
	return h
}

// newDispatcher assembles one dispatcher. The engine lookup and the candidate
// list are taken from the manager, which owns both.
func (s *Server) newDispatcher(listenerNS string) *dispatcher {
	d := &dispatcher{
		log:        s.log,
		table:      s.table,
		auth:       s.auth,
		mgr:        s.mgr,
		chooser:    s.chooser,
		rec:        s.obs.Recorder,
		listenerNS: listenerNS,
		storageFor: s.cfg.NamespaceRoot,
	}
	if s.mgr != nil {
		d.engineFor = s.mgr.Engine
		d.serving = s.mgr.Serving
	}
	return d
}

// dispatcher routes one listener's requests to a namespace engine.
type dispatcher struct {
	log   *slog.Logger
	table *namespace.Table
	auth  *auth.Authenticator
	mgr   *registry.Manager
	// chooser decides which proxy namespace serves an unqualified request. It
	// may be nil — no proxy namespace is configured — in which case such a
	// request is answered 503 instead of forwarded.
	chooser    *selection.Chooser
	rec        Recorder
	listenerNS string
	storageFor func(string) string

	// engineFor returns the registry engine of a namespace, bound to one of its
	// upstreams (empty means "the namespace's sticky one"). It is the
	// manager's Engine; tests replace it with an engine they can observe, which
	// is what makes the selection wiring testable without starting a registry
	// engine per namespace.
	engineFor func(namespace, upstream string) (http.Handler, bool)
	// serving lists the proxy namespaces that can answer a read right now. It
	// is the manager's Serving, and it is the candidate set for an unqualified
	// request: a namespace with no engine answers 503 and must not be probed.
	serving func() []string
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

	// An unqualified request names no configured namespace, so the selector
	// picks the proxy namespace that serves it. This must happen before
	// authentication: the access decision is made against the winning
	// namespace, not against an empty one.
	var reselect *reselectState
	if res.NeedsSelection {
		repo, ref, op := clientRepoRefOp(r.URL.Path)
		if !d.decideUnqualified(w, r, repo, ref, op, &res) {
			return
		}
		reselect = &reselectState{repo: repo, ref: ref, op: op}
	} else if res.Kind == namespace.KindRepository && res.Mode == config.ModeProxy {
		// The path named its namespace, so there is nothing to select between
		// namespaces — but there is still the choice of which of that
		// namespace's upstreams serves it, and that is the same race.
		//
		// It happens before authentication for the same reason the unqualified
		// selection does: a request that is eventually denied must not change
		// what the namespace would serve, and the decision is cached either way.
		repo, ref, op := clientRepoRefOp(res.EnginePath)
		d.chooseUpstream(r, &res, repo, ref, op)
		reselect = &reselectState{repo: repo, ref: ref, op: op, qualified: true}
	}

	id, ok, present := d.auth.Authenticate(r)
	denied := d.auth.Decide(r, res, id, ok, present)
	if denied != nil {
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
		engine, found := d.registryEngine(res.Namespace, res.Upstream)
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

		// A 5xx from a proxy read is the upstream failing to serve content it
		// was probed for — the "API root answers, content does not" shape. It
		// is reported to the namespace's endpoint health, so the next decision
		// stops choosing that mirror. A 404 is not reported: it is the answer
		// to a question about content that the client itself may have got
		// wrong, and retrying once (below) is all it warrants.
		if rec.status >= http.StatusInternalServerError && res.Mode == config.ModeProxy && res.Upstream != "" && d.mgr != nil {
			d.mgr.ObserveContentProbe(res.Namespace, res.Upstream, false, 0,
				fmt.Errorf("engine answered %d for %s", rec.status, r.URL.Path))
		}

		if reselect != nil {
			d.tryReselect(rec, r, res, *reselect)
		}
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

// registryEngine resolves a namespace (and the upstream it is to be served by)
// to its engine. It is a method rather than a direct manager call so the
// dispatcher can be exercised without a started manager; production wiring
// always goes through the manager.
func (d *dispatcher) registryEngine(ns, upstream string) (http.Handler, bool) {
	if d.engineFor == nil {
		return nil, false
	}
	return d.engineFor(ns, upstream)
}

// servingNamespaces is the candidate set for an unqualified request: every proxy
// namespace that can answer a read, cached-content-only ones included.
func (d *dispatcher) servingNamespaces() []string {
	if d.serving == nil {
		return nil
	}
	return d.serving()
}

// decideUnqualified routes an unqualified request — one whose path named no
// configured namespace — to the proxy namespace that should serve it. It fills
// res in on success and writes the registry v2 error envelope on failure, in
// which case it returns false.
//
// Two shapes reach here, and they are decided differently:
//
//   - a manifest (or tag list) request carries a repository name, so the
//     selector probes the candidates for it;
//   - a blob request carries only a digest, because a client that already has
//     the manifest asks for its blobs at /v2/blobs/<digest> — no repository
//     name anywhere in the path. There is nothing to probe for, so the blob is
//     routed by the affinity the repository's manifest decision recorded (and,
//     failing that, by which candidate already holds the digest on disk). See
//     decideBlob.
func (d *dispatcher) decideUnqualified(w http.ResponseWriter, r *http.Request, repo, ref, op string, res *namespace.Resolution) bool {
	if repo != "" {
		return d.decideProxy(w, r, repo, op, res)
	}

	if digest, ok := blobDigestOf(op); ok {
		return d.decideBlob(w, digest, res)
	}

	// No repository name and no digest: an upload session continuation
	// (/v2/blobs/uploads/<id>) or some other odd path. Neither the affinity
	// index (keyed by repository) nor the on-disk lookup (keyed by digest) can
	// say which namespace owns it, and a blob upload can only be addressed
	// through an explicit namespace anyway — so 503, which a client retries,
	// rather than a 404 that claims the content does not exist.
	writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE",
		fmt.Sprintf("cannot determine which namespace serves %s: the path names neither a repository nor a blob digest", r.URL.Path))
	return false
}

// decideBlob routes an unqualified blob request, whose path carries a digest
// but no repository name.
//
// The blob is looked up through the most recent manifest decision (affinity)
// and, failing that, through the candidates' local storage — see
// selection.Chooser.DecideBlob. It is not probed: a digest says nothing about
// which upstream serves it, and a client that reached this point already has
// the manifest.
func (d *dispatcher) decideBlob(w http.ResponseWriter, digest string, res *namespace.Resolution) bool {
	target, ok := d.blobTarget(digest)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE",
			fmt.Sprintf("no configured namespace can be determined for blob %s", digest))
		return false
	}
	d.applyBlobTarget(target, digest, res)
	return true
}

// blobTarget asks the selector which namespace serves a digest.
//
// There is exactly one lookup, shared by the first attempt and the retry: the
// recent manifest decision (affinity) or, failing that, a refusal — a digest on
// some namespace's disk tells us the namespace but not the repository it is
// reachable through, and a blob cannot be forwarded without one (see
// blobEnginePath). A retry therefore asks the same question again rather than
// trying a different namespace, because no other namespace can be addressed
// either.
func (d *dispatcher) blobTarget(digest string) (selection.Target, bool) {
	if d.chooser == nil {
		return selection.Target{}, false
	}

	target, err := d.chooser.DecideBlob(digest)
	if err != nil || target.Namespace == "" {
		return selection.Target{}, false
	}
	if target.EnginePath == "" {
		// Defence in depth: the selector currently refuses this case itself, so
		// a namespace without a repository should never reach here. Forwarding
		// it would send the engine a blob path with no repository segment.
		d.log.Warn("blob decision carries no repository, cannot be forwarded",
			"digest", digest, "namespace", target.Namespace)
		return selection.Target{}, false
	}
	return target, true
}

// applyBlobTarget fills res in for a blob whose namespace and repository have
// been decided.
func (d *dispatcher) applyBlobTarget(target selection.Target, digest string, res *namespace.Resolution) {
	res.Namespace = target.Namespace
	res.Mode = config.ModeProxy
	res.EnginePath = blobEnginePath(target.EnginePath, digest)
	res.Upstream = target.Upstream
	res.NeedsSelection = false
}

// blobEnginePath builds the path a namespace's engine must see for a blob.
//
// A client that already holds the manifest asks for that manifest's blobs by
// digest alone — /v2/blobs/<digest> — so the client's own path names no
// repository. rms puts the repository segment back: the engine looks a blob up
// under the repository that references it (the same repository the manifest was
// fetched with, official-library rule included), so forwarding the client's
// path unchanged would make the engine search under no repository at all and
// answer 404 for content sitting right there on disk.
//
// repoLevelPath is a repository-level path as the selector reports it, with
// that namespace's rules already applied — "library/alpine" for a
// single-segment repository in a namespace with official_library enabled.
func blobEnginePath(repoLevelPath, digest string) string {
	return "/v2/" + repoLevelPath + "/blobs/" + digest
}

// blobDigestOf returns the digest of an unqualified blob read. An upload
// session (/blobs/uploads/<id>) has no digest and does not match.
func blobDigestOf(op string) (string, bool) {
	if !strings.HasPrefix(op, "/blobs/") || strings.HasPrefix(op, "/blobs/uploads/") {
		return "", false
	}
	digest := strings.TrimPrefix(op, "/blobs/")
	if digest == "" {
		return "", false
	}
	return digest, true
}

// decideProxy resolves a request that names a repository but no namespace, by
// asking the selector which proxy namespace has it. It fills res in on success
// and writes the registry v2 error envelope on failure, returning false.
//
// A repository is asked about in two different ways, and picking the wrong one
// answers a question nobody asked:
//
//   - /v2/<repo>/manifests/<ref>: the reference names a manifest, so candidates
//     are probed on the manifest path;
//   - /v2/<repo>/blobs/<digest>: the digest names a blob. Probing
//     /v2/<repo>/manifests/<digest> would ask whether a *manifest* is published
//     under that digest — not what the client wants, and not something an
//     upstream would have. Every candidate answers "absent" and a blob that
//     exists is reported as missing. The blob path is the only place where a
//     blob's existence can be asked about.
//
// op is the operation suffix the client asked for, and is appended to the
// winning repository-level EnginePath so downstream cache classification still
// sees the manifests/, blobs/ and tags/ sentinels it depends on.
func (d *dispatcher) decideProxy(w http.ResponseWriter, r *http.Request, repo, op string, res *namespace.Resolution) bool {
	if d.chooser == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE",
			"no proxy namespace is configured to select from")
		return false
	}

	candidates := d.candidates(repo)
	if len(candidates) == 0 {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE",
			"no proxy namespace is configured to select from")
		return false
	}

	// A repository-scoped blob: the question is about the digest, on the blob
	// path.
	if digest, ok := blobDigestOf(op); ok {
		target, err := d.chooser.DecideBlobInRepo(r.Context(), candidates, repo, digest)
		if err != nil {
			d.writeSelectionFailure(w, err, fmt.Sprintf(
				"blob %s of repository %s is not available in any configured namespace", digest, repo))
			return false
		}
		res.Namespace = target.Namespace
		res.Mode = config.ModeProxy
		res.EnginePath = blobEnginePath(target.EnginePath, digest)
		res.Upstream = target.Upstream
		res.NeedsSelection = false
		return true
	}

	ref := referenceOf(op)

	target, err := d.chooser.Decide(r.Context(), candidates, repo, ref)
	if err != nil {
		d.writeSelectionFailure(w, err, fmt.Sprintf(
			"repository %s:%s is not available in any configured namespace", repo, ref))
		return false
	}

	res.Namespace = target.Namespace
	res.Mode = config.ModeProxy
	res.EnginePath = "/v2/" + target.EnginePath + op
	res.Upstream = target.Upstream
	res.NeedsSelection = false
	return true
}

// writeSelectionFailure maps a selection error onto the registry v2 response.
// notFound is the message used when the answer is a definitive "nobody has it";
// ErrUnreachable and unexpected errors are 503, because a client retries those.
func (d *dispatcher) writeSelectionFailure(w http.ResponseWriter, err error, notFound string) {
	switch {
	case errors.Is(err, selection.ErrNotFound):
		// Every reachable candidate answered and none has it. This is a v2
		// NAME_UNKNOWN, not a 503.
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", notFound)

	case errors.Is(err, selection.ErrUnreachable):
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE",
			"no configured namespace could be probed for this request")

	default:
		d.log.Error("selection failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE",
			"the namespace selection failed")
	}
}

// candidates builds the selector's candidate list for an unqualified request:
// every proxy namespace that can answer a read right now, in namespace-name
// order so the outcome does not depend on map iteration.
//
// The candidate set comes from the manager rather than from the configuration,
// because a namespace with no engine at all answers 503 and cannot serve
// anything — probing it would turn a working namespace into a slower decision
// for no benefit.
//
// Each candidate's EnginePath is that namespace's repository-level path
// *without* the /v2/ prefix — "library/alpine", not "/v2/library/alpine". That
// is what the selector appends to its own "/v2/" and "/manifests/<ref>" when it
// probes, and what it hands back as Target.EnginePath. The official_library
// rule (Docker Hub's single-segment repository means library/<name>) is applied
// per candidate, because it is a property of the namespace and not of the
// request.
func (d *dispatcher) candidates(repo string) []selection.Candidate {
	serving := d.servingNamespaces()

	out := make([]selection.Candidate, 0, len(serving))
	for _, name := range serving {
		engineRepo := repo
		if pc, ok := d.table.ProxyConfig(name); ok && pc.OfficialLibrary && repo != "" && !strings.Contains(repo, "/") {
			engineRepo = "library/" + repo
		}
		out = append(out, selection.Candidate{
			Namespace:  name,
			EnginePath: engineRepo,
		})
	}
	return out
}

// chooseUpstream picks which of a namespace's upstreams serves a request whose
// path already named that namespace, and records it on the resolution.
//
// It is deliberately best-effort. The decision is an optimisation — the fastest
// mirror among the ones that hold the content — not a gate: when no upstream can
// be probed, the request goes to the namespace's sticky engine, which still
// serves what is cached, and only a cache miss then fails. Refusing here instead
// would turn a working cache into a 503 because one mirror died, which is
// exactly what the cache exists to prevent.
//
// Nothing is written: proxy namespaces are read-only, and a write would be
// rejected by the authorization layer anyway.
func (d *dispatcher) chooseUpstream(r *http.Request, res *namespace.Resolution, repo, ref, op string) {
	if d.chooser == nil || res.Namespace == "" || repo == "" {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return
	}

	if _, ok := blobDigestOf(op); ok {
		// A blob is content-addressed, so every upstream that has it serves the
		// same bytes: follow the manifest decision made for this namespace and
		// repository instead of probing the blob path again. Without such a
		// decision the sticky engine serves it, which is the same thing that
		// happens before the manifest of a pull is known.
		if target, ok := d.chooser.QualifiedUpstream(res.Namespace, repo); ok {
			res.Upstream = target.Upstream
		}
		return
	}
	if ref == "" {
		// tags/list and the other reference-less operations have nothing to ask
		// an upstream about.
		return
	}

	target, err := d.chooser.DecideQualified(r.Context(), res.Namespace, repo, ref)
	if err != nil {
		if !errors.Is(err, selection.ErrNotFound) && !errors.Is(err, selection.ErrUnreachable) {
			d.log.Warn("upstream selection failed", "namespace", res.Namespace,
				"repo", repo, "ref", ref, "error", err)
		}
		return
	}
	res.Upstream = target.Upstream
}

// reselectState carries what a second attempt needs.
type reselectState struct {
	repo string
	ref  string
	op   string
	// qualified marks a request whose path named its namespace: the retry
	// re-decides within that namespace instead of re-racing all of them.
	qualified bool
}

// tryReselect re-decides an unqualified read that the selected namespace failed
// to serve.
//
// It exists because the selector's decision is a probe, not a guarantee: the
// upstream can drop the repository between the probe and the fetch, and the
// selected namespace can answer from a cache that still holds a stale entry.
// Dropping the cached decision and asking once more turns that into a retry
// instead of an error the client would have to work around.
//
// Only reads are retried — a write that failed has already written something,
// and re-sending it could duplicate it — and only when the failed attempt wrote
// nothing yet, because after a header is on the wire the response is no longer
// ours to replace. The second attempt writes straight to the client, and if it
// fails too the client sees that real failure. The retry happens at most once:
// no loop, and no third attempt.
func (d *dispatcher) tryReselect(rec *statusRecorder, r *http.Request, res namespace.Resolution, st reselectState) {
	if d.chooser == nil {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return
	}
	if rec.status < http.StatusNotFound {
		return
	}
	if rec.wroteHeader {
		// The failed response is already on the wire.
		return
	}

	// Drop the cached decision before asking again, so the retry cannot land on
	// the very namespace that just failed. The invalidation has to match how
	// the decision was keyed:
	//
	//   - a manifest request has a repository and a reference: drop that
	//     (repo, ref) entry and the repository affinity it wrote;
	//   - a blob request has no reference (it addresses content by digest), so
	//     only the repository affinity exists to drop — and only when its path
	//     named a repository at all. At /v2/blobs/<digest> the repository is not
	//     in the path, so there is nothing to invalidate and the digest is what
	//     the retry re-decides on.
	if st.repo != "" {
		switch {
		case st.qualified:
			// The decision is scoped to the namespace the client wrote; only
			// that entry (and its affinity) is stale.
			d.chooser.InvalidateQualified(res.Namespace, st.repo, st.ref)
		case st.ref == "":
			d.chooser.InvalidateRepo(st.repo)
		default:
			d.chooser.Invalidate(st.repo, st.ref)
		}
	}

	target, enginePath, ok := d.retryTarget(r, st, res.Namespace)
	if !ok {
		return
	}

	engine, found := d.registryEngine(target.Namespace, target.Upstream)
	if !found {
		return
	}

	inner := r.Clone(r.Context())
	inner.URL.Path = enginePath
	inner.URL.RawPath = ""

	retry := &statusRecorder{ResponseWriter: rec.ResponseWriter, status: http.StatusOK}
	engine.ServeHTTP(retry, inner)

	if retry.wroteHeader {
		// The retry owns the response now; report its outcome rather than the
		// discarded first attempt's. When the retry wrote nothing, the first
		// engine's response is what the client sees: it is already on the wire
		// and a second writeError would only append to its body.
		rec.status = retry.status
		rec.wroteHeader = true
	}
}

// retryTarget re-decides which namespace serves a failed request and returns
// the engine path to forward to it. A request that named a repository goes
// through the repository decision; one that carried only a blob digest goes
// through the blob decision — the same lookup the original attempt used.
//
// The engine path is built from the repository-level path in both cases, never
// from the client's own path: a client addressing a blob omits the repository
// segment, and the engine needs it (see blobEnginePath).
func (d *dispatcher) retryTarget(r *http.Request, st reselectState, failed string) (selection.Target, string, bool) {
	if st.repo == "" {
		digest, ok := blobDigestOf(st.op)
		if !ok {
			return selection.Target{}, "", false
		}
		target, ok := d.blobTarget(digest)
		if !ok || target.EnginePath == "" {
			return selection.Target{}, "", false
		}
		return target, blobEnginePath(target.EnginePath, digest), true
	}

	// A namespace-qualified request re-decides within its own namespace. There
	// is no other namespace it could be served from, so a failure here is "this
	// namespace's upstreams could not serve it", not "try somewhere else".
	if st.qualified {
		if _, ok := blobDigestOf(st.op); ok {
			target, ok := d.chooser.QualifiedUpstream(failed, st.repo)
			if !ok || target.Upstream == "" {
				return selection.Target{}, "", false
			}
			digest, _ := blobDigestOf(st.op)
			return target, blobEnginePath(target.EnginePath, digest), true
		}
		if st.ref == "" {
			return selection.Target{}, "", false
		}
		target, err := d.chooser.DecideQualified(r.Context(), failed, st.repo, st.ref)
		if err != nil || target.Namespace == "" {
			return selection.Target{}, "", false
		}
		return target, "/v2/" + target.EnginePath + st.op, true
	}

	candidates := d.candidates(st.repo)
	if len(candidates) == 0 {
		return selection.Target{}, "", false
	}

	// Same split as the first attempt: a blob is re-decided on the blob path,
	// never by probing the repository's manifests for the digest.
	if digest, ok := blobDigestOf(st.op); ok {
		target, err := d.chooser.DecideBlobInRepo(r.Context(), candidates, st.repo, digest)
		if err != nil || target.Namespace == "" {
			return selection.Target{}, "", false
		}
		return target, blobEnginePath(target.EnginePath, digest), true
	}

	target, err := d.chooser.Decide(r.Context(), candidates, st.repo, st.ref)
	if err != nil || target.Namespace == "" {
		return selection.Target{}, "", false
	}
	return target, "/v2/" + target.EnginePath + st.op, true
}

// clientRepoRefOp splits a client request path into the repository it names,
// the reference (tag or digest) when the operation carries one, and the
// operation suffix including its leading slash.
//
// The split has to happen on the client's path rather than on a resolution,
// because for an unqualified request the resolution deliberately leaves the
// repository as written and has no engine path yet.
func clientRepoRefOp(urlPath string) (repo, ref, op string) {
	rest, ok := strings.CutPrefix(urlPath, "/v2/")
	if !ok {
		return "", "", ""
	}
	repo, op = namespace.SplitOperation("/v2/" + strings.TrimSuffix(rest, "/"))
	return repo, referenceOf(op), op
}

// referenceOf returns the reference an operation suffix carries: the tag or
// digest after /manifests/, or the digest after /blobs/. Operations that have
// no reference (tags/list, an upload session, a bare blob) return "".
func referenceOf(op string) string {
	switch {
	case strings.HasPrefix(op, "/manifests/"):
		return strings.TrimPrefix(op, "/manifests/")
	case strings.HasPrefix(op, "/blobs/uploads/"):
		return ""
	case strings.HasPrefix(op, "/blobs/"):
		return strings.TrimPrefix(op, "/blobs/")
	default:
		return ""
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
