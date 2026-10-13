// Package registry owns the registry engine instances, one per namespace, and
// keeps proxy engines pointed at a healthy upstream.
//
// Local (writable) namespaces are built once at startup and never moved: they
// serve from local storage and have no upstream.
//
// Proxy namespaces hold one engine per upstream they have been asked to serve
// from, built lazily on first use, plus the sticky engine their current election
// chose and a read-only engine over their cache for when no upstream answers.
// They all share the namespace's storage root, which is safe because proxy
// namespaces run with cache expiry disabled, so no TTL scheduler is started and
// two engines sharing a root cannot conflict; content is content-addressed and
// written through a unique temporary file plus an atomic rename. The only
// shared mutable object is a tag's link, and the selection layer's digest gate
// is what keeps two engines from writing different content to it.
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
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/watsons/rms/internal/config"
	"github.com/watsons/rms/internal/namespace"
	"github.com/watsons/rms/internal/registryapp"
	"github.com/watsons/rms/internal/storage"
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

// Handler returns the engine for a namespace, without choosing an upstream:
// the sticky one (the namespace's current election) or the cache-only engine
// that keeps cached content pullable while every upstream is down.
func (m *Manager) Handler(ns string) (http.Handler, bool) {
	return m.Engine(ns, "")
}

// Engine returns the engine bound to one of a namespace's upstreams, building
// it on first use.
//
// One engine per (namespace, upstream) is what makes racing usable: a
// distribution proxy engine accepts exactly one RemoteURL, fixed at
// construction, so "serve from the fastest of several mirrors" needs one engine
// per mirror. They share the namespace's storage root, which is safe for two
// reasons: content is content-addressed and written through a unique temporary
// file plus an atomic rename, and cache expiry is disabled for proxy namespaces
// so no TTL scheduler is running. The one shared mutable object, a tag's link,
// is written only by engines that agree on that tag's digest — that agreement
// is what the selection's digest gate establishes before routing here.
//
// upstream == "" means "no choice was made" (a cached decision, or a caller
// that does not race), and the namespace's sticky engine is used.
//
// A build failure is not fatal: the caller falls back to the sticky or
// cache-only engine, so a mirror that dies between the probe and the build
// still leaves its namespace able to serve what it has cached.
func (m *Manager) Engine(ns, upstream string) (http.Handler, bool) {
	if app, ok := m.local[ns]; ok {
		return app, true
	}
	p, ok := m.proxy[ns]
	if !ok {
		return nil, false
	}

	if upstream != "" {
		if app, err := p.engineFor(upstream); err == nil {
			return app, true
		} else {
			m.log.Warn("upstream engine unavailable; falling back to the sticky one",
				"namespace", ns, "upstream", upstream, "error", err)
		}
	}
	if app, ok := p.fallback(); ok {
		return app, true
	}
	return nil, false
}

// UpstreamsFor lists the upstream endpoints a content race may probe for a
// namespace, in configuration order. It is one of the methods implementing
// selection.Upstream.
func (m *Manager) UpstreamsFor(namespace string) []string {
	p, ok := m.proxy[namespace]
	if !ok || p.selector == nil {
		return nil
	}
	return p.selector.Upstreams()
}

// ObserveContentProbe folds the outcome of one content probe into an endpoint's
// health, so a mirror whose API root answers while its content does not is
// dropped from later races. It is one of the methods implementing
// selection.Upstream.
func (m *Manager) ObserveContentProbe(namespace, upstream string, answered bool, latency time.Duration, err error) {
	p, ok := m.proxy[namespace]
	if !ok || p.selector == nil {
		return
	}
	p.selector.ObserveContentProbe(upstream, answered, latency, err)
}

// ContentBadUpstreams lists the upstreams of a namespace that are serving a
// content-failure cooldown. It exists for the readiness/metrics surface.
func (m *Manager) ContentBadUpstreams(namespace string) []string {
	p, ok := m.proxy[namespace]
	if !ok || p.selector == nil {
		return nil
	}
	return p.selector.ContentBad()
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

// Serving lists the proxy namespaces that can answer a read right now, sorted.
//
// It includes namespaces serving cached content only. They have no upstream to
// probe, but they do answer from disk, so whether one can serve a given request
// is settled by looking at that disk — see CachedManifest. Eligibility is
// therefore not something this list decides on its own: the selection chooser
// takes every namespace listed here as a candidate and finds out. A namespace
// with no engine at all is excluded, because it answers 503 and cannot serve
// anything.
//
// This is the candidate set the selection chooser works from; it is one of the
// methods implementing selection.Upstream.
func (m *Manager) Serving() []string {
	var out []string
	for name, p := range m.proxy {
		if state := p.state.Load(); state == stateProxy || state == stateCacheOnly {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// ElectedUpstream returns the upstream URL currently elected for a namespace,
// and whether the namespace has one at all.
//
// A namespace serving cached content only has none, and neither has one that
// was never elected: the caller cannot probe those. The URL is returned even
// while the selector holds it as unhealthy, because a content probe is exactly
// what establishes whether it works — the selector keeps the previous election
// when every endpoint is failing, and that endpoint may still be the one that
// serves this repository.
func (m *Manager) ElectedUpstream(namespace string) (string, bool) {
	p, ok := m.proxy[namespace]
	if !ok || p.selector == nil || p.state.Load() != stateProxy {
		return "", false
	}
	url, _ := p.selector.ElectedForRequest()
	if url == "" {
		return "", false
	}
	return url, true
}

// UpstreamCredentials returns the upstream user and password configured for a
// namespace. Both are empty when the upstream is public, and when the namespace
// is not a configured proxy namespace.
func (m *Manager) UpstreamCredentials(namespace string) (user, pass string) {
	ns, ok := m.cfg.Namespaces.Proxy[namespace]
	if !ok {
		return "", ""
	}
	return ns.Username, ns.Password
}

// CachedManifest reports whether a namespace already holds this repository and
// reference on local disk, whoever can or cannot reach an upstream.
//
// It answers for local and proxy namespaces alike, and it is the selection
// chooser's cheapest evidence: one stat, no network, and it is true exactly
// when the namespace's engine can answer the pull without the upstream. That is
// what lets a namespace keep serving already-cached images while its upstream
// is down — including one that is serving cached content only.
//
// repo is the repository as that namespace's storage sees it, which is the
// selection candidate's EnginePath, not necessarily the name the client wrote.
func (m *Manager) CachedManifest(namespace, repo, ref string) bool {
	if repo == "" || ref == "" {
		return false
	}
	if _, ok := m.proxy[namespace]; !ok {
		if _, ok := m.local[namespace]; !ok {
			return false
		}
	}

	root := m.cfg.NamespaceRoot(namespace)

	// A reference is either a digest, which names an immutable revision, or a
	// tag, which the driver records as a link to one. This is the same
	// distinction the cache-hit classification in internal/server makes.
	if dgst, err := digest.Parse(ref); err == nil && dgst.Validate() == nil {
		return storage.HasRevision(root, repo, dgst)
	}
	return storage.HasTag(root, repo, ref)
}

// CachedBlob reports whether a namespace already holds this blob on local
// disk.
//
// It is the digest-only counterpart of CachedManifest: blob data is
// content-addressed and stored once per namespace, so this needs no repository
// name — which is what lets the selection chooser route a request that carries
// only a digest to a namespace that can answer it without an upstream.
func (m *Manager) CachedBlob(namespace string, dgst digest.Digest) bool {
	if err := dgst.Validate(); err != nil {
		return false
	}
	if _, ok := m.proxy[namespace]; !ok {
		if _, ok := m.local[namespace]; !ok {
			return false
		}
	}
	return storage.HasBlob(m.cfg.NamespaceRoot(namespace), dgst)
}

// UpstreamLatency returns how long the namespace's recent successful health
// probes have taken, or false when no probe has succeeded yet.
//
// The selection chooser uses it as a tie-break weight between candidates whose
// content probes measured the same duration. It is optional: the chooser works
// without it.
func (m *Manager) UpstreamLatency(namespace string) (time.Duration, bool) {
	p, ok := m.proxy[namespace]
	if !ok || p.selector == nil {
		return 0, false
	}
	d := p.selector.LatencyEWMA()
	if d <= 0 {
		return 0, false
	}
	return d, true
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

// proxyNamespace tracks one proxy namespace's engines and upstream election.
type proxyNamespace struct {
	name     string
	spec     registryapp.Spec
	selector *upstream.Selector
	rec      Recorder
	log      *slog.Logger

	// buildMu serialises engine builds. Engines are never swapped out for one
	// another: each (namespace, upstream) keeps its own, so a namespace can
	// serve from any of its mirrors without rebuilding on every decision.
	buildMu sync.Mutex
	// set is the immutable *appSet currently installed. It is replaced wholesale
	// (copy-on-write) under buildMu, so the request path reads it without a lock.
	set     atomic.Pointer[appSet]
	state   atomic.Int32
	lastErr atomic.Pointer[nsError]
	started atomic.Bool
}

// appSet is the set of engines a namespace currently has: one per upstream it
// has been asked to use, the sticky one an election chose, and the read-only
// engine over the cache that keeps a degraded namespace serving.
type appSet struct {
	engines   map[string]*registryapp.App
	sticky    string
	cacheOnly *registryapp.App
}

type nsError struct{ err error }

// fallback returns the engine to use when no upstream was chosen: the sticky
// one, or the cache-only engine. It reports false only when the namespace has
// no engine at all.
func (p *proxyNamespace) fallback() (*registryapp.App, bool) {
	set := p.set.Load()
	if set == nil {
		return nil, false
	}
	if set.sticky != "" {
		if app, ok := set.engines[set.sticky]; ok {
			return app, true
		}
	}
	if set.cacheOnly != nil {
		return set.cacheOnly, true
	}
	// No election and no cache-only engine: any engine at all is better than a
	// 503, and they all serve the same storage root.
	for _, app := range set.engines {
		return app, true
	}
	return nil, false
}

// current returns the namespace's sticky engine, or nil. It is the engine a
// caller that names no upstream gets.
func (p *proxyNamespace) current() *registryapp.App {
	if set := p.set.Load(); set != nil {
		if set.sticky != "" {
			if app, ok := set.engines[set.sticky]; ok {
				return app
			}
		}
		if set.cacheOnly != nil {
			return set.cacheOnly
		}
	}
	return nil
}

// engineFor returns the engine bound to one upstream, building it on first use.
func (p *proxyNamespace) engineFor(url string) (*registryapp.App, error) {
	if url == "" {
		if app, ok := p.fallback(); ok {
			return app, nil
		}
		return nil, fmt.Errorf("namespace %s: no engine installed", p.name)
	}

	p.buildMu.Lock()
	defer p.buildMu.Unlock()

	cur := p.set.Load()
	if cur == nil {
		cur = &appSet{engines: map[string]*registryapp.App{}}
	}
	if app, ok := cur.engines[url]; ok {
		return app, nil
	}

	spec := p.spec
	spec.RemoteURL = url
	app, err := registryapp.Build(spec)
	if err != nil {
		p.lastErr.Store(&nsError{err: err})
		return nil, err
	}

	engines := make(map[string]*registryapp.App, len(cur.engines)+1)
	for k, v := range cur.engines {
		engines[k] = v
	}
	engines[url] = app

	p.set.Store(&appSet{engines: engines, sticky: cur.sticky, cacheOnly: cur.cacheOnly})
	p.state.Store(stateProxy)
	p.lastErr.Store(nil)
	p.log.Info("namespace engine ready", "namespace", p.name, "upstream", url)
	if p.rec != nil {
		p.rec.NamespaceReady(p.name)
	}
	return app, nil
}

// run watches the election and keeps the namespace's sticky engine in step with
// it.
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

// install builds the engine for url and makes it the namespace's sticky one.
func (p *proxyNamespace) install(url string) error {
	if _, err := p.engineFor(url); err != nil {
		return err
	}
	return p.setSticky(url)
}

// setSticky records which engine a caller that names no upstream gets.
func (p *proxyNamespace) setSticky(url string) error {
	p.buildMu.Lock()
	defer p.buildMu.Unlock()

	cur := p.set.Load()
	if cur == nil {
		return fmt.Errorf("namespace %s: no engine installed for %s", p.name, url)
	}
	if _, ok := cur.engines[url]; !ok {
		return fmt.Errorf("namespace %s: no engine installed for %s", p.name, url)
	}
	p.set.Store(&appSet{engines: cur.engines, sticky: url, cacheOnly: cur.cacheOnly})
	return nil
}

// installCacheOnly installs a read-only engine over this namespace's cache, so
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

	cur := p.set.Load()
	if cur == nil {
		cur = &appSet{engines: map[string]*registryapp.App{}}
	}
	p.set.Store(&appSet{engines: cur.engines, sticky: cur.sticky, cacheOnly: app})
	p.state.Store(stateCacheOnly)
	p.lastErr.Store(nil)

	if p.rec != nil {
		p.rec.NamespaceReady(p.name)
	}
	return nil
}

// degraded reports whether the namespace is not a working pull-through cache.
func (p *proxyNamespace) degraded() bool {
	return p.state.Load() != stateProxy
}

func (p *proxyNamespace) shutdown() error {
	p.state.Store(stateUnavailable)
	set := p.set.Swap(nil)
	if set == nil {
		return nil
	}

	var errs []error
	for upstreamURL, app := range set.engines {
		if err := app.Shutdown(); err != nil {
			errs = append(errs, fmt.Errorf("namespace %s upstream %s: %w", p.name, upstreamURL, err))
		}
	}
	if set.cacheOnly != nil {
		if err := set.cacheOnly.Shutdown(); err != nil {
			errs = append(errs, fmt.Errorf("namespace %s cache engine: %w", p.name, err))
		}
	}
	return errors.Join(errs...)
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
