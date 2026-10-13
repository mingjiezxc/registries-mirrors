package registry

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/watsons/rms/internal/config"
	"github.com/watsons/rms/internal/registryapp"
	"github.com/watsons/rms/internal/storage"
	"github.com/watsons/rms/internal/upstream"
)

// seedCachedManifest writes the two links distribution's filesystem driver
// creates for a cached manifest: the tag link and the revision link.
func seedCachedManifest(t *testing.T, namespaceRoot, repo, tag, dgst string) {
	t.Helper()

	writeLink := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(dgst), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	parsed, err := digest.Parse(dgst)
	if err != nil {
		t.Fatalf("parse digest %q: %v", dgst, err)
	}

	writeLink(storage.TagLinkPath(namespaceRoot, repo, tag))
	writeLink(filepath.Join(storage.V2Root(namespaceRoot), "repositories", filepath.FromSlash(repo),
		"_manifests", "revisions", parsed.Algorithm().String(), parsed.Encoded(), "link"))
}

// TestCachedManifest covers what the selection chooser asks the disk: a tag, a
// digest, and everything that must answer no.
func TestCachedManifest(t *testing.T) {
	const dgst = "sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6"

	root := t.TempDir()
	cfg := &config.Config{
		Storage: config.Storage{Root: root},
		Namespaces: config.Namespaces{
			Proxy: map[string]config.ProxyNamespace{"docker.io": {}},
		},
	}
	m := &Manager{
		cfg:   cfg,
		log:   silentLogger(),
		local: map[string]*registryapp.App{},
		proxy: map[string]*proxyNamespace{"docker.io": {name: "docker.io", log: silentLogger()}},
	}

	// A local namespace is queried the same way. It is registered by key: the
	// answer only depends on which namespaces this process owns, not on the
	// engine pointer, and a local namespace has no upstream at all.
	m.local["alice"] = nil

	seedCachedManifest(t, cfg.NamespaceRoot("docker.io"), "library/alpine", "latest", dgst)
	seedCachedManifest(t, cfg.NamespaceRoot("alice"), "notes", "v1", dgst)

	cases := []struct {
		name      string
		namespace string
		repo      string
		ref       string
		want      bool
	}{
		{"cached tag", "docker.io", "library/alpine", "latest", true},
		{"cached digest", "docker.io", "library/alpine", dgst, true},
		{"same tag in another repository", "docker.io", "myorg/alpine", "latest", false},
		{"another tag", "docker.io", "library/alpine", "v3.20", false},
		{"another repository", "docker.io", "library/busybox", "latest", false},
		{"cached tag in a local namespace", "alice", "notes", "v1", true},
		{"unknown namespace", "nope", "library/alpine", "latest", false},
		{"no reference", "docker.io", "library/alpine", "", false},
		{"no repository", "docker.io", "", "latest", false},
		{"unparsable reference is a tag", "docker.io", "library/alpine", "not a ref", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := m.CachedManifest(tc.namespace, tc.repo, tc.ref); got != tc.want {
				t.Errorf("CachedManifest(%q, %q, %q) = %v, want %v",
					tc.namespace, tc.repo, tc.ref, got, tc.want)
			}
		})
	}
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newV2Server answers /v2/ like a reachable registry.
func newV2Server(t *testing.T) *httptest.Server {
	t.Helper()

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s.Close)
	return s
}

// TestSelectionAccessors covers the three methods the selection chooser uses to
// see this process's namespaces: which can serve, which upstream to probe, and
// with which credentials.
func TestSelectionAccessors(t *testing.T) {
	live := newV2Server(t)
	log := silentLogger()

	cfg := &config.Config{
		Namespaces: config.Namespaces{
			Proxy: map[string]config.ProxyNamespace{
				"live":        {Upstreams: []string{live.URL}, Username: "robot", Password: "s3cret"},
				"cache-only":  {Upstreams: []string{live.URL}},
				"unavailable": {Upstreams: []string{live.URL}},
			},
		},
	}

	sel, err := upstream.New("live", []string{live.URL}, live.Client(), upstream.Options{Logger: log})
	if err != nil {
		t.Fatalf("upstream.New: %v", err)
	}
	if _, err := sel.ElectForStartup(context.Background()); err != nil {
		t.Fatalf("ElectForStartup: %v", err)
	}

	m := &Manager{
		cfg: cfg,
		log: log,
		proxy: map[string]*proxyNamespace{
			"live":        {name: "live", selector: sel, log: log},
			"cache-only":  {name: "cache-only", log: log},
			"unavailable": {name: "unavailable", log: log},
		},
	}
	m.proxy["live"].state.Store(stateProxy)
	m.proxy["cache-only"].state.Store(stateCacheOnly)
	m.proxy["unavailable"].state.Store(stateUnavailable)

	t.Run("Serving", func(t *testing.T) {
		got := m.Serving()
		want := []string{"cache-only", "live"}
		if len(got) != len(want) {
			t.Fatalf("Serving() = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("Serving() = %v, want %v (sorted)", got, want)
			}
		}
	})

	t.Run("ElectedUpstream", func(t *testing.T) {
		url, ok := m.ElectedUpstream("live")
		if !ok || url != live.URL {
			t.Errorf("ElectedUpstream(live) = (%q, %v), want (%q, true)", url, ok, live.URL)
		}
		if url, ok := m.ElectedUpstream("cache-only"); ok || url != "" {
			t.Errorf("ElectedUpstream(cache-only) = (%q, %v), want no upstream", url, ok)
		}
		if url, ok := m.ElectedUpstream("unavailable"); ok || url != "" {
			t.Errorf("ElectedUpstream(unavailable) = (%q, %v), want no upstream", url, ok)
		}
		if url, ok := m.ElectedUpstream("nope"); ok || url != "" {
			t.Errorf("ElectedUpstream(unknown) = (%q, %v), want no upstream", url, ok)
		}
	})

	t.Run("UpstreamCredentials", func(t *testing.T) {
		user, pass := m.UpstreamCredentials("live")
		if user != "robot" || pass != "s3cret" {
			t.Errorf("UpstreamCredentials(live) = (%q, %q), want the configured pair", user, pass)
		}
		if user, pass := m.UpstreamCredentials("cache-only"); user != "" || pass != "" {
			t.Errorf("UpstreamCredentials(cache-only) = (%q, %q), want empty", user, pass)
		}
		if user, pass := m.UpstreamCredentials("nope"); user != "" || pass != "" {
			t.Errorf("UpstreamCredentials(unknown) = (%q, %q), want empty", user, pass)
		}
	})

	t.Run("UpstreamLatency", func(t *testing.T) {
		d, ok := m.UpstreamLatency("live")
		if !ok || d <= 0 {
			t.Errorf("UpstreamLatency(live) = (%v, %v), want the measured startup probe", d, ok)
		}
		if d, ok := m.UpstreamLatency("cache-only"); ok || d != 0 {
			t.Errorf("UpstreamLatency(cache-only) = (%v, %v), want no figure", d, ok)
		}
		if d, ok := m.UpstreamLatency("nope"); ok || d != 0 {
			t.Errorf("UpstreamLatency(unknown) = (%v, %v), want no figure", d, ok)
		}
		t.Logf("startup health probe latency (EWMA seed): %v", d)
	})
}

// TestElectedUpstreamKeepsAnUnhealthyElection documents a deliberate choice: a
// content probe is how the chooser finds out whether an endpoint works, so the
// URL is reported even while the selector holds it as unhealthy. Otherwise a
// namespace whose only endpoint is failing could never be probed again.
func TestElectedUpstreamKeepsAnUnhealthyElection(t *testing.T) {
	live := newV2Server(t)
	log := silentLogger()

	sel, err := upstream.New("live", []string{live.URL}, live.Client(), upstream.Options{
		Logger:           log,
		FailThreshold:    1,
		SuccessThreshold: 1,
	})
	if err != nil {
		t.Fatalf("upstream.New: %v", err)
	}

	m := &Manager{
		cfg:   &config.Config{Namespaces: config.Namespaces{Proxy: map[string]config.ProxyNamespace{"live": {Upstreams: []string{live.URL}}}}},
		log:   log,
		proxy: map[string]*proxyNamespace{"live": {name: "live", selector: sel, log: log}},
	}
	m.proxy["live"].state.Store(stateProxy)

	if url, ok := m.ElectedUpstream("live"); ok || url != "" {
		t.Errorf("before any election ElectedUpstream = (%q, %v), want no upstream", url, ok)
	}

	if _, err := sel.ElectForStartup(context.Background()); err != nil {
		t.Fatalf("ElectForStartup: %v", err)
	}
	if url, ok := m.ElectedUpstream("live"); !ok || url != live.URL {
		t.Errorf("ElectedUpstream = (%q, %v), want (%q, true)", url, ok, live.URL)
	}

	// The upstream goes away: the health probe now fails enough times to latch
	// the endpoint unhealthy, but the election is kept.
	live.Close()
	if err := sel.ProbeAll(context.Background()); err == nil {
		t.Fatal("ProbeAll against a closed upstream should fail")
	}
	if _, healthy := sel.ElectedForRequest(); healthy {
		t.Fatal("the endpoint should be unhealthy after a failed probe")
	}

	if url, ok := m.ElectedUpstream("live"); !ok || url != live.URL {
		t.Errorf("ElectedUpstream while the endpoint is unhealthy = (%q, %v), want (%q, true): a content probe is what re-establishes it", url, ok, live.URL)
	}
}

// --- racing: per-upstream candidates and per-upstream engines --------------

// TestUpstreamsForAndObserveContentProbe covers the two methods the chooser uses
// to run the upstream race: the candidate list, and the verdict that takes a
// mirror out of it when it answers /v2/ but not content.
func TestUpstreamsForAndObserveContentProbe(t *testing.T) {
	live := newV2Server(t)
	log := silentLogger()

	sel, err := upstream.New("docker.io", []string{"https://first", live.URL}, live.Client(), upstream.Options{
		Interval:      time.Minute,
		FailThreshold: 2,
		Logger:        log,
	})
	if err != nil {
		t.Fatalf("upstream.New: %v", err)
	}

	m := &Manager{
		cfg:   &config.Config{},
		log:   log,
		proxy: map[string]*proxyNamespace{"docker.io": {name: "docker.io", selector: sel, log: log}},
	}

	got := m.UpstreamsFor("docker.io")
	if len(got) != 2 || got[0] != "https://first" || got[1] != live.URL {
		t.Fatalf("UpstreamsFor = %v, want both endpoints in configuration order", got)
	}
	if got := m.UpstreamsFor("nope"); got != nil {
		t.Errorf("UpstreamsFor(unknown) = %v, want nil", got)
	}

	// Two unanswered content probes: the endpoint is out of the race even
	// though its API root answers.
	m.ObserveContentProbe("docker.io", live.URL, false, 0, context.DeadlineExceeded)
	m.ObserveContentProbe("docker.io", live.URL, false, 0, context.DeadlineExceeded)

	got = m.UpstreamsFor("docker.io")
	if len(got) != 1 || got[0] != "https://first" {
		t.Fatalf("UpstreamsFor after two unanswered content probes = %v, want only the other endpoint", got)
	}
	if bad := m.ContentBadUpstreams("docker.io"); len(bad) != 1 || bad[0] != live.URL {
		t.Fatalf("ContentBadUpstreams = %v, want [%s]", bad, live.URL)
	}

	// Answering again puts it straight back.
	m.ObserveContentProbe("docker.io", live.URL, true, time.Millisecond, nil)
	if got := m.UpstreamsFor("docker.io"); len(got) != 2 {
		t.Fatalf("UpstreamsFor after recovery = %v, want both endpoints", got)
	}
}

// TestEngineBuildsOneEnginePerUpstream is the reason racing can work at all:
// distribution accepts one RemoteURL per engine, so a namespace served by three
// mirrors needs three engines, built lazily and kept apart.
func TestEngineBuildsOneEnginePerUpstream(t *testing.T) {
	first := newV2Server(t)
	second := newV2Server(t)
	log := silentLogger()

	cfg := &config.Config{
		Storage: config.Storage{Root: t.TempDir()},
		Namespaces: config.Namespaces{
			Proxy: map[string]config.ProxyNamespace{
				"docker.io": {Upstreams: []string{first.URL, second.URL}},
			},
		},
	}
	m := New(cfg, nil, log, nil, "test-secret", map[string]string{"docker.io": ""}, nil)
	p, err := m.newProxyNamespace("docker.io")
	if err != nil {
		t.Fatalf("newProxyNamespace: %v", err)
	}
	m.proxy["docker.io"] = p

	a, ok := m.Engine("docker.io", first.URL)
	if !ok {
		t.Fatal("Engine(first) reported no engine")
	}
	again, ok := m.Engine("docker.io", first.URL)
	if !ok || again != a {
		t.Fatalf("Engine(first) returned a different engine the second time: %v vs %v", again, a)
	}
	b, ok := m.Engine("docker.io", second.URL)
	if !ok {
		t.Fatal("Engine(second) reported no engine")
	}
	if b == a {
		t.Error("both upstreams were served by the same engine: each needs its own RemoteURL")
	}

	// An unknown upstream URL has no engine of its own; the sticky one serves
	// it rather than the namespace going dark.
	if _, ok := m.Engine("docker.io", "https://not-a-configured-upstream.invalid"); !ok {
		t.Error("Engine(unknown upstream) reported no engine, want the sticky fallback")
	}
	if _, ok := m.Engine("docker.io", ""); !ok {
		t.Error("Engine(sticky) reported no engine")
	}

	if err := m.Shutdown(); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}
