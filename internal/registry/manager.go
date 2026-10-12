// Package registry owns the registry engine instances, one per namespace, and
// keeps proxy engines pointed at a healthy upstream.
//
// Local (writable) namespaces are built once at startup and never moved: they
// serve from local storage and have no upstream. Proxy namespaces follow their
// upstream election, so when an endpoint fails the engine is rebuilt against the
// newly elected one. Rebuilding is safe because proxy namespaces run with cache
// expiry disabled, which means no TTL scheduler is started and two engines
// briefly sharing a storage root cannot conflict.
package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/watsons/rms/internal/config"
	"github.com/watsons/rms/internal/namespace"
	"github.com/watsons/rms/internal/registryapp"
	"github.com/watsons/rms/internal/upstream"
)

// Recorder observes lifecycle events. It may be nil.
type Recorder interface {
	// UpstreamElectionChanged reports that a namespace moved to a new upstream.
	UpstreamElectionChanged(namespace, upstream string)
	// NamespaceDegraded reports that a namespace has no working engine.
	NamespaceDegraded(namespace string, err error)
	// NamespaceReady reports that a namespace is serving.
	NamespaceReady(namespace string)
}

// Manager holds the engine for every namespace.
type Manager struct {
	cfg      *config.Config
	table    *namespace.Table
	log      *slog.Logger
	client   *http.Client
	secret   string
	external map[string]string
	rec      Recorder

	local map[string]*registryapp.App
	proxy map[string]*proxyNamespace

	shutdownOnce sync.Once
}

// New builds a Manager. secret is the upload-state secret applied to every
// engine; external maps a namespace to its client-visible origin (empty means
// "derive from the request").
func New(
	cfg *config.Config,
	table *namespace.Table,
	log *slog.Logger,
	client *http.Client,
	secret string,
	external map[string]string,
	rec Recorder,
) *Manager {
	if log == nil {
		log = slog.Default()
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &Manager{
		cfg:      cfg,
		table:    table,
		log:      log,
		client:   client,
		secret:   secret,
		external: external,
		rec:      rec,
		local:    map[string]*registryapp.App{},
		proxy:    map[string]*proxyNamespace{},
	}
}

// Start builds every namespace engine. A failure to build a local namespace is
// fatal, because it means local storage is unusable. A proxy namespace whose
// upstream is unreachable starts degraded instead: the rest of the service,
// including pushes, keeps working, and the namespace comes up when the upstream
// does.
func (m *Manager) Start(ctx context.Context) error {
	var errs []error

	for _, name := range m.table.LocalNames() {
		app, err := m.buildLocal(name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		m.local[name] = app
		m.log.Info("namespace ready", "namespace", name, "mode", string(config.ModeLocal),
			"root", m.cfg.NamespaceRoot(name))
	}

	for _, name := range sortedProxyNames(m.cfg) {
		p, err := m.newProxyNamespace(name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		m.proxy[name] = p

		url, err := p.selector.ElectForStartup(ctx)
		if err != nil {
			m.degrade(ctx, name, p, err)
			continue
		}
		if err := p.install(url); err != nil {
			m.degrade(ctx, name, p, err)
			continue
		}
		m.log.Info("namespace ready", "namespace", name, "mode", string(config.ModeProxy),
			"root", m.cfg.NamespaceRoot(name), "upstream", url)
		go p.run(ctx)
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// degrade handles a proxy namespace whose upstream is unusable at startup.
//
// Instead of answering 503 for everything — which would make every image that is
// already cached unusable during an upstream outage, including across a restart
// — the namespace falls back to a read-only engine over its cache. Cached blobs
// and manifests keep being served, and the selector's loop rebuilds a real
// pull-through engine as soon as an endpoint answers again.
func (m *Manager) degrade(ctx context.Context, name string, p *proxyNamespace, cause error) {
	m.log.Warn("namespace degraded at startup; serving cached content only",
		"namespace", name, "error", cause)

	if err := p.installCacheOnly(); err != nil {
		// Nothing could be built: the namespace answers 503 until an upstream
		// answers. /readyz reports it either way.
		m.log.Error("namespace unavailable", "namespace", name, "error", err)
	} else {
		m.log.Info("serving from cache only", "namespace", name, "root", m.cfg.NamespaceRoot(name))
	}

	if m.rec != nil {
		m.rec.NamespaceDegraded(name, cause)
	}
	go p.run(ctx)
}

func (m *Manager) buildLocal(name string) (*registryapp.App, error) {
	purge := m.cfg.Storage.UploadPurge
	if !purge.Enabled {
		purge = config.UploadPurge{}
	}
	return registryapp.Build(registryapp.Spec{
		Namespace:     name,
		Mode:          config.ModeLocal,
		Root:          m.cfg.NamespaceRoot(name),
		ExternalURL:   m.external[name],
		DeleteEnabled: m.cfg.Storage.DeleteEnabled,
		HTTPSecret:    m.secret,
		UploadPurge:   purge,
		Logger:        m.log,
	})
}

func (m *Manager) newProxyNamespace(name string) (*proxyNamespace, error) {
	nsCfg, ok := m.cfg.Namespaces.Proxy[name]
	if !ok {
		return nil, fmt.Errorf("namespace %s: missing proxy configuration", name)
	}

	health := m.cfg.Upstream.Health
	sel, err := upstream.New(name, nsCfg.Upstreams, m.client, upstream.Options{
		Path:             health.Path,
		Interval:         health.Interval.Std(),
		Timeout:          health.Timeout.Std(),
		FailThreshold:    health.FailThreshold,
		SuccessThreshold: health.SuccessThreshold,
		Enabled:          health.Enabled,
		Logger:           m.log,
	})
	if err != nil {
		return nil, err
	}

	return &proxyNamespace{
		name: name,
		spec: registryapp.Spec{
			Namespace:        name,
			Mode:             config.ModeProxy,
			Root:             m.cfg.NamespaceRoot(name),
			ExternalURL:      m.external[name],
			UpstreamUsername: nsCfg.Username,
			UpstreamPassword: nsCfg.Password,
			TTL:              nsCfg.TTL.Std(),
			// Proxy namespaces cannot receive uploads, so purging is pointless.
			UploadPurge: config.UploadPurge{},
			HTTPSecret:  m.secret,
			Logger:      m.log,
		},
		selector: sel,
		rec:      m.rec,
		log:      m.log,
	}, nil
}

// Handler returns the engine for a namespace.
func (m *Manager) Handler(ns string) (http.Handler, bool) {
	if app, ok := m.local[ns]; ok {
		return app, true
	}
	if p, ok := m.proxy[ns]; ok {
		if app := p.current(); app != nil {
			return app, true
		}
	}
	return nil, false
}

// Degraded lists proxy namespaces that are not working pull-through caches,
// either because they are serving cached content only or because no engine could
// be built at all.
func (m *Manager) Degraded() []string {
	var out []string
	for name, p := range m.proxy {
		if p.degraded() {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// CacheOnly lists proxy namespaces serving cached content only.
func (m *Manager) CacheOnly() []string {
	var out []string
	for name, p := range m.proxy {
		if p.state.Load() == stateCacheOnly {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Namespaces returns the configured namespaces and their modes.
func (m *Manager) Namespaces() map[string]config.Mode {
	out := make(map[string]config.Mode, len(m.local)+len(m.proxy))
	for name := range m.local {
		out[name] = config.ModeLocal
	}
	for name := range m.proxy {
		out[name] = config.ModeProxy
	}
	return out
}

// Shutdown stops every engine.
func (m *Manager) Shutdown() error {
	var errs []error
	m.shutdownOnce.Do(func() {
		for name, app := range m.local {
			if err := app.Shutdown(); err != nil {
				errs = append(errs, fmt.Errorf("namespace %s: %w", name, err))
			}
		}
		for name, p := range m.proxy {
			if err := p.shutdown(); err != nil {
				errs = append(errs, fmt.Errorf("namespace %s: %w", name, err))
			}
		}
	})
	return errors.Join(errs...)
}

// Serving states of a proxy namespace.
const (
	// stateUnavailable: no engine at all, requests get 503.
	stateUnavailable int32 = iota
	// stateCacheOnly: a read-only engine over the cache while the upstream is
	// unreachable. Cached content is served; nothing is fetched.
	stateCacheOnly
	// stateProxy: a pull-through engine backed by an elected upstream.
	stateProxy
)

// proxyNamespace tracks one proxy namespace's engine and upstream election.
type proxyNamespace struct {
	name     string
	spec     registryapp.Spec
	selector *upstream.Selector
	rec      Recorder
	log      *slog.Logger

	// buildMu serialises rebuilds so two elections cannot build concurrently.
	buildMu sync.Mutex
	app     atomic.Pointer[registryapp.App]
	state   atomic.Int32
	lastErr atomic.Pointer[nsError]
	started atomic.Bool
}

type nsError struct{ err error }

func (p *proxyNamespace) current() *registryapp.App { return p.app.Load() }

// run watches the election and rebuilds the engine when it changes.
func (p *proxyNamespace) run(ctx context.Context) {
	if p.started.CompareAndSwap(false, true) {
		p.selector.OnElect(func(url string) {
			if p.rec != nil {
				p.rec.UpstreamElectionChanged(p.name, url)
			}
			if url == "" {
				return
			}
			if err := p.install(url); err != nil {
				p.log.Error("rebuild after election change failed", "namespace", p.name, "error", err)
				if p.rec != nil {
					p.rec.NamespaceDegraded(p.name, err)
				}
			}
		})
	}

	p.selector.Run(ctx)
}

// install builds the pull-through engine against url and swaps it in, shutting
// the previous engine down afterwards.
func (p *proxyNamespace) install(url string) error {
	p.buildMu.Lock()
	defer p.buildMu.Unlock()

	spec := p.spec
	spec.RemoteURL = url

	app, err := registryapp.Build(spec)
	if err != nil {
		p.lastErr.Store(&nsError{err: err})
		return err
	}

	p.swap(app, stateProxy)
	return nil
}

// installCacheOnly swaps in a read-only engine over this namespace's cache, so
// already-cached images stay pullable while the upstream is unreachable.
//
// It uses the same storage root. That is safe because cache expiry is disabled
// for proxy namespaces (no TTL scheduler is running), and the read-only engine
// starts no upload purger because it cannot receive uploads.
func (p *proxyNamespace) installCacheOnly() error {
	p.buildMu.Lock()
	defer p.buildMu.Unlock()

	spec := p.spec
	spec.Mode = config.ModeLocal
	spec.RemoteURL = ""
	spec.ReadOnly = true
	spec.DeleteEnabled = false

	app, err := registryapp.Build(spec)
	if err != nil {
		p.lastErr.Store(&nsError{err: err})
		return err
	}

	p.swap(app, stateCacheOnly)
	return nil
}

// swap installs a new engine and retires the previous one. Callers must hold
// buildMu.
func (p *proxyNamespace) swap(app *registryapp.App, state int32) {
	old := p.app.Swap(app)
	p.state.Store(state)
	p.lastErr.Store(nil)

	if p.rec != nil {
		p.rec.NamespaceReady(p.name)
	}

	if old != nil {
		// Shut the previous engine down after the swap so in-flight requests on
		// it finish before its workers stop.
		if err := old.Shutdown(); err != nil {
			p.log.Warn("previous engine shutdown failed", "namespace", p.name, "error", err)
		}
	}
}

// degraded reports whether the namespace is not a working pull-through cache.
func (p *proxyNamespace) degraded() bool {
	return p.state.Load() != stateProxy
}

func (p *proxyNamespace) shutdown() error {
	p.state.Store(stateUnavailable)
	if app := p.app.Swap(nil); app != nil {
		return app.Shutdown()
	}
	return nil
}

// Err reports the most recent failure for a namespace.
func (p *proxyNamespace) Err() error {
	if e := p.lastErr.Load(); e != nil {
		return e.err
	}
	return nil
}

func sortedProxyNames(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Namespaces.Proxy))
	for name := range cfg.Namespaces.Proxy {
		out = append(out, name)
	}
	// Stable order keeps startup logs and the primary upstream predictable.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
