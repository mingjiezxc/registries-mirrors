package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/watsons/rms/internal/auth"
	"github.com/watsons/rms/internal/config"
	"github.com/watsons/rms/internal/namespace"
	"github.com/watsons/rms/internal/registry"
)

const (
	tagDigest  = "sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6"
	testRepo   = "library/alpine"
	testTag    = "latest"
	blobDigest = "sha256:661031bf07db2b64e306402ef77406214d500f9a7f0587c878f3e83689a86285"
)

func seedCache(t *testing.T, root string) {
	t.Helper()

	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	v2 := "docker/registry/v2"
	write(v2+"/repositories/"+testRepo+"/_manifests/tags/"+testTag+"/current/link", tagDigest)
	write(v2+"/repositories/"+testRepo+"/_manifests/revisions/sha256/"+
		"294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6/link", tagDigest)
	write(v2+"/blobs/sha256/66/"+
		"661031bf07db2b64e306402ef77406214d500f9a7f0587c878f3e83689a86285/data", "blob")
}

// TestClassifyCache is the cache hit/miss classifier. Getting it wrong makes the
// metrics lie, so every shape is pinned.
func TestClassifyCache(t *testing.T) {
	root := t.TempDir()
	seedCache(t, root)

	proxyRes := func(path string) namespace.Resolution {
		return namespace.Resolution{
			Namespace:  "docker.io",
			Mode:       config.ModeProxy,
			Kind:       namespace.KindRepository,
			EnginePath: path,
			Repo:       testRepo,
		}
	}

	tests := []struct {
		name      string
		path      string
		wantCache bool
		wantKnown bool
	}{
		{
			name:      "tag present in the cache",
			path:      "/v2/" + testRepo + "/manifests/" + testTag,
			wantCache: true, wantKnown: true,
		},
		{
			name:      "tag absent from the cache",
			path:      "/v2/" + testRepo + "/manifests/nope",
			wantCache: false, wantKnown: true,
		},
		{
			name:      "manifest revision present",
			path:      "/v2/" + testRepo + "/manifests/" + tagDigest,
			wantCache: true, wantKnown: true,
		},
		{
			name:      "unknown revision",
			path:      "/v2/" + testRepo + "/manifests/" + blobDigest,
			wantCache: false, wantKnown: true,
		},
		{
			name:      "blob present",
			path:      "/v2/" + testRepo + "/blobs/" + blobDigest,
			wantCache: true, wantKnown: true,
		},
		{
			name:      "blob absent",
			path:      "/v2/" + testRepo + "/blobs/" + tagDigest,
			wantCache: false, wantKnown: true,
		},
		{
			// An upload session is not a cacheable object: reporting it would
			// pollute the hit/miss counters.
			name:      "upload session is not classifiable",
			path:      "/v2/" + testRepo + "/blobs/uploads/abc-123",
			wantCache: false, wantKnown: false,
		},
		{
			name:      "tags list is not classifiable",
			path:      "/v2/" + testRepo + "/tags/list",
			wantCache: false, wantKnown: false,
		},
		{
			name:      "malformed digest is not classifiable",
			path:      "/v2/" + testRepo + "/blobs/not-a-digest",
			wantCache: false, wantKnown: false,
		},
		{
			name:      "api root is not classifiable",
			path:      "/v2/",
			wantCache: false, wantKnown: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cached, known := classifyCache(root, proxyRes(tc.path))
			if cached != tc.wantCache || known != tc.wantKnown {
				t.Errorf("classifyCache(%s) = (cached=%v, known=%v), want (%v, %v)",
					tc.path, cached, known, tc.wantCache, tc.wantKnown)
			}
		})
	}
}

func TestClassifyCacheUsesTheNamespaceRoot(t *testing.T) {
	empty := t.TempDir()
	res := namespace.Resolution{
		Namespace:  "docker.io",
		Mode:       config.ModeProxy,
		EnginePath: "/v2/" + testRepo + "/manifests/" + testTag,
	}

	// The same request classified against an empty namespace root must be a
	// miss, which is what proves the per-namespace root is being used.
	cached, known := classifyCache(empty, res)
	if !known || cached {
		t.Errorf("empty root: got (cached=%v, known=%v), want (false, true)", cached, known)
	}
}

func TestApplyCatalogPaging(t *testing.T) {
	repos := []string{"a/one", "a/two", "b/one", "c/one"}

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{name: "no query returns everything", query: "", want: repos},
		{name: "n limits the result", query: "n=2", want: repos[:2]},
		{name: "n larger than the set is ignored", query: "n=99", want: repos},
		{name: "n=0 is ignored", query: "n=0", want: repos},
		{name: "non-numeric n is ignored", query: "n=abc", want: repos},
		{name: "last skips up to and including it", query: "last=a/two", want: repos[2:]},
		{name: "last for an absent entry starts at its insertion point", query: "last=a/one-and-a-half", want: repos[1:]},
		{name: "last after everything returns nothing", query: "last=z", want: []string{}},
		{name: "last and n combine", query: "last=a/one&n=2", want: repos[1:3]},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v2/_catalog?"+tc.query, nil)
			got := applyCatalogPaging(r, append([]string(nil), repos...))
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestWriteErrorEnvelope pins the registry v2 error shape clients parse.
func TestWriteErrorEnvelope(t *testing.T) {
	w := httptest.NewRecorder()
	writeError(w, http.StatusForbidden, "DENIED", "not yours")

	if got, want := w.Code, http.StatusForbidden; got != want {
		t.Errorf("status = %d, want %d", got, want)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := w.Header().Get("Docker-Distribution-Api-Version"); got != registryAPIVersion {
		t.Errorf("API version header = %q, want %q", got, registryAPIVersion)
	}

	var envelope struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, w.Body.String())
	}
	if len(envelope.Errors) != 1 || envelope.Errors[0].Code != "DENIED" {
		t.Fatalf("unexpected envelope: %s", w.Body.String())
	}
	if envelope.Errors[0].Message != "not yours" {
		t.Errorf("message = %q", envelope.Errors[0].Message)
	}
}

func TestStatusRecorder(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, status: http.StatusOK}

	// A handler that writes a body without setting a status must still be
	// recorded as 200 exactly once.
	if _, err := sr.Write([]byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}
	sr.WriteHeader(http.StatusTeapot) // must be ignored: headers already sent

	if sr.status != http.StatusOK {
		t.Errorf("status = %d, want %d", sr.status, http.StatusOK)
	}
	if got := rec.Body.String(); got != "hi" {
		t.Errorf("body = %q, want %q", got, "hi")
	}

	sr2 := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	sr2.WriteHeader(http.StatusCreated)
	sr2.WriteHeader(http.StatusConflict) // ignored
	if sr2.status != http.StatusCreated {
		t.Errorf("second recorder status = %d, want %d", sr2.status, http.StatusCreated)
	}
}

// newTestDispatcher builds a dispatcher over a real Manager that has not been
// started, which is enough for the health endpoints and the API root.
func newTestDispatcher(t *testing.T, authType string) *dispatcher {
	t.Helper()

	authExtras := ""
	if authType == "htpasswd" {
		dir := t.TempDir()
		ht := filepath.Join(dir, "htpasswd")
		hash, err := auth.HashPassword("secret")
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		if err := os.WriteFile(ht, []byte("alice:"+hash+"\n"), 0o600); err != nil {
			t.Fatalf("write htpasswd: %v", err)
		}
		authExtras = "  htpasswd: " + ht + "\n  user_namespace: true\n"
	}

	cfgBody := `
server:
  listen: "127.0.0.1:0"
auth:
  type: ` + authType + `
` + authExtras + `namespaces:
  default: docker.io
  local: [me]
  proxy:
    docker.io:
      upstreams: ["https://registry.invalid"]
`

	cfg, err := config.Parse([]byte(cfgBody))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}

	users := []string{"alice"}
	table := namespace.NewTable(cfg, users)

	authenticator, err := auth.New(auth.Options{
		Realm:              "rms",
		HtpasswdFile:       cfg.Auth.Htpasswd,
		Users:              users,
		AnonymousPullProxy: true,
	})
	if err != nil {
		t.Fatalf("auth: %v", err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mgr := registry.New(cfg, table, log, nil, "test-secret", map[string]string{"docker.io": ""}, nil)

	return &dispatcher{
		log:        log,
		table:      table,
		auth:       authenticator,
		mgr:        mgr,
		listenerNS: "",
		storageFor: cfg.NamespaceRoot,
	}
}

func TestDispatcherHealthEndpoints(t *testing.T) {
	d := newTestDispatcher(t, "none")

	for _, path := range []string{"/healthz", "/readyz"} {
		w := httptest.NewRecorder()
		d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200 (%s)", path, w.Code, w.Body.String())
		}
	}
}

// TestDispatcherAPIRootContract pins the docker login contract: the API root
// challenges anonymously and accepts valid credentials.
func TestDispatcherAPIRootContract(t *testing.T) {
	d := newTestDispatcher(t, "htpasswd")

	w := httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous /v2/ = %d, want 401", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != `Basic realm="rms"` {
		t.Errorf("WWW-Authenticate = %q", got)
	}

	r := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	r.SetBasicAuth("alice", "secret")
	w = httptest.NewRecorder()
	d.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("authenticated /v2/ = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "{}" {
		t.Errorf("body = %q, want %q", got, "{}")
	}
}

func TestDispatcherRejectsNonRegistryPaths(t *testing.T) {
	d := newTestDispatcher(t, "none")

	for _, path := range []string{"/", "/api/version", "/v3/"} {
		w := httptest.NewRecorder()
		d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, w.Code)
		}
	}
}

// TestDispatcherCatalogRequiresAdmin covers the catalog ACL at the dispatcher
// level, including that a non-admin gets 403 rather than 401.
func TestDispatcherCatalogRequiresAdmin(t *testing.T) {
	d := newTestDispatcher(t, "htpasswd")

	w := httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/_catalog", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous catalog = %d, want 401", w.Code)
	}

	r := httptest.NewRequest(http.MethodGet, "/v2/_catalog", nil)
	r.SetBasicAuth("alice", "secret")
	w = httptest.NewRecorder()
	d.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("non-admin catalog = %d, want 403", w.Code)
	}
}

func TestLabelOr(t *testing.T) {
	if got := labelOr("", "fallback"); got != "fallback" {
		t.Errorf("labelOr empty = %q", got)
	}
	if got := labelOr("v", "fallback"); got != "v" {
		t.Errorf("labelOr value = %q", got)
	}
}

func TestDigestHelperMatchesLayoutExpectations(t *testing.T) {
	// Guards the fixture constants against a typo, since classifyCache depends
	// on the two paths agreeing.
	if _, err := digest.Parse(tagDigest); err != nil {
		t.Errorf("tagDigest is not a valid digest: %v", err)
	}
	if _, err := digest.Parse(blobDigest); err != nil {
		t.Errorf("blobDigest is not a valid digest: %v", err)
	}
}
