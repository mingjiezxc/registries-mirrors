//go:build e2e

// Package e2e drives a real rms process against a real (in-process) upstream
// registry over HTTP, asserting the behaviours that matter: the docker login
// contract, pull-through caching, the default-namespace rewrite, the push flow
// including the Location round-trip, and the authorisation matrix.
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
	primary string // http://127.0.0.1:PORT (multi-namespace)
	docker  string // http://127.0.0.1:PORT (namespace-bound to docker.io)
	dataDir string
}

func startRMS(t *testing.T, upstreamURL string) *rmsInstance {
	t.Helper()

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

	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data: %v", err)
	}

	primaryPort := freePort(t)
	boundPort := freePort(t)
	metricsPort := freePort(t)

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
  default: %s
  proxy:
    %s:
      upstreams: [%q]
      official_library: true
      ttl: 0
listeners:
  - listen: "127.0.0.1:%d"
    namespace: %s
upstream:
  health:
    enabled: true
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
`, primaryPort, dataDir, htpasswdFile, adminUser, proxyNS, proxyNS, upstreamURL, boundPort, proxyNS, metricsPort)

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
		docker:  fmt.Sprintf("http://127.0.0.1:%d", boundPort),
		dataDir: dataDir,
	}
	waitReady(t, inst.primary+"/v2/", boundPort)
	return inst
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
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

	t.Run("default namespace rewrite", func(t *testing.T) {
		resp := anon.do(http.MethodGet, inst.primary+"/v2/library/demo/manifests/v1", nil, manifestAccept)
		requireStatus(t, resp, http.StatusOK, "unqualified two-segment manifest")
	})

	t.Run("single segment becomes a library image", func(t *testing.T) {
		// Upstream only has library/demo, so this proves the library/ injection.
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

	t.Run("namespace-bound listener needs no prefix", func(t *testing.T) {
		resp := anon.do(http.MethodGet, inst.docker+"/v2/library/demo/manifests/v1", nil, manifestAccept)
		requireStatus(t, resp, http.StatusOK, "namespace-bound manifest")
	})
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

	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")

	dir := t.TempDir()
	htpasswdFile := filepath.Join(dir, "htpasswd")
	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	adminHash, err := auth.HashPassword(adminPass)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	body := fmt.Sprintf("%s:%s\n%s:%s\n", testUser, hash, adminUser, adminHash)
	if err := os.WriteFile(htpasswdFile, []byte(body), 0o600); err != nil {
		t.Fatalf("write htpasswd: %v", err)
	}

	primaryPort := freePort(t)
	boundPort := freePort(t)
	metricsPort := freePort(t)

	cfgBody := fmt.Sprintf(`
server:
  listen: "127.0.0.1:%d"
  http_secret: "e2e-secret"
storage:
  root: %q
auth:
  type: htpasswd
  htpasswd: %q
  user_namespace: true
  shared_namespaces: [me]
  anonymous_pull_proxy: true
  admin_users: [%s]
namespaces:
  default: %s
  proxy:
    %s:
      upstreams: [%q]
      official_library: true
      ttl: 0
listeners:
  - listen: "127.0.0.1:%d"
    namespace: %s
upstream:
  health:
    enabled: false
    timeout: 2s
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
`, primaryPort, dataDir, htpasswdFile, adminUser, proxyNS, proxyNS, upstreamURL, boundPort, proxyNS, metricsPort)

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
		}
	})

	inst := &rmsInstance{
		primary: fmt.Sprintf("http://127.0.0.1:%d", primaryPort),
		docker:  fmt.Sprintf("http://127.0.0.1:%d", boundPort),
		dataDir: dataDir,
	}
	waitReady(t, inst.primary+"/v2/", primaryPort)
	return inst
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
