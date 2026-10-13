//go:build e2e

// Package e2e drives a real rms process against a real (in-process) upstream
// registry over HTTP, asserting the behaviours that matter: the docker login
// contract, pull-through caching, namespace selection for an unqualified
// request, the push flow including the Location round-trip, and the
// authorisation matrix.
//
// It deliberately needs no internet access and no docker daemon, so it is
// deterministic. For the client-driven variant set RMS_E2E_DOCKER=1.
package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/distribution/distribution/v3/configuration"
	"github.com/distribution/distribution/v3/registry/handlers"

	_ "github.com/distribution/distribution/v3/registry/storage/driver/filesystem"

	"github.com/watsons/rms/internal/app"
	"github.com/watsons/rms/internal/auth"
	"github.com/watsons/rms/internal/config"
	"github.com/watsons/rms/internal/server"
)

const (
	testUser     = "alice"
	testPassword = "alicepw"
	adminUser    = "admin"
	adminPass    = "adminpw"
	proxyNS      = "docker.io"
	// secondNS is the second proxy namespace every fixture configures. An
	// unqualified request has to choose between it and proxyNS, which is what
	// makes the selection tests meaningful.
	secondNS = "ghcr.io"
)

// ---------------------------------------------------------------------------
// fixture image
// ---------------------------------------------------------------------------

// fixture is a minimal but valid OCI image pushed into the fake upstream.
type fixture struct {
	configDigest string
	configBody   []byte
	layerDigest  string
	layerBody    []byte
	manifest     []byte
	manifestDgst string
}

func newFixture(t *testing.T) fixture {
	t.Helper()

	// The layer must be a real gzip of a real tar stream, and the config's
	// diff_ids must agree with it: a client such as `docker pull` verifies both,
	// and rejects a synthetic fixture with "number of layers and diffIDs don't
	// match".
	layer, diffID := buildLayer(t)

	config, err := json.Marshal(map[string]any{
		"architecture": "amd64",
		"os":           "linux",
		"rootfs": map[string]any{
			"type":     "layers",
			"diff_ids": []string{diffID},
		},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	f := fixture{
		configBody:   config,
		layerBody:    layer,
		configDigest: digestOf(config),
		layerDigest:  digestOf(layer),
	}

	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    f.configDigest,
			"size":      len(config),
		},
		"layers": []map[string]any{{
			"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
			"digest":    f.layerDigest,
			"size":      len(layer),
		}},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	f.manifest = body
	f.manifestDgst = digestOf(body)

	return f
}

// buildLayer produces a gzipped tar layer plus its uncompressed diff ID, which
// is what the image config must reference.
func buildLayer(t *testing.T) (layer []byte, diffID string) {
	t.Helper()

	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	content := []byte("rms end-to-end fixture\n")
	if err := tw.WriteHeader(&tar.Header{
		Name: "hello.txt",
		Mode: 0o644,
		Size: int64(len(content)),
	}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}

	var gzBuf bytes.Buffer
	zw := gzip.NewWriter(&gzBuf)
	if _, err := zw.Write(tarBuf.Bytes()); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	return gzBuf.Bytes(), digestOf(tarBuf.Bytes())
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// fake upstream registry
// ---------------------------------------------------------------------------

// fakeUpstream is a real distribution registry in local mode, fronted by a
// counter so tests can prove that cached content is no longer fetched.
type fakeUpstream struct {
	srv  *httptest.Server
	app  *handlers.App
	hits atomic.Int64
	url  string
	fixt fixture
	repo string
	tag  string

	// delay slows every response, which is how a racing test makes one mirror
	// of a namespace measurably slower than another.
	delay time.Duration
	mu    sync.Mutex
	// paths records what this upstream was asked for, so a test can tell a
	// probe (a manifest HEAD) from the fetches of a served pull.
	paths []string
}

func startFakeUpstream(t *testing.T, fx fixture, repo, tag string) *fakeUpstream {
	t.Helper()

	cfg := &configuration.Configuration{}
	cfg.Log.Level = "error"
	cfg.Storage = configuration.Storage{
		"filesystem": configuration.Parameters{"rootdirectory": t.TempDir()},
		"redirect":   configuration.Parameters{"disable": true},
		"delete":     configuration.Parameters{"enabled": true},
		"maintenance": configuration.Parameters{
			"uploadpurging": map[any]any{"enabled": false},
		},
	}
	cfg.HTTP.Secret = "e2e-upstream-secret"

	up := &fakeUpstream{fixt: fx, repo: repo, tag: tag}
	up.app = handlers.NewApp(context.Background(), cfg)

	up.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.hits.Add(1)
		up.mu.Lock()
		up.paths = append(up.paths, r.Method+" "+r.URL.Path)
		delay := up.delay
		up.mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		up.app.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { up.srv.Close(); _ = up.app.Shutdown() })

	up.url = up.srv.URL
	up.Seed(t)
	return up
}

// Seed pushes the fixture image into the upstream.
func (up *fakeUpstream) Seed(t *testing.T) {
	t.Helper()

	for _, blob := range []struct {
		digest string
		body   []byte
	}{
		{up.fixt.configDigest, up.fixt.configBody},
		{up.fixt.layerDigest, up.fixt.layerBody},
	} {
		up.startUpload(t, blob.digest, blob.body)
	}

	req, err := http.NewRequest(http.MethodPut,
		fmt.Sprintf("%s/v2/%s/manifests/%s", up.url, up.repo, up.tag), bytes.NewReader(up.fixt.manifest))
	if err != nil {
		t.Fatalf("new manifest request: %v", err)
	}
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put manifest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("put manifest: got %d, want 201 (%s)", resp.StatusCode, readBody(resp))
	}
}

// startUpload performs the monolithic blob upload dance.
func (up *fakeUpstream) startUpload(t *testing.T, digest string, body []byte) {
	t.Helper()

	post, err := http.Post(fmt.Sprintf("%s/v2/%s/blobs/uploads/", up.url, up.repo), "", nil)
	if err != nil {
		t.Fatalf("start upload: %v", err)
	}
	defer post.Body.Close()
	if post.StatusCode != http.StatusAccepted {
		t.Fatalf("start upload: got %d (%s)", post.StatusCode, readBody(post))
	}
	loc := post.Header.Get("Location")
	if loc == "" {
		t.Fatal("start upload: no Location")
	}

	put, err := http.NewRequest(http.MethodPut, withDigest(loc, digest), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new blob put: %v", err)
	}
	put.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Fatalf("put blob: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("put blob: got %d, want 201 (%s)", resp.StatusCode, readBody(resp))
	}
}

func withDigest(loc, digest string) string {
	sep := "?"
	if strings.Contains(loc, "?") {
		sep = "&"
	}
	return loc + sep + "digest=" + digest
}

// ---------------------------------------------------------------------------
// rms under test
// ---------------------------------------------------------------------------

type rmsInstance struct {
	primary string // http://127.0.0.1:PORT — the only content endpoint
	dataDir string
}

// rmsOptions describes one rms under test. There is deliberately no
// namespace-bound listener any more: everything is served by the single
// multi-namespace endpoint, and an unqualified request picks its namespace.
type rmsOptions struct {
	// dockerUpstream backs the proxyNS namespace. proxyNS keeps
	// official_library, so a single-segment repository means library/<name>
	// there and only there.
	dockerUpstream string
	// dockerUpstreams, when set, replaces dockerUpstream with a list of mirrors
	// for the proxyNS namespace: the shape a namespace with several upstreams
	// has, which is what the upstream race exists for.
	dockerUpstreams []string
	// secondNSUpstream backs the secondNS namespace, which has no
	// official_library rule. It is what an unqualified request has to be
	// chosen *over* when only proxyNS has the repository.
	secondNSUpstream string
	// dataDir reuses existing storage instead of a fresh temp dir.
	dataDir string
	// probeTimeout bounds one selection probe. Kept short so a test that
	// deliberately points at a dead port does not wait 10s.
	probeTimeout string
	// healthEnabled must be false when the upstream is deliberately dead at
	// startup, otherwise the namespace never builds an engine.
	healthEnabled bool
}

// upstreamList renders a namespace's upstreams as a YAML flow sequence: the
// single default, or the list a racing test configured.
func upstreamList(single string, list []string) string {
	if len(list) == 0 {
		list = []string{single}
	}
	quoted := make([]string, 0, len(list))
	for _, u := range list {
		quoted = append(quoted, fmt.Sprintf("%q", u))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func startRMS(t *testing.T, upstreamURL string) *rmsInstance {
	t.Helper()
	// Both namespaces point at the same upstream by default, so a test that
	// only cares about proxyNS behaves as before. Selection tests use
	// startRMSOpts with two different upstreams.
	return startRMSOpts(t, rmsOptions{
		dockerUpstream:   upstreamURL,
		secondNSUpstream: upstreamURL,
		healthEnabled:    true,
	})
}

func startRMSOpts(t *testing.T, o rmsOptions) *rmsInstance {
	t.Helper()

	if o.probeTimeout == "" {
		o.probeTimeout = "2s"
	}

	// Hermetic: no outbound proxy, so the loopback upstream is reached directly.
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")

	dir := t.TempDir()
	htpasswdFile := filepath.Join(dir, "htpasswd")
	var sb strings.Builder
	for _, u := range []struct{ user, pass string }{
		{testUser, testPassword}, {adminUser, adminPass}, {"bob", "bobpw"},
	} {
		hash, err := auth.HashPassword(u.pass)
		if err != nil {
			t.Fatalf("hash %s: %v", u.user, err)
		}
		fmt.Fprintf(&sb, "%s:%s\n", u.user, hash)
	}
	if err := os.WriteFile(htpasswdFile, []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("write htpasswd: %v", err)
	}

	dataDir := o.dataDir
	if dataDir == "" {
		dataDir = filepath.Join(dir, "data")
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			t.Fatalf("mkdir data: %v", err)
		}
	}

	// Both ports are reserved in one call, while both listeners are still open,
	// so rms can never be configured with two identical listeners. See
	// reservePorts.
	ports := reservePorts(t, 2)
	primaryPort, metricsPort := ports[0], ports[1]

	cfgBody := fmt.Sprintf(`
server:
  listen: "127.0.0.1:%d"
  http_secret: "e2e-secret"
storage:
  root: %q
auth:
  type: htpasswd
  htpasswd: %q
  realm: rms
  user_namespace: true
  shared_namespaces: [me]
  anonymous_pull_proxy: true
  anonymous_pull_local: false
  admin_users: [%s]
namespaces:
  selection:
    probe_ttl: 30s
    probe_timeout: %s
  proxy:
    %s:
      upstreams: %s
      official_library: true
      ttl: 0
    %s:
      upstreams: [%q]
      ttl: 0
upstream:
  health:
    enabled: %t
    interval: 5s
    timeout: 5s
  http:
    proxy: ""
    no_proxy: "127.0.0.1,localhost"
observability:
  logs:
    level: error
    format: json
  traces:
    enabled: false
  metrics:
    listen: "127.0.0.1:%d"
`, primaryPort, dataDir, htpasswdFile, adminUser, o.probeTimeout,
		proxyNS, upstreamList(o.dockerUpstream, o.dockerUpstreams), secondNS, o.secondNSUpstream, o.healthEnabled, metricsPort)

	cfgPath := filepath.Join(dir, "rms.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	srv, err := app.Build(context.Background(), cfg, nil, server.Observability{})
	if err != nil {
		t.Fatalf("build app: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Log("rms did not stop in time")
		}
	})

	inst := &rmsInstance{
		primary: fmt.Sprintf("http://127.0.0.1:%d", primaryPort),
		dataDir: dataDir,
	}
	waitReady(t, inst.primary+"/v2/", primaryPort)
	return inst
}

// issuedPorts remembers every loopback port this test process has handed out.
//
// net.Listen("tcp", "127.0.0.1:0") on its own is not a port allocator: the
// listener is closed before the caller binds, and the kernel is free to hand
// that just-released port straight back to the next call. Two consecutive calls
// therefore collide occasionally — measured at 3 pairs in 20 000 in this
// process with nothing else running, and more often during a full run, where
// the fake upstreams and the rms instances churn ephemeral ports alongside it.
// The symptom is a config that fails validation with
// "observability.metrics.listen: must differ from server.listen", i.e. a
// randomly red e2e run that has nothing to do with the code under test.
//
// A port stays in this set for the life of the process, so neither reservePorts
// nor a deliberately-dead port can be handed out twice.
var (
	issuedMu    sync.Mutex
	issuedPorts = map[int]bool{}
)

// reservePorts returns n distinct loopback ports that nothing is listening on.
//
// It holds every listener open until the whole set has been chosen, because a
// port cannot be handed to a second reservation while the first still holds it.
// That is what makes the result collision-free rather than merely
// usually-collision-free: the kernel only reuses a port once every listener on
// it is closed, and by then this call is done choosing.
//
// The listeners are closed before returning, so the caller can bind them; that
// last window is the unavoidable TOCTOU of address-based reservation, and it is
// orders of magnitude smaller than the one this replaces.
func reservePorts(t *testing.T, n int) []int {
	t.Helper()

	issuedMu.Lock()
	defer issuedMu.Unlock()

	var (
		open  []net.Listener
		ports []int
	)
	// Safety net: if this function fails partway (t.Fatalf below), nothing may
	// be left listening on a port a later test wants.
	t.Cleanup(func() {
		for _, ln := range open {
			_ = ln.Close()
		}
	})

	for len(ports) < n {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserve port: %v", err)
		}
		open = append(open, ln) // stays open: the kernel cannot hand it out again
		port := ln.Addr().(*net.TCPAddr).Port
		if issuedPorts[port] {
			continue // already given to an earlier test in this process
		}
		issuedPorts[port] = true
		ports = append(ports, port)
	}

	for _, ln := range open {
		_ = ln.Close()
	}
	open = nil
	return ports
}

// deadPort returns a loopback port that nothing is listening on and that no
// later reservation in this process will reuse.
func deadPort(t *testing.T) int {
	t.Helper()
	return reservePorts(t, 1)[0]
}

// TestReservePortsNeverRepeat pins the property the flake violated: no port is
// ever handed out twice in this process, and nothing is left listening on a
// port that was handed out.
//
// It is deterministic on purpose. The bug it guards against showed up in about
// one full run in a few hundred, so running the suite a handful of times is not
// evidence that it is gone; asserting the property directly is.
func TestReservePortsNeverRepeat(t *testing.T) {
	const total = 200

	seen := map[int]bool{}
	// Two small reservations of the size the harness makes, then one large one:
	// the harness asks for its pair repeatedly, so a repeat would show up first
	// across separate calls rather than within a single batch.
	for i := 0; i < 20; i++ {
		for _, p := range reservePorts(t, 2) {
			if seen[p] {
				t.Fatalf("port %d was handed out twice", p)
			}
			seen[p] = true
		}
	}
	for _, p := range reservePorts(t, total-40) {
		if seen[p] {
			t.Fatalf("port %d was handed out twice", p)
		}
		seen[p] = true
	}
	if len(seen) != total {
		t.Fatalf("reserved %d distinct ports, want %d", len(seen), total)
	}

	// And every one of them must be free: nothing may still be listening, or a
	// test that binds its reservation would fail with "address already in use".
	for p := range seen {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			t.Fatalf("reserved port %d is not bindable: %v", p, err)
		}
		_ = ln.Close()
	}
}

// waitReady polls the given port until rms answers, so tests never race startup.
func waitReady(t *testing.T, url string, port int) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("rms on port %d did not become reachable", port)
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

type client struct {
	t      *testing.T
	user   string
	pass   string
	client *http.Client
}

func newClient(t *testing.T) *client {
	return &client{t: t, client: &http.Client{Timeout: 30 * time.Second}}
}

func (c *client) as(user, pass string) *client {
	return &client{t: c.t, user: user, pass: pass, client: c.client}
}

func (c *client) do(method, url string, body []byte, hdr map[string]string) *http.Response {
	c.t.Helper()

	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		c.t.Fatalf("new %s %s: %v", method, url, err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, url, err)
	}
	c.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func readBody(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return string(b)
}

func requireStatus(t *testing.T, resp *http.Response, want int, what string) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("%s: got %d, want %d (%s)", what, resp.StatusCode, want, readBody(resp))
	}
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

// TestLoginContract pins the docker login contract: the API root challenges, and
// a bad password is rejected there.
func TestLoginContract(t *testing.T) {
	fx := newFixture(t)
	up := startFakeUpstream(t, fx, "library/demo", "v1")
	inst := startRMS(t, up.url)

	anon := newClient(t)
	resp := anon.do(http.MethodGet, inst.primary+"/v2/", nil, nil)
	requireStatus(t, resp, http.StatusUnauthorized, "anonymous API root")
	if got := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic ") {
		t.Errorf("WWW-Authenticate = %q, want a Basic challenge", got)
	}

	good := anon.as(testUser, testPassword)
	requireStatus(t, good.do(http.MethodGet, inst.primary+"/v2/", nil, nil), http.StatusOK, "valid credentials")

	bad := anon.as(testUser, "wrong")
	requireStatus(t, bad.do(http.MethodGet, inst.primary+"/v2/", nil, nil), http.StatusUnauthorized, "invalid credentials")
}

// TestPullThroughAndCache covers the three ways a proxied image is addressed and
// proves the payload is served locally on the second read.
func TestPullThroughAndCache(t *testing.T) {
	fx := newFixture(t)
	up := startFakeUpstream(t, fx, "library/demo", "v1")
	inst := startRMS(t, up.url)

	anon := newClient(t)
	manifestAccept := map[string]string{"Accept": "application/vnd.oci.image.manifest.v1+json"}

	t.Run("explicit namespace", func(t *testing.T) {
		resp := anon.do(http.MethodGet, inst.primary+"/v2/"+proxyNS+"/library/demo/manifests/v1", nil, manifestAccept)
		requireStatus(t, resp, http.StatusOK, "explicit namespace manifest")
		if body := readBody(resp); body != string(fx.manifest) {
			t.Errorf("manifest body differs from the upstream fixture")
		}
	})

	t.Run("unqualified request is resolved by selection", func(t *testing.T) {
		resp := anon.do(http.MethodGet, inst.primary+"/v2/library/demo/manifests/v1", nil, manifestAccept)
		requireStatus(t, resp, http.StatusOK, "unqualified two-segment manifest")
		if body := readBody(resp); body != string(fx.manifest) {
			t.Errorf("unqualified manifest body differs from the upstream fixture")
		}
	})

	t.Run("single segment becomes a library image", func(t *testing.T) {
		// The upstream only has library/demo, and only the proxyNS namespace
		// has official_library, so this proves the library/ injection happens
		// per candidate and is what makes proxyNS win.
		resp := anon.do(http.MethodGet, inst.primary+"/v2/demo/manifests/v1", nil, manifestAccept)
		requireStatus(t, resp, http.StatusOK, "single-segment manifest with library/ injection")
	})

	t.Run("blob is served and then cached", func(t *testing.T) {
		blobURL := inst.primary + "/v2/" + proxyNS + "/library/demo/blobs/" + fx.layerDigest

		before := up.hits.Load()
		resp := anon.do(http.MethodGet, blobURL, nil, nil)
		requireStatus(t, resp, http.StatusOK, "first blob read")
		if body := readBody(resp); body != string(fx.layerBody) {
			t.Fatalf("blob body differs from the upstream fixture")
		}
		firstHits := up.hits.Load()
		if firstHits <= before {
			t.Fatalf("first blob read did not reach the upstream (%d -> %d)", before, firstHits)
		}

		// Second read must be served from local storage: no new upstream traffic.
		before = up.hits.Load()
		resp = anon.do(http.MethodGet, blobURL, nil, nil)
		requireStatus(t, resp, http.StatusOK, "cached blob read")
		if got := readBody(resp); got != string(fx.layerBody) {
			t.Fatalf("cached blob body differs")
		}
		if after := up.hits.Load(); after != before {
			t.Errorf("cached blob read still hit the upstream (%d -> %d)", before, after)
		}
	})

}

// TestUnqualifiedPicksNamespaceWithContent is the selection contract: an
// unqualified request must be served by the namespace that actually has the
// repository, decided per request rather than by a fixed default. Two fake
// upstreams carry different content, so "which one answered" is observable both
// in the response body and in the per-upstream hit counters.
//
// It is deliberately a *cold cache* test: nothing is pulled before the
// unqualified request, so the selection probe has to answer for real instead of
// short-circuiting on content already on local disk.
//
// PRECONDITION on the code under test: the selection probe must send the
// manifest Accept media types on its HEAD (and on the GET it degrades to).
// Without them a registry that stores OCI manifests rejects the probe with
// "OCI manifest found, but accept header does not support OCI manifests" —
// ghcr.io answers 404 for an image that exists — and the two subtests below
// fail with a NAME_UNKNOWN that is not the client's fault. That is a defect in
// internal/selection/probe.go, not in this test: the repository really is
// there. Measured readings and the reproduction are in
// reports/raw/config-docs-notes.md §5 and reports/raw/selection-real-upstream.txt §8.
func TestUnqualifiedPicksNamespaceWithContent(t *testing.T) {
	fx := newFixture(t)
	dockerUp := startFakeUpstream(t, fx, "library/demo", "v1")
	// The other namespace has content of its own, but not the repository under
	// test — so a fixed default could not fake a pass here.
	otherUp := startFakeUpstream(t, fx, "owner/other", "v1")

	inst := startRMSOpts(t, rmsOptions{
		dockerUpstream:   dockerUp.url,
		secondNSUpstream: otherUp.url,
		healthEnabled:    true,
	})

	anon := newClient(t)
	manifestAccept := map[string]string{"Accept": "application/vnd.oci.image.manifest.v1+json"}

	t.Run("only one namespace has it, so it wins", func(t *testing.T) {
		beforeDocker, beforeOther := dockerUp.hits.Load(), otherUp.hits.Load()

		resp := anon.do(http.MethodGet, inst.primary+"/v2/library/demo/manifests/v1", nil, manifestAccept)
		requireStatus(t, resp, http.StatusOK, "unqualified manifest")
		if body := readBody(resp); body != string(fx.manifest) {
			t.Errorf("served manifest is not the fixture")
		}

		// Both candidates must have been probed: the decision is a real
		// concurrent probe, not a first-match lookup.
		if dockerUp.hits.Load() <= beforeDocker {
			t.Errorf("namespace %s was never probed", proxyNS)
		}
		if otherUp.hits.Load() <= beforeOther {
			t.Errorf("namespace %s was never probed", secondNS)
		}
	})

	t.Run("the other namespace wins for its own repository", func(t *testing.T) {
		// Same configuration, different repository: owner/other exists only in
		// the second namespace, so the winner must flip.
		resp := anon.do(http.MethodGet, inst.primary+"/v2/owner/other/manifests/v1", nil, manifestAccept)
		requireStatus(t, resp, http.StatusOK, "unqualified manifest for the second namespace")
		if body := readBody(resp); body != string(fx.manifest) {
			t.Errorf("served manifest is not the fixture")
		}
	})

	t.Run("explicit namespace prefix stays inside its namespace", func(t *testing.T) {
		// A path that names a namespace must never consult the selection: the
		// other namespace's upstream must see no traffic at all. (The named
		// namespace's own upstreams are still raced among themselves.)
		beforeOther := otherUp.hits.Load()
		resp := anon.do(http.MethodGet, inst.primary+"/v2/"+proxyNS+"/library/demo/manifests/v1", nil, manifestAccept)
		requireStatus(t, resp, http.StatusOK, "explicit namespaced manifest")
		if got := otherUp.hits.Load(); got != beforeOther {
			t.Errorf("explicit /v2/%s/ request probed %s anyway (%d -> %d)", proxyNS, secondNS, beforeOther, got)
		}

		beforeDocker := dockerUp.hits.Load()
		resp = anon.do(http.MethodGet, inst.primary+"/v2/"+secondNS+"/owner/other/manifests/v1", nil, manifestAccept)
		requireStatus(t, resp, http.StatusOK, "explicit manifest for the second namespace")
		if got := dockerUp.hits.Load(); got != beforeDocker {
			t.Errorf("explicit /v2/%s/ request probed %s anyway (%d -> %d)", secondNS, proxyNS, beforeDocker, got)
		}
	})

	t.Run("a missing tag is NAME_UNKNOWN, not UNAVAILABLE", func(t *testing.T) {
		// Every candidate answers and none has it: that is a definitive "no",
		// which the client may cache, so it must be a 404.
		resp := anon.do(http.MethodGet, inst.primary+"/v2/library/demo/manifests/nope", nil, manifestAccept)
		requireStatus(t, resp, http.StatusNotFound, "unqualified missing tag")
		if code := firstErrorCode(t, resp); code != "NAME_UNKNOWN" {
			t.Errorf("error code = %q, want NAME_UNKNOWN", code)
		}
	})
}

// TestUnqualifiedAllUnreachable is the other side of that boundary: when no
// candidate could be probed at all, the answer is not knowable, so rms must say
// 503 rather than claim the image does not exist.
func TestUnqualifiedAllUnreachable(t *testing.T) {
	// A port that was reserved and then released: nothing listens on it.
	dead := fmt.Sprintf("http://127.0.0.1:%d", deadPort(t))

	inst := startRMSOpts(t, rmsOptions{
		dockerUpstream:   dead,
		secondNSUpstream: dead,
		probeTimeout:     "2s",
	})

	anon := newClient(t)
	resp := anon.do(http.MethodGet, inst.primary+"/v2/library/demo/manifests/v1", nil,
		map[string]string{"Accept": "application/vnd.oci.image.manifest.v1+json"})
	requireStatus(t, resp, http.StatusServiceUnavailable, "unqualified request with no reachable upstream")
	if code := firstErrorCode(t, resp); code != "UNAVAILABLE" {
		t.Errorf("error code = %q, want UNAVAILABLE", code)
	}
}

// TestUnqualifiedBlobFollowsTheManifest covers the second half of an unqualified
// pull: a client that has just read /v2/<repo>/manifests/<tag> then asks for the
// layers and the config with NO repository in the path — /v2/blobs/<digest>.
//
// That path names no repository, so nothing in it can be probed and the request
// cannot be decided on its own merits. It has to follow the decision already
// made for the repository it belongs to (repository affinity). If it does not,
// a cold pull 404s on the first blob even though the manifest was served
// correctly — the manifest succeeds and the image still cannot be pulled.
func TestUnqualifiedBlobFollowsTheManifest(t *testing.T) {
	fx := newFixture(t)
	dockerUp := startFakeUpstream(t, fx, "library/demo", "v1")
	// Second namespace with content of its own: if the blob request were probed
	// instead of following affinity, this upstream would show traffic.
	otherUp := startFakeUpstream(t, fx, "owner/other", "v1")

	inst := startRMSOpts(t, rmsOptions{
		dockerUpstream:   dockerUp.url,
		secondNSUpstream: otherUp.url,
		healthEnabled:    true,
	})

	anon := newClient(t)
	manifestAccept := map[string]string{"Accept": "application/vnd.oci.image.manifest.v1+json"}

	// 1. The manifest establishes the affinity. Nothing is cached beforehand,
	// so this is the cold path that matters.
	resp := anon.do(http.MethodGet, inst.primary+"/v2/library/demo/manifests/v1", nil, manifestAccept)
	requireStatus(t, resp, http.StatusOK, "unqualified manifest")
	manifestBody := readBody(resp)
	if manifestBody != string(fx.manifest) {
		t.Fatalf("served manifest is not the fixture")
	}

	// Read the config digest off the wire rather than trusting the fixture
	// constant: the client is the one that decides which blob to ask for next.
	var manifestDoc struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := json.Unmarshal([]byte(manifestBody), &manifestDoc); err != nil {
		t.Fatalf("decode manifest %q: %v", manifestBody, err)
	}
	configDigest := manifestDoc.Config.Digest
	if configDigest != fx.configDigest {
		t.Fatalf("manifest config digest = %q, want %q", configDigest, fx.configDigest)
	}

	// 2. The config blob, addressed with no repository in the path.
	beforeDocker, beforeOther := dockerUp.hits.Load(), otherUp.hits.Load()

	blobResp := anon.do(http.MethodGet, inst.primary+"/v2/blobs/"+configDigest, nil, nil)
	requireStatus(t, blobResp, http.StatusOK, "unqualified blob after a manifest")
	if body := readBody(blobResp); body != string(fx.configBody) {
		t.Errorf("blob body differs from the fixture config")
	}

	// 3. It must have gone to the namespace that served the manifest — and only
	// there. A hit on the other upstream means the blob was probed or guessed.
	if got := otherUp.hits.Load(); got != beforeOther {
		t.Errorf("blob request reached namespace %s, which never served this repository (%d -> %d)",
			secondNS, beforeOther, got)
	}
	if got := dockerUp.hits.Load(); got <= beforeDocker {
		t.Errorf("blob request never reached namespace %s (%d -> %d)", proxyNS, beforeDocker, got)
	}
}

// TestUnqualifiedBlobWithUnknownDigestIsUnavailable pins the boundary for a blob
// that follows no manifest: with no repository to be affined to, rms does not
// know who owns the digest, so "not found" would be a claim it cannot back.
// The answer must be 503, exactly as it is when no candidate can be probed.
func TestUnqualifiedBlobWithUnknownDigestIsUnavailable(t *testing.T) {
	fx := newFixture(t)
	dockerUp := startFakeUpstream(t, fx, "library/demo", "v1")
	otherUp := startFakeUpstream(t, fx, "owner/other", "v1")

	inst := startRMSOpts(t, rmsOptions{
		dockerUpstream:   dockerUp.url,
		secondNSUpstream: otherUp.url,
		healthEnabled:    true,
		probeTimeout:     "2s",
	})

	// No manifest was read first: nothing is known about this digest. It is
	// also not present in either upstream.
	unknown := "sha256:" + strings.Repeat("0", 64)

	anon := newClient(t)
	resp := anon.do(http.MethodGet, inst.primary+"/v2/blobs/"+unknown, nil, nil)
	requireStatus(t, resp, http.StatusServiceUnavailable, "unqualified blob with an unknown digest")
	if code := firstErrorCode(t, resp); code != "UNAVAILABLE" {
		t.Errorf("error code = %q, want UNAVAILABLE", code)
	}
}

// firstErrorCode decodes the registry v2 error envelope a response carries.
func firstErrorCode(t *testing.T, resp *http.Response) string {
	t.Helper()

	var out struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	body := readBody(resp)
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode error envelope %q: %v", body, err)
	}
	if len(out.Errors) == 0 {
		t.Fatalf("no errors in envelope %q", body)
	}
	return out.Errors[0].Code
}

// TestPushFlow exercises the upload sequence and, crucially, that the Location
// the engine returns is usable by a client (the repository name carries the
// namespace for exactly this reason).
func TestPushFlow(t *testing.T) {
	fx := newFixture(t)
	up := startFakeUpstream(t, fx, "library/demo", "v1")
	inst := startRMS(t, up.url)

	alice := newClient(t).as(testUser, testPassword)

	// Seed the cache with the fixture so the push has blobs to mount.
	anon := newClient(t)
	requireStatus(t, anon.do(http.MethodGet, inst.primary+"/v2/"+proxyNS+"/library/demo/manifests/v1", nil,
		map[string]string{"Accept": "application/vnd.oci.image.manifest.v1+json"}), http.StatusOK, "seed manifest")

	const target = "alice/app"

	for _, blob := range []struct {
		digest string
		body   []byte
	}{
		{fx.configDigest, fx.configBody},
		{fx.layerDigest, fx.layerBody},
	} {
		// Start the upload and follow the returned Location verbatim.
		resp := alice.do(http.MethodPost, inst.primary+"/v2/"+target+"/blobs/uploads/", nil, nil)
		requireStatus(t, resp, http.StatusAccepted, "start upload")
		loc := resp.Header.Get("Location")
		if loc == "" {
			t.Fatal("start upload returned no Location")
		}
		if !strings.Contains(loc, "/v2/"+target+"/blobs/uploads/") {
			t.Fatalf("Location %q does not address %q; a client could not use it", loc, target)
		}

		// A relative Location must be resolved against the request; an absolute
		// one is used as-is.
		putURL := loc
		if strings.HasPrefix(loc, "/") {
			putURL = inst.primary + loc
		}

		put := alice.do(http.MethodPut, withDigest(putURL, blob.digest), blob.body,
			map[string]string{"Content-Type": "application/octet-stream"})
		requireStatus(t, put, http.StatusCreated, "finish upload")
	}

	put := alice.do(http.MethodPut, inst.primary+"/v2/"+target+"/manifests/v1", fx.manifest,
		map[string]string{"Content-Type": "application/vnd.oci.image.manifest.v1+json"})
	requireStatus(t, put, http.StatusCreated, "put manifest")

	got := alice.do(http.MethodGet, inst.primary+"/v2/"+target+"/manifests/v1", nil,
		map[string]string{"Accept": "application/vnd.oci.image.manifest.v1+json"})
	requireStatus(t, got, http.StatusOK, "read back pushed manifest")
	if body := readBody(got); body != string(fx.manifest) {
		t.Errorf("pushed manifest read back differs")
	}

	// The pushed blobs must be on disk under this namespace, not the proxy one.
	root := filepath.Join(inst.dataDir, testUser, "docker", "registry", "v2", "repositories",
		testUser, "app", "_layers")
	if _, err := os.Stat(root); err != nil {
		t.Errorf("expected pushed layers under %s: %v", root, err)
	}

	// And it must appear in the merged catalog under its namespaced name, which
	// is the name a client has to use.
	admin := anon.as(adminUser, adminPass)
	resp := admin.do(http.MethodGet, inst.primary+"/v2/_catalog", nil, nil)
	requireStatus(t, resp, http.StatusOK, "catalog after push")

	var out struct {
		Repositories []string `json:"repositories"`
	}
	if err := json.Unmarshal([]byte(readBody(resp)), &out); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	want := testUser + "/app"
	found := false
	for _, r := range out.Repositories {
		if r == want {
			found = true
		}
	}
	if !found {
		t.Errorf("catalog %v does not contain %q", out.Repositories, want)
	}
}

// TestAuthorisationMatrix covers who may read and write what.
func TestAuthorisationMatrix(t *testing.T) {
	fx := newFixture(t)
	up := startFakeUpstream(t, fx, "library/demo", "v1")
	inst := startRMS(t, up.url)

	anon := newClient(t)
	alice := anon.as(testUser, testPassword)
	bob := anon.as("bob", "bobpw")
	admin := anon.as(adminUser, adminPass)

	uploads := func(c *client, ns string) *http.Response {
		return c.do(http.MethodPost, inst.primary+"/v2/"+ns+"/app/blobs/uploads/", nil, nil)
	}

	t.Run("anonymous pull from a proxy namespace", func(t *testing.T) {
		resp := anon.do(http.MethodGet, inst.primary+"/v2/"+proxyNS+"/library/demo/manifests/v1", nil,
			map[string]string{"Accept": "application/vnd.oci.image.manifest.v1+json"})
		requireStatus(t, resp, http.StatusOK, "anonymous proxy pull")
	})

	t.Run("anonymous pull from a private namespace is challenged", func(t *testing.T) {
		requireStatus(t, anon.do(http.MethodGet, inst.primary+"/v2/"+testUser+"/app/manifests/v1", nil, nil),
			http.StatusUnauthorized, "anonymous private pull")
	})

	t.Run("anonymous push is challenged", func(t *testing.T) {
		requireStatus(t, uploads(anon, testUser), http.StatusUnauthorized, "anonymous push")
	})

	t.Run("push to the read-only proxy namespace is denied", func(t *testing.T) {
		resp := uploads(alice, proxyNS)
		requireStatus(t, resp, http.StatusForbidden, "push to proxy namespace")
		body := readBody(resp)
		if !strings.Contains(body, proxyNS) || !strings.Contains(body, testUser) {
			t.Errorf("denial should be actionable, got %s", body)
		}
	})

	t.Run("push to another user's namespace is denied", func(t *testing.T) {
		resp := uploads(alice, "bob")
		requireStatus(t, resp, http.StatusForbidden, "push to bob")
		if body := readBody(resp); !strings.Contains(body, "another user") {
			t.Errorf("denial should be actionable, got %s", body)
		}
	})

	t.Run("owner may push to their own namespace", func(t *testing.T) {
		requireStatus(t, uploads(alice, testUser), http.StatusAccepted, "owner push")
	})

	t.Run("any authenticated user may push to a shared namespace", func(t *testing.T) {
		requireStatus(t, uploads(bob, "me"), http.StatusAccepted, "shared namespace push")
	})

	t.Run("admin may push anywhere local", func(t *testing.T) {
		requireStatus(t, uploads(admin, "bob"), http.StatusAccepted, "admin push")
	})

	t.Run("catalog is admin-only", func(t *testing.T) {
		requireStatus(t, anon.do(http.MethodGet, inst.primary+"/v2/_catalog", nil, nil),
			http.StatusUnauthorized, "anonymous catalog")
		requireStatus(t, alice.do(http.MethodGet, inst.primary+"/v2/_catalog", nil, nil),
			http.StatusForbidden, "non-admin catalog")

		resp := admin.do(http.MethodGet, inst.primary+"/v2/_catalog", nil, nil)
		requireStatus(t, resp, http.StatusOK, "admin catalog")

		// The shape must be the registry v2 envelope. Content is asserted in
		// TestPushFlow, where a repository actually exists.
		var out struct {
			Repositories []string `json:"repositories"`
		}
		if err := json.Unmarshal([]byte(readBody(resp)), &out); err != nil {
			t.Fatalf("decode catalog: %v", err)
		}
	})

	t.Run("unknown paths are 404, not a namespace", func(t *testing.T) {
		requireStatus(t, anon.do(http.MethodGet, inst.primary+"/not-a-registry-path", nil, nil),
			http.StatusNotFound, "non-v2 path")
	})
}

// TestDegradedServesCache covers the outage path: with every upstream dead, an
// already-cached image is still pullable and writes are refused.
func TestDegradedServesCache(t *testing.T) {
	fx := newFixture(t)
	up := startFakeUpstream(t, fx, "library/demo", "v1")

	// Warm the cache first.
	inst := startRMS(t, up.url)
	anon := newClient(t)
	manifestAccept := map[string]string{"Accept": "application/vnd.oci.image.manifest.v1+json"}
	requireStatus(t, anon.do(http.MethodGet, inst.primary+"/v2/"+proxyNS+"/library/demo/manifests/v1", nil, manifestAccept),
		http.StatusOK, "warm the cache")
	requireStatus(t, anon.do(http.MethodGet, inst.primary+"/v2/"+proxyNS+"/library/demo/blobs/"+fx.layerDigest, nil, nil),
		http.StatusOK, "warm the blob cache")

	// A second instance over the same storage, with an upstream that refuses
	// connections, must fall back to serving the cache.
	dead := "http://127.0.0.1:1"
	inst2 := startRMSWithData(t, dead, inst.dataDir)

	resp := anon.do(http.MethodGet, inst2.primary+"/v2/"+proxyNS+"/library/demo/blobs/"+fx.layerDigest, nil, nil)
	requireStatus(t, resp, http.StatusOK, "cached blob with a dead upstream")
	if body := readBody(resp); body != string(fx.layerBody) {
		t.Errorf("degraded read returned the wrong bytes")
	}

	alice := anon.as(testUser, testPassword)
	requireStatus(t, alice.do(http.MethodPost, inst2.primary+"/v2/"+proxyNS+"/app/blobs/uploads/", nil, nil),
		http.StatusForbidden, "write to a cache-only namespace")
}

// startRMSWithData is startRMS against a specific storage directory, so a cache
// warmed by one instance can be reused by another.
func startRMSWithData(t *testing.T, upstreamURL, dataDir string) *rmsInstance {
	t.Helper()
	// healthEnabled is false because this variant exists to point rms at a
	// dead upstream: with health checks on, the namespace would never finish
	// building an engine and the cache-only path could not be exercised.
	return startRMSOpts(t, rmsOptions{
		dockerUpstream:   upstreamURL,
		secondNSUpstream: upstreamURL,
		dataDir:          dataDir,
		probeTimeout:     "2s",
	})
}

// TestDockerClient drives the real CLI when explicitly requested: it is the only
// way to prove the Location round-trip against an actual client.
func TestDockerClient(t *testing.T) {
	if os.Getenv("RMS_E2E_DOCKER") == "" {
		t.Skip("set RMS_E2E_DOCKER=1 to run the docker-driven checks")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}

	fx := newFixture(t)
	up := startFakeUpstream(t, fx, "library/demo", "v1")
	inst := startRMS(t, up.url)

	host := strings.TrimPrefix(inst.primary, "http://")
	dockerConfig := t.TempDir()

	runStdin := func(stdin string, args ...string) (string, error) {
		cmd := exec.Command("docker", args...)
		cmd.Env = append(os.Environ(), "DOCKER_CONFIG="+dockerConfig)
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	run := func(args ...string) (string, error) { return runStdin("", args...) }

	if out, err := run("pull", host+"/"+proxyNS+"/library/demo:v1"); err != nil {
		t.Fatalf("docker pull failed: %v\n%s", err, out)
	}

	if out, err := runStdin(testPassword+"\n", "login", host, "-u", testUser, "--password-stdin"); err != nil {
		t.Fatalf("docker login failed: %v\n%s", err, out)
	} else if !strings.Contains(out, "Succeeded") {
		t.Fatalf("docker login did not succeed:\n%s", out)
	}

	if out, err := run("tag", host+"/"+proxyNS+"/library/demo:v1", host+"/"+testUser+"/demo:v1"); err != nil {
		t.Fatalf("docker tag failed: %v\n%s", err, out)
	}
	if out, err := run("push", host+"/"+testUser+"/demo:v1"); err != nil {
		t.Fatalf("docker push failed: %v\n%s", err, out)
	}
}

// TestNamespaceUpstreamRaceServesTheFastestMirror drives the whole path a
// Docker daemon's registry-mirrors list uses: one unqualified request, one
// namespace, two mirrors of it, and the requirement that the fastest one that
// has the content serves it.
//
// The configured order deliberately puts the slow mirror first: with the
// ordered election this deployment used to have, the first healthy upstream
// would have won, and the point of the race is that it does not.
func TestNamespaceUpstreamRaceServesTheFastestMirror(t *testing.T) {
	fx := newFixture(t)
	slow := startFakeUpstream(t, fx, "library/demo", "v1")
	slow.delay = 200 * time.Millisecond
	fast := startFakeUpstream(t, fx, "library/demo", "v1")

	other := startFakeUpstream(t, fx, "owner/other", "v1")

	inst := startRMSOpts(t, rmsOptions{
		dockerUpstreams:  []string{slow.url, fast.url},
		secondNSUpstream: other.url,
		healthEnabled:    true,
		probeTimeout:     "2s",
	})

	anon := newClient(t)
	before := map[string]int64{slow.url: slow.hits.Load(), fast.url: fast.hits.Load()}

	resp := anon.do(http.MethodGet, inst.primary+"/v2/library/demo/manifests/v1", nil,
		map[string]string{"Accept": "application/vnd.oci.image.manifest.v1+json"})
	requireStatus(t, resp, http.StatusOK, "unqualified manifest through the raced namespace")
	if body := readBody(resp); body != string(fx.manifest) {
		t.Errorf("served manifest is not the fixture")
	}

	slowHits := slow.hits.Load() - before[slow.url]
	fastHits := fast.hits.Load() - before[fast.url]

	// Both mirrors were probed — that is the fan-out — but only the fast one
	// also served the pull, which is the part that has to follow the race.
	if slowHits == 0 || fastHits == 0 {
		t.Fatalf("probes: slow=%d fast=%d, want both mirrors asked", slowHits, fastHits)
	}
	if fastHits <= slowHits {
		t.Fatalf("served traffic: slow=%d fast=%d, want the fast mirror to serve the content", slowHits, fastHits)
	}
	t.Logf("mirror hits for one pull: slow=%d fast=%d", slowHits, fastHits)

	// And the winner is cached for the probe TTL: the second request must not
	// fan out again, so neither mirror's count jumps by the full probe set.
	beforeSlow, beforeFast := slow.hits.Load(), fast.hits.Load()
	resp = anon.do(http.MethodGet, inst.primary+"/v2/library/demo/manifests/v1", nil,
		map[string]string{"Accept": "application/vnd.oci.image.manifest.v1+json"})
	requireStatus(t, resp, http.StatusOK, "cached decision")
	_ = readBody(resp)
	if got := slow.hits.Load() - beforeSlow; got != 0 {
		t.Errorf("the slow mirror was asked %d more times within the decision TTL, want 0", got)
	}
	if got := fast.hits.Load() - beforeFast; got == 0 {
		t.Errorf("the fast mirror served the second request too, so the decision was reused")
	}
}
