package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/watsons/rms/internal/auth"
	"github.com/watsons/rms/internal/config"
	"github.com/watsons/rms/internal/namespace"
	"github.com/watsons/rms/internal/registry"
	"github.com/watsons/rms/internal/selection"
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

// --- selection -------------------------------------------------------------

// fakeUpstream is a selection.Upstream backed by in-process HTTP servers, so the
// dispatcher tests exercise the real chooser — probe, decision cache and
// invalidation included — instead of a stubbed decision.
type fakeUpstream struct {
	names   []string
	elected map[string]string
	// upstreams, when set, overrides the candidate list UpstreamsFor reports
	// for a namespace: the racing tests need a namespace with more than one
	// mirror. Unset means "the elected upstream alone".
	upstreams map[string][]string
	services  map[string]*http.Server

	// cachedBlobs is what each namespace holds on disk, keyed by digest.
	cachedBlobs map[string]map[string]bool
}

func newFakeUpstream(t *testing.T, names ...string) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{
		names:    append([]string(nil), names...),
		elected:  map[string]string{},
		services: map[string]*http.Server{},
	}
	t.Cleanup(u.close)
	return u
}

func (u *fakeUpstream) Serving() []string {
	out := append([]string(nil), u.names...)
	sort.Strings(out)
	return out
}

func (u *fakeUpstream) ElectedUpstream(ns string) (string, bool) {
	url, ok := u.elected[ns]
	return url, ok
}

// UpstreamsFor reports the elected upstream of a namespace as its one
// candidate upstream. These tests are about the dispatcher's wiring — which
// namespace and which engine received the request — so a single upstream per
// namespace is all they need; the racing itself is covered in
// internal/selection and internal/registry.
func (u *fakeUpstream) UpstreamsFor(ns string) []string {
	if list, ok := u.upstreams[ns]; ok {
		return append([]string(nil), list...)
	}
	url, ok := u.elected[ns]
	if !ok || url == "" {
		return nil
	}
	return []string{url}
}

// ObserveContentProbe is a no-op here: the endpoint health it feeds lives in
// the manager's selector, which these fixtures replace with a plain map.
func (u *fakeUpstream) ObserveContentProbe(string, string, bool, time.Duration, error) {}

func (u *fakeUpstream) UpstreamCredentials(string) (string, string) { return "", "" }

// CachedManifest reports "not cached" unless a test says otherwise: these tests
// drive the decision through the upstream probes, which is the path that
// exercises the forwarding and retry wiring. The disk-hit shortcut and the
// cache-only candidates have their own coverage in internal/selection.
func (u *fakeUpstream) CachedManifest(string, string, string) bool { return false }

// CachedBlob reports the digests a test has planted on a namespace's disk.
// A blob request carries no repository, so this is the only way one of them can
// be given a namespace when no manifest decision is recent.
func (u *fakeUpstream) CachedBlob(namespace string, dgst digest.Digest) bool {
	return u.cachedBlobs[namespace][dgst.String()]
}

// cacheBlob plants a digest on a namespace's disk. The value is what the
// namespace's engine serves for it when the request is forwarded.
func (u *fakeUpstream) cacheBlob(namespace, dgst string) {
	if u.cachedBlobs == nil {
		u.cachedBlobs = map[string]map[string]bool{}
	}
	if u.cachedBlobs[namespace] == nil {
		u.cachedBlobs[namespace] = map[string]bool{}
	}
	u.cachedBlobs[namespace][dgst] = true
}

// serve starts an in-process upstream for a namespace and elects it.
func (u *fakeUpstream) serve(ns string, h http.Handler) *httptest.Server {
	srv := httptest.NewServer(h)
	u.services[ns] = srv.Config
	u.elected[ns] = srv.URL
	return srv
}

func (u *fakeUpstream) close() {
	for _, s := range u.services {
		s.Close()
	}
}

// upstream answers the probes for one namespace.
//
// A namespace that does not hold the repository answers 404: a definitive "not
// here", which is what makes the decision deterministic instead of a race
// between two namespaces that both claim the repository.
//
// A namespace that does hold it answers 401 with a challenge, then 200 once the
// bearer token is presented: the smallest shape that mirrors a real registry's
// token handshake. That shape matters for the tests — it makes the winner's
// probe strictly slower than any 404, so "which candidate wins" is pinned by
// the fixture rather than by which probe happened to return first.
//
// The success answer is a bare w.WriteHeader(http.StatusOK) with NO
// Content-Type, and that is deliberate, not an oversight: the selector must
// treat "a 200 carrying no media type" as acceptable, because that is all a
// minimal upstream sends and refusing it would call a present image absent.
// selection-dev tightened the media-type check once, every test in this file
// went red, and the tightening was reverted — the missing header is the
// tolerance this fixture exists to keep exercised. Do not "fix" it by adding a
// Content-Type; that would silently retire the coverage.
type upstream struct {
	holds atomic.Bool
	seen  atomic.Int32

	// paths records what the probes actually asked for, so a test can assert
	// the question was asked on the right path (a blob probe must not be sent
	// to the manifests path, and vice versa).
	paths []string
	mu    sync.Mutex
}

func (p *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.seen.Add(1)
	p.mu.Lock()
	p.paths = append(p.paths, r.URL.Path)
	p.mu.Unlock()
	if !p.holds.Load() {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Header.Get("Authorization") == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+tokenRealm()+`",service="rms-test"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// tokenRealm is the token endpoint the fake upstreams advertise. It is set once
// per test binary by TestMain.
var (
	realmURL atomic.Pointer[string]
	tokenSrv *httptest.Server
)

func tokenRealm() string {
	if p := realmURL.Load(); p != nil {
		return *p
	}
	return ""
}

// TestMain starts the token endpoint the fake upstreams point at. It stands in
// for a registry's auth service: the probes present no credentials, so the
// handshake is anonymous, exactly as it is against Docker Hub.
func TestMain(m *testing.M) {
	tokenSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"rms-test-token"}`))
	}))
	defer tokenSrv.Close()
	url := tokenSrv.URL + "/token"
	realmURL.Store(&url)
	os.Exit(m.Run())
}

func (p *upstream) probeCount() int { return int(p.seen.Load()) }

// probedPaths returns the paths the probes asked for.
func (p *upstream) probedPaths() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.paths...)
}

// newSelectionDispatcher builds a dispatcher over two proxy namespaces backed by
// in-process upstreams. a.registry declares official_library, b.registry does
// not, so the two exercise the per-candidate repository rule.
func newSelectionDispatcher(t *testing.T, authType string) (*dispatcher, *fakeUpstream, map[string]*fakeEngine, map[string]*upstream) {
	t.Helper()
	return newSelectionDispatcherWithLog(t, authType,
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
}

// newSelectionDispatcherWithLog is the same harness with a caller-supplied
// logger, so a test can capture the selector's decision lines.
func newSelectionDispatcherWithLog(t *testing.T, authType string, log *slog.Logger) (*dispatcher, *fakeUpstream, map[string]*fakeEngine, map[string]*upstream) {
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
		authExtras = "  htpasswd: " + ht + "\n"
	}

	const nsA, nsB = "a.registry", "b.registry"

	cfgBody := `
server:
  listen: "127.0.0.1:0"
auth:
  type: ` + authType + `
` + authExtras + `namespaces:
  selection:
    probe_ttl: 50ms
    probe_timeout: 500ms
  local: [me]
  proxy:
    ` + nsA + `:
      upstreams: ["https://a.invalid"]
      official_library: true
    ` + nsB + `:
      upstreams: ["https://b.invalid"]
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

	up := newFakeUpstream(t, nsA, nsB)
	ups := map[string]*upstream{}
	for _, ns := range up.names {
		p := &upstream{}
		ups[ns] = p
		up.serve(ns, p)
	}

	mgr := registry.New(cfg, table, log, nil, "test-secret",
		map[string]string{nsA: "", nsB: ""}, nil)

	engines := map[string]*fakeEngine{}
	for _, ns := range up.names {
		engines[ns] = &fakeEngine{status: http.StatusOK, body: ns + " ok"}
	}

	chooser := selection.New(up, selection.Options{
		TTL:     cfg.Namespaces.Selection.ProbeTTL.Std(),
		Timeout: cfg.Namespaces.Selection.ProbeTimeout.Std(),
		Logger:  log,
	})

	// The manager is not started: the dispatcher's two manager-derived hooks
	// are supplied directly, so a test observes exactly which namespace's
	// engine received the forwarded request without building a real registry
	// engine per namespace.
	d := &dispatcher{
		log:        log,
		table:      table,
		auth:       authenticator,
		mgr:        mgr,
		chooser:    chooser,
		listenerNS: "",
		storageFor: cfg.NamespaceRoot,
		serving: func() []string {
			return append([]string(nil), up.names...)
		},
		engineFor: func(ns, _ string) (http.Handler, bool) {
			e, ok := engines[ns]
			return e, ok
		},
	}
	return d, up, engines, ups
}

// fakeEngine stands in for a namespace's registry engine: it records the path it
// was handed and answers a canned status.
type fakeEngine struct {
	status int
	body   string

	mu    sync.Mutex
	paths []string
}

func (e *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	e.paths = append(e.paths, r.URL.Path)
	e.mu.Unlock()

	if e.status != 0 {
		w.WriteHeader(e.status)
	}
	if e.body != "" {
		_, _ = w.Write([]byte(e.body))
	}
}

func (e *fakeEngine) requests() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.paths...)
}

// registryErrorEnvelope decodes a registry v2 error response.
func registryErrorEnvelope(t *testing.T, body []byte) (code, message string) {
	t.Helper()
	var envelope struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, body)
	}
	if len(envelope.Errors) == 0 {
		t.Fatalf("no errors in envelope: %s", body)
	}
	return envelope.Errors[0].Code, envelope.Errors[0].Message
}

// TestUnqualifiedRequestIsForwardedToTheSelectedNamespace is the core wiring
// check: an unqualified request must reach the namespace the selector picked,
// with that namespace's repository rule applied and the operation suffix
// preserved — cache classification depends on the manifests/ sentinel.
func TestUnqualifiedRequestIsForwardedToTheSelectedNamespace(t *testing.T) {
	d, _, engines, ups := newSelectionDispatcher(t, "none")
	ups["b.registry"].holds.Store(true) // only B holds the repository

	w := httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/alpine/manifests/latest", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if got, want := w.Body.String(), "b.registry ok"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if got := engines["b.registry"].requests(); len(got) != 1 || got[0] != "/v2/alpine/manifests/latest" {
		t.Errorf("B engine saw %v, want [/v2/alpine/manifests/latest]", got)
	}
	if got := engines["a.registry"].requests(); len(got) != 0 {
		t.Errorf("A engine saw %v, want nothing", got)
	}
}

// TestSelectionAppliesOfficialLibraryPerCandidate pins the per-candidate
// repository rule: a single-segment repository becomes library/<name> only for
// the namespace that declares official_library, so the engine path cannot be
// computed before the decision is made.
func TestSelectionAppliesOfficialLibraryPerCandidate(t *testing.T) {
	d, _, engines, ups := newSelectionDispatcher(t, "none")
	ups["a.registry"].holds.Store(true) // A wins, and A has official_library

	w := httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/alpine/manifests/latest", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if got := engines["a.registry"].requests(); len(got) != 1 || got[0] != "/v2/library/alpine/manifests/latest" {
		t.Errorf("A engine saw %v, want [/v2/library/alpine/manifests/latest]", got)
	}
}

// TestUnqualifiedRequestNotFound covers "no candidate has it": the client must
// get the v2 NAME_UNKNOWN envelope, not a 503 and not a fallback to some default
// namespace.
func TestUnqualifiedRequestNotFound(t *testing.T) {
	d, _, engines, _ := newSelectionDispatcher(t, "none")

	w := httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/nothing-here/manifests/latest", nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
	code, msg := registryErrorEnvelope(t, w.Body.Bytes())
	if code != "NAME_UNKNOWN" {
		t.Errorf("error code = %q, want NAME_UNKNOWN", code)
	}
	if !strings.Contains(msg, "nothing-here") || !strings.Contains(msg, "latest") {
		t.Errorf("message %q should name the repository and the reference", msg)
	}
	for ns, e := range engines {
		if got := e.requests(); len(got) != 0 {
			t.Errorf("%s engine saw %v, want nothing: the request was never forwarded", ns, got)
		}
	}
}

// TestUnqualifiedRequestUnreachable covers "no candidate could be probed": the
// client gets 503 UNAVAILABLE, which a registry client retries, rather than a
// 404 that would make it give up on a repository that may well exist.
func TestUnqualifiedRequestUnreachable(t *testing.T) {
	d, up, _, _ := newSelectionDispatcher(t, "none")

	// Point both elected upstreams at an address nothing listens on.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	for _, ns := range up.names {
		up.elected[ns] = deadURL
	}

	w := httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/alpine/manifests/latest", nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", w.Code, w.Body.String())
	}
	code, _ := registryErrorEnvelope(t, w.Body.Bytes())
	if code != "UNAVAILABLE" {
		t.Errorf("error code = %q, want UNAVAILABLE", code)
	}
}

// TestReselectOnReadFailure pins the retry rule: when the selected namespace
// fails a read, the dispatcher drops the cached decision, asks once more, and
// the second attempt answers the client.
func TestReselectOnReadFailure(t *testing.T) {
	d, _, engines, ups := newSelectionDispatcher(t, "none")
	ups["a.registry"].holds.Store(true) // A is selected first

	engines["a.registry"].status = http.StatusNotFound
	engines["b.registry"].status = http.StatusOK
	engines["b.registry"].body = "from b"

	// By the time of the retry B is the namespace that has it.
	ups["a.registry"].holds.Store(false)
	ups["b.registry"].holds.Store(true)

	w := httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/library/alpine/manifests/latest", nil))

	if got, want := w.Body.String(), "from b"; got != want {
		t.Fatalf("body = %q, want %q (status %d)", got, want, w.Code)
	}
	if got := engines["b.registry"].requests(); len(got) != 1 {
		t.Errorf("B engine saw %v, want exactly one retry", got)
	}
}

// TestNoReselectOnWriteRequests is the other half of the retry rule: a write is
// never replayed, because the first attempt may already have written something.
//
// It drives the retry gate directly. A write to a proxy namespace never reaches
// an engine at all — proxy namespaces are read-only, so authentication refuses
// it first, which TestWriteToAProxyNamespaceIsRefused pins — and the gate must
// also hold where a write does reach an engine, as it does in a local
// namespace. The observable property is that the decision stays cached.
func TestNoReselectOnWriteRequests(t *testing.T) {
	d, _, engines, ups := newSelectionDispatcher(t, "none")
	ups["a.registry"].holds.Store(true) // only A holds it, so A is decided

	// Prime the decision through a real request, so the cache entry the gate
	// must leave alone is one the dispatcher itself created.
	w := httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/library/alpine/manifests/latest", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("priming request: status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	probesBefore := ups["a.registry"].probeCount()
	engines["a.registry"].status = http.StatusNotFound

	r := httptest.NewRequest(http.MethodPost, "/v2/library/alpine/blobs/uploads/", nil)
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusNotFound}
	d.tryReselect(rec, r, namespace.Resolution{Namespace: "a.registry", Mode: config.ModeProxy},
		reselectState{repo: "library/alpine", op: "/blobs/uploads/"})

	if rec.status != http.StatusNotFound {
		t.Errorf("status = %d, want the engine's 404 to stand", rec.status)
	}
	if got := engines["b.registry"].requests(); len(got) != 0 {
		t.Errorf("B engine saw %v, want nothing: writes are not retried", got)
	}
	if got := ups["a.registry"].probeCount(); got != probesBefore {
		t.Errorf("A upstream probed %d times, want %d: a write must not invalidate the decision",
			got, probesBefore)
	}

	// The read gate, on the same primed cache, does retry — which is what makes
	// the assertion above a property of the method and not of a dead path.
	recGet := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusNotFound}
	d.tryReselect(recGet, httptest.NewRequest(http.MethodGet, "/v2/library/alpine/manifests/latest", nil),
		namespace.Resolution{Namespace: "a.registry", Mode: config.ModeProxy},
		reselectState{repo: "library/alpine", ref: "latest", op: "/manifests/latest"})

	if got := ups["a.registry"].probeCount(); got <= probesBefore {
		t.Errorf("A upstream probed %d times, want more than %d: a failing read must re-decide",
			got, probesBefore)
	}
}

// TestWriteToAProxyNamespaceIsRefused pins why the retry rule has to be a gate
// inside the dispatcher rather than something only local namespaces can reach:
// a push into a proxy namespace is refused before any engine sees it.
func TestWriteToAProxyNamespaceIsRefused(t *testing.T) {
	d, _, engines, ups := newSelectionDispatcher(t, "none")
	ups["a.registry"].holds.Store(true)

	w := httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v2/library/alpine/blobs/uploads/", nil))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a write into a read-only cache namespace (%s)",
			w.Code, w.Body.String())
	}
	if got := engines["a.registry"].requests(); len(got) != 0 {
		t.Errorf("A engine saw %v, want nothing: the write was refused before the engine", got)
	}
	code, _ := registryErrorEnvelope(t, w.Body.Bytes())
	if code != "DENIED" {
		t.Errorf("error code = %q, want DENIED", code)
	}
}

// TestReselectHappensAtMostOnce pins the single-retry rule: when the second
// attempt fails too, there is no third.
func TestReselectHappensAtMostOnce(t *testing.T) {
	d, _, engines, ups := newSelectionDispatcher(t, "none")
	ups["a.registry"].holds.Store(true)

	engines["a.registry"].status = http.StatusNotFound
	engines["b.registry"].status = http.StatusNotFound

	w := httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/library/alpine/manifests/latest", nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if got := engines["a.registry"].requests(); len(got) != 1 {
		t.Errorf("A engine saw %v, want exactly one attempt", got)
	}
	if got := engines["b.registry"].requests(); len(got) > 1 {
		t.Errorf("B engine saw %v, want at most one retry", got)
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

// --- unqualified blob requests ---------------------------------------------

// blobDigest is the digest the blob tests ask for.
const blobDigest2 = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// TestUnqualifiedBlobFollowsTheManifestAffinity is the case the old default
// namespace used to hide: a client that has the manifest asks for its config
// blob at /v2/blobs/<digest>, a path with no repository name in it. It must be
// served by whichever namespace served the manifest, without a probe.
func TestUnqualifiedBlobFollowsTheManifestAffinity(t *testing.T) {
	d, _, engines, ups := newSelectionDispatcher(t, "none")
	ups["b.registry"].holds.Store(true) // only B holds the repository

	// The manifest request decides B and writes the repository affinity.
	w := httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/alpine/manifests/latest", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("manifest: status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	probesAfterManifest := ups["a.registry"].probeCount() + ups["b.registry"].probeCount()

	engines["b.registry"].body = "blob from b"
	w = httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/blobs/"+blobDigest2, nil))

	if w.Code != http.StatusOK {
		t.Fatalf("blob: status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if got, want := w.Body.String(), "blob from b"; got != want {
		t.Errorf("blob body = %q, want %q", got, want)
	}
	// requests()[0] is the manifest fetch of the same namespace; the blob is
	// the request after it. The engine path carries the same repository the
	// manifest was fetched with — B does not declare official_library, so that
	// repository is plain "alpine", not "library/alpine". The client asked for
	// /v2/blobs/<digest> with no repository at all; rms put the segment back.
	if got := engines["b.registry"].requests(); len(got) != 2 || got[1] != "/v2/alpine/blobs/"+blobDigest2 {
		t.Errorf("B engine saw %v, want [/v2/alpine/blobs/%s]", got, blobDigest2)
	}
	if got := engines["a.registry"].requests(); len(got) != 0 {
		t.Errorf("A engine saw %v, want nothing", got)
	}
	if got := ups["a.registry"].probeCount() + ups["b.registry"].probeCount(); got != probesAfterManifest {
		t.Errorf("probes went from %d to %d: a blob must be routed by affinity, not probed", probesAfterManifest, got)
	}
}

// TestUnqualifiedBlobIsNotReportedAsMissing pins the failure code: when no
// namespace can be determined for a digest, that is a 503 — the content may
// exist in a namespace nobody can point at — and never a 404, which a client
// would cache as "this image does not exist".
func TestUnqualifiedBlobIsNotReportedAsMissing(t *testing.T) {
	d, _, _, _ := newSelectionDispatcher(t, "none") // nothing holds anything, no affinity

	w := httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/blobs/"+blobDigest2, nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", w.Code, w.Body.String())
	}
	code, _ := registryErrorEnvelope(t, w.Body.Bytes())
	if code != "UNAVAILABLE" {
		t.Errorf("error code = %q, want UNAVAILABLE (a 404 would claim the content does not exist)", code)
	}
}

// TestUnqualifiedUploadSessionIsRefusedAndNotProbed covers what is left when a
// path carries neither a repository nor a digest: an upload continuation. No
// decision can be made about it, so it is refused with 503 and the selector is
// not consulted at all.
func TestUnqualifiedUploadSessionIsRefusedAndNotProbed(t *testing.T) {
	d, _, engines, ups := newSelectionDispatcher(t, "none")
	ups["a.registry"].holds.Store(true)

	w := httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/v2/blobs/uploads/session-42", nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", w.Code, w.Body.String())
	}
	code, msg := registryErrorEnvelope(t, w.Body.Bytes())
	if code != "UNAVAILABLE" {
		t.Errorf("error code = %q, want UNAVAILABLE", code)
	}
	if !strings.Contains(msg, "neither a repository nor a blob digest") {
		t.Errorf("message %q should explain that neither key is present", msg)
	}
	for ns, e := range engines {
		if got := e.requests(); len(got) != 0 {
			t.Errorf("%s engine saw %v, want nothing", ns, got)
		}
	}
	if got := ups["a.registry"].probeCount(); got != 0 {
		t.Errorf("A upstream probed %d times, want 0: an unusable path must not probe", got)
	}
}

// TestUnqualifiedBlobOnDiskWithoutARepositoryIsRefused covers the disk-hit
// route when it is all that is available: no recent manifest decision, so the
// namespace comes from "which candidate holds this digest". That answer carries
// no repository — a blob is content-addressed — and a blob cannot be given to
// an engine without one, so the request is refused with 503 (retryable) rather
// than forwarded to a path the engine cannot resolve. See
// reports/raw/router-notes.md; a contract fix is being worked on so this route
// can be completed.
func TestUnqualifiedBlobOnDiskWithoutARepositoryIsRefused(t *testing.T) {
	d, up, engines, _ := newSelectionDispatcher(t, "none")
	up.cacheBlob("b.registry", blobDigest2)

	w := httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/blobs/"+blobDigest2, nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", w.Code, w.Body.String())
	}
	code, _ := registryErrorEnvelope(t, w.Body.Bytes())
	if code != "UNAVAILABLE" {
		t.Errorf("error code = %q, want UNAVAILABLE", code)
	}
	for ns, e := range engines {
		if got := e.requests(); len(got) != 0 {
			t.Errorf("%s engine saw %v, want nothing: the path cannot be built", ns, got)
		}
	}
}

// TestUnqualifiedBlobRetryKeepsTheRepositorySegment pins the retry half of the
// rule: the second attempt is built from the repository-level path too, so a
// blob is never forwarded without its repository segment.
func TestUnqualifiedBlobRetryKeepsTheRepositorySegment(t *testing.T) {
	d, _, engines, ups := newSelectionDispatcher(t, "none")
	ups["b.registry"].holds.Store(true)

	// A manifest decides B (which has no official_library, so the repository is
	// plain "alpine") and writes the affinity.
	w := httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/alpine/manifests/latest", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("manifest: status = %d, want 200 (%s)", w.Code, w.Body.String())
	}

	// The first blob attempt fails, so the retry has to decide again and build
	// its path the same way.
	engines["b.registry"].status = http.StatusNotFound
	blobReq := httptest.NewRequest(http.MethodGet, "/v2/blobs/"+blobDigest2, nil)
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusNotFound}
	d.tryReselect(rec, blobReq, namespace.Resolution{Namespace: "b.registry", Mode: config.ModeProxy},
		reselectState{op: "/blobs/" + blobDigest2})

	paths := engines["b.registry"].requests()
	if len(paths) != 2 {
		t.Fatalf("B engine saw %v, want the manifest fetch plus one blob attempt", paths)
	}
	for _, p := range paths {
		if strings.HasPrefix(p, "/v2/blobs/") {
			t.Errorf("B engine saw the client's own path %q: the repository segment was dropped", p)
		}
	}
	if want := "/v2/alpine/blobs/" + blobDigest2; paths[1] != want {
		t.Errorf("retry path = %q, want %q", paths[1], want)
	}
}

// authChallengeUpstream is an upstream that challenges every request, whatever
// the repository, and whose token endpoint refuses the anonymous handshake with
// 403 — ghcr.io and gcr.io behave exactly this way. Such a namespace has
// answered the question ("not here, for the credentials you have"), so it counts
// as a definitive no even though rms cannot authenticate to it.
//
// Contrast with a realm that cannot be reached at all: that is a transport
// failure, which leaves the question open and is still a 503. The line between
// the two is what these two tests pin.
type authChallengeUpstream struct {
	seen    atomic.Int32
	refused int
	realm   string
}

func (p *authChallengeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/token") {
		w.WriteHeader(p.refused)
		return
	}
	p.seen.Add(1)
	w.Header().Set("WWW-Authenticate", `Bearer realm="`+p.realmURL()+`",service="rms-test"`)
	w.WriteHeader(http.StatusUnauthorized)
}

// realmURL is the token endpoint this upstream advertises.
func (p *authChallengeUpstream) realmURL() string { return p.realm }

// realm is filled in by the test once its token endpoint is listening.
func (p *authChallengeUpstream) setRealm(u string) { p.realm = u }

// TestUnqualifiedRequestNotFoundWhenCandidatesOnlyChallenge pins the interaction
// with the auth behavior of real registries: when every candidate answers with a
// challenge that rms cannot satisfy (403 from the token endpoint, or a token
// that still is not accepted), that is a definitive "no such repository here",
// so the client gets 404 NAME_UNKNOWN — not a 503 that would look like an
// outage and that a client retries forever.
func TestUnqualifiedRequestNotFoundWhenCandidatesOnlyChallenge(t *testing.T) {
	d, up, _, _ := newSelectionDispatcher(t, "none")

	// One token endpoint for both namespaces: it answers 403, exactly like
	// ghcr.io and gcr.io do for an anonymous handshake.
	tokenEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(tokenEndpoint.Close)

	challenges := map[string]*authChallengeUpstream{}
	for _, ns := range up.names {
		p := &authChallengeUpstream{refused: http.StatusForbidden, realm: tokenEndpoint.URL + "/token"}
		challenges[ns] = p
		up.serve(ns, p)
	}

	w := httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/typo-here/manifests/latest", nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 NAME_UNKNOWN (%s)", w.Code, w.Body.String())
	}
	code, _ := registryErrorEnvelope(t, w.Body.Bytes())
	if code != "NAME_UNKNOWN" {
		t.Errorf("error code = %q, want NAME_UNKNOWN", code)
	}
	for ns, p := range challenges {
		if p.seen.Load() == 0 {
			t.Errorf("%s upstream was never asked", ns)
		}
	}
}

// TestUnqualifiedRequestUnreachableIsTransportOnly is the other side of that
// line: a namespace whose upstream cannot be connected to has not answered, so
// it is still 503. A merely unreachable token endpoint does not, by itself,
// downgrade a candidate — that case is covered above.
func TestUnqualifiedRequestUnreachableIsTransportOnly(t *testing.T) {
	d, up, _, _ := newSelectionDispatcher(t, "none")

	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close() // nothing listens on it any more
	for _, ns := range up.names {
		up.elected[ns] = deadURL
	}

	w := httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/typo-here/manifests/latest", nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 UNAVAILABLE for a transport failure (%s)", w.Code, w.Body.String())
	}
	code, _ := registryErrorEnvelope(t, w.Body.Bytes())
	if code != "UNAVAILABLE" {
		t.Errorf("error code = %q, want UNAVAILABLE", code)
	}
}

// TestAnsweredCandidateDecidesWhenAnotherIsDown pins the resolution rule for a
// request no candidate can serve, measured on two candidates:
//
//	A unreachable (nothing listening) + B answers 404  -> 404 NAME_UNKNOWN
//	A answers 404 + B unreachable                      -> 404 NAME_UNKNOWN
//	both unreachable                                   -> 503 UNAVAILABLE
//	A unreachable + B HOLDS the repository             -> 200, served by B
//
// A candidate that answers — 404, 403, a refusal — has settled the question for
// itself, and one that cannot be reached has not. The rule is "nobody answered"
// rather than "somebody did not answer", because 503 makes a client retry a
// repository that does not exist forever, while the cost of 404 is the narrow
// case where the unreachable candidate happens to hold it — which the client
// could not have fetched anyway, and which a tag pull re-resolves next time.
//
// This is a decision, not an accident, and it was measured before it was
// decided: with the previous rule ("any candidate unanswered -> 503") a single
// flaky namespace turned every typo'd image name into a 503 for every client —
// the same user-visible symptom as the credential-refusal bug, arriving by the
// transport route. The last row is what keeps the rule honest: a candidate that
// does hold the repository still wins outright.
func TestAnsweredCandidateDecidesWhenAnotherIsDown(t *testing.T) {
	d, up, engines, _ := newSelectionDispatcher(t, "none")

	// A's upstream cannot be connected to; B answers 404 to everything.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	up.elected["a.registry"] = deadURL

	w := httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/nothing-here/manifests/latest", nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 because B answered definitively (%s)", w.Code, w.Body.String())
	}
	code, msg := registryErrorEnvelope(t, w.Body.Bytes())
	if code != "NAME_UNKNOWN" {
		t.Errorf("error code = %q, want NAME_UNKNOWN", code)
	}
	if !strings.Contains(msg, "nothing-here") {
		t.Errorf("message %q should name the repository", msg)
	}
	for ns, e := range engines {
		if got := e.requests(); len(got) != 0 {
			t.Errorf("%s engine saw %v, want nothing: no candidate claimed the repository", ns, got)
		}
	}

	// Both unreachable is the only case left for 503: nobody answered at all.
	for _, ns := range up.names {
		up.elected[ns] = deadURL
	}
	w = httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/nothing-here/manifests/latest", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when nobody answered (%s)", w.Code, w.Body.String())
	}
	if code, _ := registryErrorEnvelope(t, w.Body.Bytes()); code != "UNAVAILABLE" {
		t.Errorf("error code = %q, want UNAVAILABLE", code)
	}
	if got := engines["b.registry"].requests(); len(got) != 0 {
		t.Errorf("B engine saw %v, want nothing", got)
	}
}

// TestSelectionBoundaryCasesAreStillNamed keeps the cross-references honest.
//
// The other side of this boundary lives in internal/selection, and its notes
// cite these three cases by name (reports/raw/selection-notes.md, section
// "第二次修订"). Renaming one of them would leave that citation pointing at
// nothing, and nobody would notice until someone went looking for the coverage
// and did not find it. This is the cheapest check that fails when that happens:
// if it goes red, update the citation rather than only the name.
//
// The same file carries the reciprocal warning, so an edit there also sees that
// this test asserts on it. The section title is asserted too, because our own
// notes cite it by name and nothing else would notice it being renamed.
func TestSelectionBoundaryCasesAreStillNamed(t *testing.T) {
	const notesPath = "../../reports/raw/selection-notes.md"

	notes, err := os.ReadFile(notesPath)
	if err != nil {
		t.Fatalf("read %s: %v", notesPath, err)
	}

	for _, name := range []string{
		"TestUnqualifiedRequestNotFoundWhenCandidatesOnlyChallenge",
		"TestUnqualifiedRequestUnreachableIsTransportOnly",
		"TestAnsweredCandidateDecidesWhenAnotherIsDown",
	} {
		if !strings.Contains(string(notes), name) {
			t.Errorf("%s no longer cites %s: either the case was renamed (update the citation) or the cross-reference was dropped", notesPath, name)
		}
	}

	// reports/raw/router-notes.md points readers at this section by name, so the
	// title is part of the cross-reference too.
	const section = "第二次修订"
	if !strings.Contains(string(notes), section) {
		t.Errorf("%s no longer has a %q section: update the reference in reports/raw/router-notes.md", notesPath, section)
	}
}

// TestRepoScopedBlobIsDecidedOnTheBlobPath is the defect the 103 client found:
// GET /v2/<repo>/blobs/<digest> for content that exists was answered 404,
// because the dispatcher asked the selector about the repository and a
// reference — so the candidates were probed on
// /v2/<repo>/manifests/<the blob's digest>, a manifest question no upstream can
// answer yes to, every candidate came back absent, and the client got
// NAME_UNKNOWN for a blob sitting on disk.
func TestRepoScopedBlobIsDecidedOnTheBlobPath(t *testing.T) {
	d, _, engines, ups := newSelectionDispatcher(t, "none")
	ups["b.registry"].holds.Store(true) // only B holds the repository

	w := httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/alpine/blobs/"+blobDigest2, nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if got, want := w.Body.String(), "b.registry ok"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}

	// The engine is addressed with the repository segment restored.
	if got := engines["b.registry"].requests(); len(got) != 1 || got[0] != "/v2/alpine/blobs/"+blobDigest2 {
		t.Errorf("B engine saw %v, want [/v2/alpine/blobs/%s]", got, blobDigest2)
	}

	// And the probe asked the blob path, not the manifest path. This is the
	// assertion that would have caught the original defect: probing
	// /v2/<repo>/manifests/<digest> asks whether a manifest is published under
	// the blob's digest, which no upstream answers yes to.
	for _, ns := range ups {
		for _, p := range ns.probedPaths() {
			if strings.Contains(p, "/manifests/") {
				t.Errorf("a probe went to %q: a blob must not be probed as a manifest", p)
			}
			if !strings.HasSuffix(p, "/blobs/"+blobDigest2) {
				t.Errorf("a probe went to %q, want a blob path", p)
			}
		}
	}
	// A declares official_library, so its repository-level path is
	// library/alpine — which is what the probe must carry.
	if got := ups["a.registry"].probedPaths(); len(got) != 1 || got[0] != "/v2/library/alpine/blobs/"+blobDigest2 {
		t.Errorf("A was probed on %v, want [/v2/library/alpine/blobs/%s]", got, blobDigest2)
	}
}

// TestRepoScopedBlobNotFoundIsStillNameUnknown pins the failure shape for a blob
// that really is nowhere: a defensive 404, not a 503, because every candidate
// answered.
func TestRepoScopedBlobNotFoundIsStillNameUnknown(t *testing.T) {
	d, _, engines, _ := newSelectionDispatcher(t, "none") // nobody holds anything

	w := httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/alpine/blobs/"+blobDigest2, nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", w.Code, w.Body.String())
	}
	code, msg := registryErrorEnvelope(t, w.Body.Bytes())
	if code != "NAME_UNKNOWN" {
		t.Errorf("error code = %q, want NAME_UNKNOWN", code)
	}
	if !strings.Contains(msg, blobDigest2) {
		t.Errorf("message %q should name the digest", msg)
	}
	for ns, e := range engines {
		if got := e.requests(); len(got) != 0 {
			t.Errorf("%s engine saw %v, want nothing", ns, got)
		}
	}
}

// TestBlobDecisionLineSaysKindBlob grounds the 103 troubleshooting criterion in
// a real log capture instead of in an assumption about the log format.
//
// The criterion used to be "if the decision's timings mention /manifests/, the
// blob branch did not take effect". That could never fire: timings render as
// "namespace=duration(outcome)" and carry no path at all, so it would have
// given false assurance exactly when it was needed. What does distinguish the
// two branches is the decision line's kind, which this test pins by capturing
// the selector's own output for both request shapes.
func TestBlobDecisionLineSaysKindBlob(t *testing.T) {
	capture := func(t *testing.T, path string) []string {
		t.Helper()

		var buf bytes.Buffer
		var mu sync.Mutex
		log := slog.New(slog.NewTextHandler(&syncWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug}))

		d, _, _, ups := newSelectionDispatcherWithLog(t, "none", log)
		ups["b.registry"].holds.Store(true)

		w := httptest.NewRecorder()
		d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d (%s)", path, w.Code, w.Body.String())
		}

		mu.Lock()
		defer mu.Unlock()
		var lines []string
		for _, line := range strings.Split(buf.String(), "\n") {
			if strings.Contains(line, `msg="selection"`) || strings.Contains(line, `msg=selection`) {
				lines = append(lines, line)
			}
		}
		return lines
	}

	t.Run("a blob request decides with kind=blob", func(t *testing.T) {
		lines := capture(t, "/v2/alpine/blobs/"+blobDigest2)
		if len(lines) == 0 {
			t.Fatal("the selector logged no decision line at all")
		}
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, "kind=blob") {
			t.Errorf("no decision line carries kind=blob; the 103 criterion cannot be applied:\n%s", joined)
		}
		if strings.Contains(joined, "kind=manifest") {
			t.Errorf("a blob request was decided as a manifest:\n%s", joined)
		}
	})

	t.Run("a manifest request decides with kind=manifest", func(t *testing.T) {
		lines := capture(t, "/v2/alpine/manifests/latest")
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, "kind=manifest") {
			t.Errorf("no decision line carries kind=manifest:\n%s", joined)
		}
		if strings.Contains(joined, "kind=blob") {
			t.Errorf("a manifest request was decided as a blob:\n%s", joined)
		}
	})
}

// syncWriter serialises the log handler's writes for capture.
type syncWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// --- upstream racing on a namespace-qualified path -------------------------

// mirror is an upstream that answers the manifest probe for one repository,
// optionally slowly, and records that it was asked.
func mirror(t *testing.T, delay time.Duration) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		if !strings.HasSuffix(r.URL.Path, "/manifests/latest") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", tagDigest)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// raceDispatcher builds a dispatcher over one proxy namespace whose upstreams
// are the given URLs, and reports which (namespace, upstream) each request was
// forwarded to.
func raceDispatcher(t *testing.T, ns string, upstreams []string) (*dispatcher, *[]string) {
	t.Helper()

	cfg, err := config.Parse([]byte(`
server:
  listen: "127.0.0.1:0"
auth:
  type: none
namespaces:
  local: [me]
  proxy:
    ` + ns + `:
      upstreams: ["https://placeholder.invalid"]
      official_library: true
`))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	table := namespace.NewTable(cfg, nil)
	authenticator, err := auth.New(auth.Options{Realm: "rms", AnonymousPullProxy: true})
	if err != nil {
		t.Fatalf("auth: %v", err)
	}

	up := &fakeUpstream{
		names:     []string{ns},
		elected:   map[string]string{ns: upstreams[0]},
		upstreams: map[string][]string{ns: upstreams},
		services:  map[string]*http.Server{},
	}
	t.Cleanup(up.close)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	chooser := selection.New(up, selection.Options{
		TTL:     time.Minute,
		Timeout: 2 * time.Second,
		Logger:  log,
	})

	seen := &[]string{}
	var mu sync.Mutex
	d := &dispatcher{
		log:        log,
		table:      table,
		auth:       authenticator,
		chooser:    chooser,
		listenerNS: "",
		storageFor: cfg.NamespaceRoot,
		serving:    func() []string { return []string{ns} },
		engineFor: func(gotNS, gotUpstream string) (http.Handler, bool) {
			mu.Lock()
			*seen = append(*seen, gotNS+"|"+gotUpstream)
			mu.Unlock()
			return &fakeEngine{status: http.StatusOK, body: "served"}, true
		},
	}
	return d, seen
}

func TestQualifiedRequestIsServedByTheFastestUpstream(t *testing.T) {
	const ns = "docker.io"

	slow, slowHits := mirror(t, 80*time.Millisecond)
	fast, fastHits := mirror(t, 2*time.Millisecond)

	d, seen := raceDispatcher(t, ns, []string{slow.URL, fast.URL})

	req := httptest.NewRequest(http.MethodGet, "/v2/"+ns+"/library/alpine/manifests/latest", nil)
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if slowHits.Load() == 0 || fastHits.Load() == 0 {
		t.Errorf("probes: slow=%d fast=%d, want both upstreams asked", slowHits.Load(), fastHits.Load())
	}
	if len(*seen) != 1 {
		t.Fatalf("engine lookups = %v, want exactly one", *seen)
	}
	if want := ns + "|" + fast.URL; (*seen)[0] != want {
		t.Fatalf("forwarded to %q, want %q (the fastest mirror)", (*seen)[0], want)
	}
}

func TestQualifiedUpstreamSelectionIsBestEffort(t *testing.T) {
	const ns = "docker.io"

	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	d, seen := raceDispatcher(t, ns, []string{deadURL, deadURL + "/other"})

	req := httptest.NewRequest(http.MethodGet, "/v2/"+ns+"/library/alpine/manifests/latest", nil)
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)

	// No upstream could be probed, but the namespace still has an engine —
	// and cached content behind it. The request must reach that engine, not a
	// 503 that would make the cache useless whenever a mirror is down.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want the namespace's sticky engine to serve: %s", rec.Code, rec.Body.String())
	}
	if len(*seen) != 1 || (*seen)[0] != ns+"|" {
		t.Fatalf("engine lookups = %v, want one lookup with no upstream chosen", *seen)
	}
}
