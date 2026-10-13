package selection

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
)

// Every test here runs against httptest servers on loopback: the chooser never
// touches a real registry.

// ---------------------------------------------------------------- test doubles

// fakeUpstream is the selection.Upstream the chooser talks to. It is a plain
// map, so a test states the cluster it wants instead of building one.
type fakeUpstream struct {
	serving []string
	elected map[string]string
	// upstreams, when set, overrides the list UpstreamsFor reports for a
	// namespace. Unset means "the elected upstream alone", which is the shape
	// every fixture written before racing existed relies on.
	upstreams map[string][]string
	creds     map[string][2]string
	ewma      map[string]time.Duration
	// cached holds "namespace|repo|ref" for manifests a namespace already has
	// on local disk.
	cached map[string]bool
	// blobs holds "namespace|digest" for blob data a namespace already has on
	// local disk.
	blobs map[string]bool
	// cacheLookups counts CachedManifest calls, so a test can show the disk is
	// consulted before anything is probed.
	cacheLookups atomic.Int64
	// blobLookups counts CachedBlob calls, for the same reason.
	blobLookups atomic.Int64

	mu       sync.Mutex
	observed []observedProbe
}

// observedProbe is one ObserveContentProbe call.
type observedProbe struct {
	namespace string
	upstream  string
	answered  bool
	latency   time.Duration
	err       error
}

// observedProbes returns a copy of the content-probe verdicts the chooser
// reported back.
func (f *fakeUpstream) observedProbes() []observedProbe {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]observedProbe(nil), f.observed...)
}

// UpstreamsFor reports the namespace's upstreams: the override when a test set
// one, otherwise the elected upstream alone.
func (f *fakeUpstream) UpstreamsFor(namespace string) []string {
	if list, ok := f.upstreams[namespace]; ok {
		return append([]string(nil), list...)
	}
	if url, ok := f.elected[namespace]; ok && url != "" {
		return []string{url}
	}
	return nil
}

// ObserveContentProbe records the verdict. The chooser is the only caller, and
// what matters to these tests is which upstream was reported and whether the
// answer was a refusal or a silence.
func (f *fakeUpstream) ObserveContentProbe(namespace, upstream string, answered bool, latency time.Duration, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observed = append(f.observed, observedProbe{
		namespace: namespace, upstream: upstream, answered: answered, latency: latency, err: err,
	})
}

// cacheHit builds the key a test uses to mark a manifest as cached.
func cacheHit(namespace, repo, ref string) string {
	return namespace + "|" + repo + "|" + ref
}

// blobHit builds the key a test uses to mark a blob as cached.
func blobHit(namespace string, dgst digest.Digest) string {
	return namespace + "|" + dgst.String()
}

func (f *fakeUpstream) CachedManifest(namespace, repo, ref string) bool {
	f.cacheLookups.Add(1)
	return f.cached[cacheHit(namespace, repo, ref)]
}

func (f *fakeUpstream) CachedBlob(namespace string, dgst digest.Digest) bool {
	f.blobLookups.Add(1)
	return f.blobs[blobHit(namespace, dgst)]
}

func (f *fakeUpstream) Serving() []string {
	out := append([]string(nil), f.serving...)
	sort.Strings(out)
	return out
}

func (f *fakeUpstream) ElectedUpstream(namespace string) (string, bool) {
	url, ok := f.elected[namespace]
	return url, ok && url != ""
}

func (f *fakeUpstream) UpstreamCredentials(namespace string) (user, pass string) {
	c := f.creds[namespace]
	return c[0], c[1]
}

func (f *fakeUpstream) UpstreamLatency(namespace string) (time.Duration, bool) {
	d, ok := f.ewma[namespace]
	return d, ok
}

// fakeRegistry is one candidate's upstream: a registry that knows a fixed set
// of repositories, optionally behind a bearer challenge, optionally slow, and
// optionally unable to answer HEAD on manifests.
type fakeRegistry struct {
	*httptest.Server

	mu      sync.Mutex
	has     map[string]bool // "repo@ref"
	delay   time.Duration
	paths   []string // manifest paths served, in arrival order
	arrived []time.Time
	accepts []requestHeader

	requireToken       bool
	tokenUser, tokenPw string
	rejectHEAD         bool
	// tokenRealm overrides the realm advertised in the 401 challenge. Empty
	// means the registry's own /token endpoint.
	tokenRealm string
	// afterAuthStatus, when set, is the status returned to a request that
	// presented a valid token — how a registry that authenticates us and then
	// refuses the repository answers.
	afterAuthStatus int
	// alwaysChallenge keeps answering 401 even to a valid token, which is a
	// registry whose challenge we cannot satisfy.
	alwaysChallenge bool
	// challengeStatus wraps the credential demand in a status other than 401
	// (0 means 401), which some proxies do.
	challengeStatus int
	// requireManifestAccept makes the registry behave like one that stores OCI
	// manifests: a manifest request whose Accept does not list one is answered
	// 404, exactly as distribution's own manifest handler does
	// ("OCI manifest found, but accept header does not support OCI manifests").
	requireManifestAccept bool
	// rejectGET makes the registry refuse manifests on GET as well as HEAD.
	rejectGET bool
	// contentType overrides the media type of a successful response (empty
	// means the manifest media type a real registry sends).
	contentType string
	// digest overrides the Docker-Content-Digest a successful response
	// advertises. Empty means one fixed value, which is what makes two mirrors
	// of the same registry agree by default; a racing test that wants them to
	// disagree sets it per registry.
	digest string
	// redirectManifest, when set, is answered as a 302 for manifest requests —
	// the shape a mirror uses to send a client to its landing page.
	redirectManifest string
	// redirectBlob does the same for blob requests, which is also how a CDN is
	// reached legitimately.
	redirectBlob string
	// manifestStatus, when set, is answered for every manifest request
	// whatever the registry holds — how an upstream that is failing (500) or
	// asking us to back off (429) answers.
	manifestStatus int
	// tokenRefusal is the status its token endpoint answers with instead of
	// issuing a ticket. 403 models ghcr.io and gcr.io; 5xx models a broken
	// token service.
	tokenRefusal int

	manifests atomic.Int64
	blobs     atomic.Int64
	heads     atomic.Int64
	gets      atomic.Int64
	tokens    atomic.Int64
}

// requestHeader is what one manifest request carried, so a test can assert the
// probe negotiated content the way a real client does.
type requestHeader struct {
	method string
	accept string
}

// acceptMissingManifestType reports whether the Accept header asks for no
// manifest media type at all — the condition distribution answers 404 for on a
// repository that stores OCI manifests.
func acceptMissingManifestType(accept string) bool {
	for _, mediaType := range strings.Split(accept, ",") {
		switch strings.TrimSpace(mediaType) {
		case "application/vnd.oci.image.manifest.v1+json",
			"application/vnd.oci.image.index.v1+json",
			"application/vnd.docker.distribution.manifest.v2+json",
			"application/vnd.docker.distribution.manifest.list.v2+json":
			return false
		}
	}
	return true
}

func newFakeRegistry(t *testing.T, repos ...string) *fakeRegistry {
	t.Helper()

	f := &fakeRegistry{has: map[string]bool{}}
	for _, r := range repos {
		f.has[r] = true
	}

	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeRegistry) handle(w http.ResponseWriter, r *http.Request) {
	// The token endpoint. Its URL is derived from the request so the handler
	// never reads a field the test goroutine is writing.
	if r.URL.Path == "/token" {
		f.tokens.Add(1)
		if f.tokenRefusal != 0 {
			w.WriteHeader(f.tokenRefusal)
			return
		}
		if f.tokenUser != "" || f.tokenPw != "" {
			user, pass, ok := r.BasicAuth()
			if !ok || user != f.tokenUser || pass != f.tokenPw {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":"test-token"}`)
		return
	}

	// Record every content request, manifest or blob, before deciding what to
	// answer: a test must be able to see which path an implementation asked
	// for, since asking the wrong one is a real defect.
	repo, ref, isManifest := splitManifestPath(r.URL.Path)
	if !isManifest {
		repo, ref, isManifest = splitBlobPath(r.URL.Path)
	}
	if !isManifest {
		http.NotFound(w, r)
		return
	}

	if strings.Contains(r.URL.Path, "/manifests/") {
		f.manifests.Add(1)
	} else {
		f.blobs.Add(1)
	}
	switch r.Method {
	case http.MethodHead:
		f.heads.Add(1)
	case http.MethodGet:
		f.gets.Add(1)
	}

	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	f.arrived = append(f.arrived, time.Now())
	f.accepts = append(f.accepts, requestHeader{method: r.Method, accept: r.Header.Get("Accept")})
	delay := f.delay
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}

	if f.requireToken && (f.alwaysChallenge || r.Header.Get("Authorization") != "Bearer test-token") {
		realm := f.tokenRealm
		if realm == "" {
			realm = "http://" + r.Host + "/token"
		}
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(
			`Bearer realm="%s",service="fake-registry",scope="repository:%s:pull"`, realm, repo))
		status := f.challengeStatus
		if status == 0 {
			status = http.StatusUnauthorized
		}
		w.WriteHeader(status)
		return
	}
	if f.afterAuthStatus != 0 {
		w.WriteHeader(f.afterAuthStatus)
		return
	}
	if f.manifestStatus != 0 {
		w.WriteHeader(f.manifestStatus)
		return
	}
	redirect := ""
	if strings.Contains(r.URL.Path, "/manifests/") {
		redirect = f.redirectManifest
	} else {
		redirect = f.redirectBlob
	}
	if redirect != "" {
		w.Header().Set("Location", redirect)
		w.WriteHeader(http.StatusFound)
		return
	}
	if f.requireManifestAccept && acceptMissingManifestType(r.Header.Get("Accept")) {
		// What a registry storing OCI manifests does with a client that did
		// not ask for one. The repository is there; this is a 404 that lies.
		http.NotFound(w, r)
		return
	}
	if (f.rejectHEAD && r.Method == http.MethodHead) || (f.rejectGET && r.Method == http.MethodGet) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	f.mu.Lock()
	has := f.has[repo+"@"+ref]
	f.mu.Unlock()

	if !has {
		http.NotFound(w, r)
		return
	}

	// A registry labels the manifest it serves; the probe insists on a media
	// type that is not a web page.
	contentType := f.contentType
	if contentType == "" {
		contentType = "application/vnd.oci.image.manifest.v1+json"
	}
	w.Header().Set("Content-Type", contentType)
	digest := f.digest
	if digest == "" {
		digest = "sha256:" + strings.Repeat("a", 64)
	}
	w.Header().Set("Docker-Content-Digest", digest)
	w.WriteHeader(http.StatusOK)
}

func (f *fakeRegistry) firstArrival() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.arrived) == 0 {
		return time.Time{}
	}
	return f.arrived[0]
}

func (f *fakeRegistry) sentHeaders() []requestHeader {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]requestHeader(nil), f.accepts...)
}

func (f *fakeRegistry) probedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

// splitManifestPath pulls the repository and reference out of a manifest path,
// the way the registries under test do.
func splitManifestPath(p string) (repo, ref string, ok bool) {
	return splitRepoOp(p, "/manifests/")
}

// splitBlobPath does the same for a blob path: /v2/<repo>/blobs/<digest>.
func splitBlobPath(p string) (repo, ref string, ok bool) {
	return splitRepoOp(p, "/blobs/")
}

func splitRepoOp(p, op string) (repo, ref string, ok bool) {
	rest, ok := strings.CutPrefix(p, "/v2/")
	if !ok {
		return "", "", false
	}
	i := strings.LastIndex(rest, op)
	if i < 0 {
		return "", "", false
	}
	return rest[:i], rest[i+len(op):], true
}

// -------------------------------------------------------------------- helpers

func testClient() *http.Client { return &http.Client{Timeout: 10 * time.Second} }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// logBuffer collects JSON log records so a test can read back what the chooser
// recorded.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) records(t *testing.T) []map[string]any {
	t.Helper()

	b.mu.Lock()
	defer b.mu.Unlock()

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %v (%q)", err, line)
		}
		out = append(out, rec)
	}
	return out
}

// candidatesFor builds one candidate per namespace, all pointing at the same
// engine path, which is what a cluster of mirrors of the same registry looks
// like.
func candidatesFor(namespaces ...string) []Candidate {
	out := make([]Candidate, 0, len(namespaces))
	for _, ns := range namespaces {
		out = append(out, Candidate{Namespace: ns, EnginePath: "library/alpine"})
	}
	return out
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// ------------------------------------------------------------------ (a) and (a+)

// TestDecidePicksTheOnlyCandidateThatHasTheRepo is requirement (a): only A has
// the repository, so A wins — and B must still have been probed, otherwise the
// choice would prove nothing.
func TestDecidePicksTheOnlyCandidateThatHasTheRepo(t *testing.T) {
	a := newFakeRegistry(t, "library/alpine@latest")
	b := newFakeRegistry(t)

	c := New(&fakeUpstream{
		serving: []string{"a", "b"},
		elected: map[string]string{"a": a.URL, "b": b.URL},
	}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

	target, err := c.Decide(context.Background(), candidatesFor("a", "b"), "alpine", "latest")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if target.Namespace != "a" {
		t.Errorf("winner = %q, want %q", target.Namespace, "a")
	}
	if target.EnginePath != "library/alpine" {
		t.Errorf("EnginePath = %q, want the repository-level path %q", target.EnginePath, "library/alpine")
	}
	if target.ProbeLatency <= 0 {
		t.Errorf("ProbeLatency = %v, want the measured probe duration", target.ProbeLatency)
	}

	if got := b.manifests.Load(); got == 0 {
		t.Error("the candidate that does not have the repository was never probed")
	}
	if got := a.manifests.Load(); got != 1 {
		t.Errorf("A received %d probes, want 1", got)
	}
}

// TestDecideProbesTheCandidateEnginePath checks the contract Lead fixed: the
// probe asks each candidate for its own repository-level EnginePath, not for
// the client's repo name.
func TestDecideProbesTheCandidateEnginePath(t *testing.T) {
	a := newFakeRegistry(t, "library/alpine@latest")
	b := newFakeRegistry(t, "myorg/alpine@latest")

	c := New(&fakeUpstream{
		serving: []string{"a", "b"},
		elected: map[string]string{"a": a.URL, "b": b.URL},
	}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

	// The client asked for "alpine"; only a namespace whose official_library
	// rule turns that into library/alpine has it.
	candidates := []Candidate{
		{Namespace: "a", EnginePath: "library/alpine"},
		{Namespace: "b", EnginePath: "library/alpine"},
	}
	target, err := c.Decide(context.Background(), candidates, "alpine", "latest")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if target.Namespace != "a" {
		t.Errorf("winner = %q, want a", target.Namespace)
	}
	if got := b.probedPaths(); len(got) == 0 || got[0] != "/v2/library/alpine/manifests/latest" {
		t.Errorf("B was probed at %v, want /v2/library/alpine/manifests/latest", got)
	}
}

// ------------------------------------------------------------------ (b)

// TestDecidePicksTheFasterCandidate is requirement (b): both candidates have
// the repository, A is 200ms slower, so B wins — proving the decision is made
// on measured latency, not on the order the candidates were handed over.
func TestDecidePicksTheFasterCandidate(t *testing.T) {
	slow := newFakeRegistry(t, "library/alpine@latest")
	slow.delay = 200 * time.Millisecond
	fast := newFakeRegistry(t, "library/alpine@latest")

	// A is first in the candidate list and still must lose.
	c := New(&fakeUpstream{
		serving: []string{"a", "b"},
		elected: map[string]string{"a": slow.URL, "b": fast.URL},
	}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

	target, err := c.Decide(context.Background(), candidatesFor("a", "b"), "alpine", "latest")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if target.Namespace != "b" {
		t.Errorf("winner = %q, want the faster candidate b", target.Namespace)
	}
	if target.ProbeLatency >= 200*time.Millisecond {
		t.Errorf("winner latency = %v, want the fast candidate's own duration", target.ProbeLatency)
	}
	t.Logf("latencies: slower candidate b=%v (winner), a=%v (200ms delay)", target.ProbeLatency, slow.delay)
}

// ------------------------------------------------------------------ (c)

// TestDecideProbesCandidatesConcurrently is requirement (c). Two upstreams that
// each take 150ms are probed at the same time: the decision must cost roughly
// one probe, not two, and both upstreams must see the request within
// milliseconds of each other.
func TestDecideProbesCandidatesConcurrently(t *testing.T) {
	a := newFakeRegistry(t, "library/alpine@latest")
	a.delay = 150 * time.Millisecond
	b := newFakeRegistry(t, "library/alpine@latest")
	b.delay = 150 * time.Millisecond

	c := New(&fakeUpstream{
		serving: []string{"a", "b"},
		elected: map[string]string{"a": a.URL, "b": b.URL},
	}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

	start := time.Now()
	if _, err := c.Decide(context.Background(), candidatesFor("a", "b"), "alpine", "latest"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	elapsed := time.Since(start)

	tA, tB := a.firstArrival(), b.firstArrival()
	if tA.IsZero() || tB.IsZero() {
		t.Fatalf("both upstreams must have been asked (a=%v b=%v)", tA, tB)
	}
	skew := absDuration(tA.Sub(tB))
	if skew >= 50*time.Millisecond {
		t.Errorf("probes arrived %v apart, want < 50ms", skew)
	}
	// Serial probes would cost >= 300ms; concurrent ones cost one delay.
	if elapsed >= 250*time.Millisecond {
		t.Errorf("Decide took %v, want roughly one 150ms probe", elapsed)
	}
	t.Logf("concurrency: skew between the two upstreams = %v; total Decide = %v (two serial probes would be >= 300ms)",
		skew.Round(time.Microsecond), elapsed.Round(time.Microsecond))
}

// ------------------------------------------------------------------ (d)

// TestDecideFetchesBearerTokenAndRetries is requirement (d): a challenged
// registry is only found when the chooser performs the token handshake and
// retries once with the token. The second sub-test shows the handshake is what
// makes the difference, without it the same repository is judged absent.
func TestDecideFetchesBearerTokenAndRetries(t *testing.T) {
	challenged := newFakeRegistry(t, "library/alpine@latest")
	challenged.requireToken = true
	challenged.tokenUser, challenged.tokenPw = "robot", "s3cret"

	t.Run("with credentials", func(t *testing.T) {
		c := New(&fakeUpstream{
			serving: []string{"a"},
			elected: map[string]string{"a": challenged.URL},
			creds:   map[string][2]string{"a": {"robot", "s3cret"}},
		}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

		before := challenged.manifests.Load()
		target, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest")
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if target.Namespace != "a" {
			t.Errorf("winner = %q, want a", target.Namespace)
		}
		if got := challenged.tokens.Load(); got != 1 {
			t.Errorf("token endpoint called %d times, want 1", got)
		}
		if got := challenged.manifests.Load() - before; got != 2 {
			t.Errorf("manifest probed %d times, want 2 (challenged HEAD + retry)", got)
		}
		if got := challenged.heads.Load(); got != 2 {
			t.Errorf("HEAD requests = %d, want 2", got)
		}
		t.Logf("token flow: 1 token request, 2 manifest HEADs (401 then 200)")
	})

	// Without credentials to offer, the token endpoint turns us down with 401
	// — a refusal, which for an anonymous read-only cache means "rms cannot
	// serve this repository", so the namespace is absent rather than unknown.
	t.Run("without credentials", func(t *testing.T) {
		c := New(&fakeUpstream{
			serving: []string{"b"},
			elected: map[string]string{"b": challenged.URL},
		}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

		_, err := c.Decide(context.Background(), candidatesFor("b"), "alpine", "latest")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound: the token endpoint refused us, so this namespace cannot serve it", err)
		}
	})
}

// ------------------------------------------------------------------ (l)

// TestAuthenticatedRefusalIsAbsence is requirement (l): once the challenge is
// satisfied, the registry's refusal — 403, or 404 — is a definitive "not here",
// so it makes the namespace absent rather than unknown.
func TestAuthenticatedRefusalIsAbsence(t *testing.T) {
	newChallenged := func(t *testing.T, status int) *fakeRegistry {
		t.Helper()
		r := newFakeRegistry(t)
		r.requireToken = true
		r.afterAuthStatus = status
		return r
	}

	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		t.Run(fmt.Sprintf("authenticated %d is a definitive no", status), func(t *testing.T) {
			a := newChallenged(t, status)

			c := New(&fakeUpstream{
				serving: []string{"a"},
				elected: map[string]string{"a": a.URL},
			}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

			_, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest")
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}
			if got := a.tokens.Load(); got != 1 {
				t.Errorf("token endpoint called %d times, want 1", got)
			}
			if got := a.heads.Load(); got != 2 {
				t.Errorf("HEAD requests = %d, want 2 (challenged, then authenticated)", got)
			}
		})
	}

	t.Run("an authenticated no decides even when another candidate is down", func(t *testing.T) {
		a := newChallenged(t, http.StatusForbidden)
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		deadURL := dead.URL
		dead.Close()

		c := New(&fakeUpstream{
			serving: []string{"a", "b"},
			elected: map[string]string{"a": a.URL, "b": deadURL},
		}, Options{Client: testClient(), TTL: time.Minute, Timeout: time.Second, Logger: discardLogger()})

		_, err := c.Decide(context.Background(), candidatesFor("a", "b"), "alpine", "latest")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound: one candidate answered, so the answer is a finished 404", err)
		}
	})
}

// ------------------------------------------------------------------ (q)

// TestCredentialRefusalsAreAbsence is requirement (q), and the correction the
// 103 deployment produced: for an anonymous read-only cache, "this registry
// wants credentials it did not accept" is the registry's answer, not an outage.
// Measured on ghcr.io and gcr.io, whose token endpoints answer rms with 403.
//
// Treating these as failures made every uncached unqualified request for a
// mistyped image answer 503, because three of the five configured namespaces
// always refuse: the client should have been told 404 NAME_UNKNOWN.
func TestCredentialRefusalsAreAbsence(t *testing.T) {
	t.Run("token endpoint refuses us, and another candidate is absent too", func(t *testing.T) {
		refusing := newFakeRegistry(t)
		refusing.requireToken = true
		refusing.tokenRefusal = http.StatusForbidden // ghcr.io and gcr.io

		absent := newFakeRegistry(t) // answers 404 for everything

		c := New(&fakeUpstream{
			serving: []string{"a", "b"},
			elected: map[string]string{"a": refusing.URL, "b": absent.URL},
		}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

		_, err := c.Decide(context.Background(), candidatesFor("a", "b"), "alpine", "latest")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound: both candidates gave a definite refusal", err)
		}
		if got := refusing.tokens.Load(); got != 1 {
			t.Errorf("token endpoint called %d times, want 1", got)
		}
		t.Logf("a typo'd repository with a 403-refusing token endpoint and a 404 candidate answers 404, not 503")
	})

	t.Run("still challenged after the ticket", func(t *testing.T) {
		a := newFakeRegistry(t)
		a.requireToken = true
		a.alwaysChallenge = true // quay.io: a ticket, then 401 again

		c := New(&fakeUpstream{
			serving: []string{"a"},
			elected: map[string]string{"a": a.URL},
		}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

		_, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound: the registry refuses us even holding a ticket", err)
		}
		if got := a.tokens.Load(); got != 1 {
			t.Errorf("token endpoint called %d times, want 1", got)
		}
	})

	t.Run("a credential demand wrapped in another status is a refusal too", func(t *testing.T) {
		a := newFakeRegistry(t)
		a.requireToken = true
		// A proxy that answers 403 with a WWW-Authenticate header is refusing
		// the anonymous client, whatever the header says.
		a.challengeStatus = http.StatusForbidden

		c := New(&fakeUpstream{
			serving: []string{"a"},
			elected: map[string]string{"a": a.URL},
		}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

		_, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("challenge carries no usable realm", func(t *testing.T) {
		a := newFakeRegistry(t)
		a.requireToken = true
		a.tokenRealm = "not-a-realm"

		c := New(&fakeUpstream{
			serving: []string{"a"},
			elected: map[string]string{"a": a.URL},
		}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

		_, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound: there is no realm rms could present credentials to", err)
		}
	})

	t.Run("a 405 on both methods is a refusal, not an outage", func(t *testing.T) {
		a := newFakeRegistry(t, "library/alpine@latest")
		a.rejectHEAD = true
		a.rejectGET = true

		c := New(&fakeUpstream{
			serving: []string{"a"},
			elected: map[string]string{"a": a.URL},
		}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

		_, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}

// TestDecideMistypedImageIsNotFound reproduces the shape measured on the 103
// deployment: five configured namespaces, a repository that exists in none of
// them, and three of the five refusing an anonymous client as a matter of
// course (ghcr.io and gcr.io turn the token endpoint down with 403, quay.io
// keeps answering 401 after issuing a ticket).
//
// Before the correction this answered 503 for a typo, because the three
// refusals were counted as failures. The client must get 404 NAME_UNKNOWN.
func TestDecideMistypedImageIsNotFound(t *testing.T) {
	// docker.io: an open registry that simply has no such repository.
	docker := newFakeRegistry(t)
	// registry.k8s.io: same, 404 straight away.
	k8s := newFakeRegistry(t)

	// ghcr.io and gcr.io: the token endpoint refuses an anonymous caller.
	ghcr := newFakeRegistry(t)
	ghcr.requireToken = true
	ghcr.tokenRefusal = http.StatusForbidden
	gcr := newFakeRegistry(t)
	gcr.requireToken = true
	gcr.tokenRefusal = http.StatusForbidden

	// quay.io: a ticket is issued, and the manifest is refused anyway.
	quay := newFakeRegistry(t)
	quay.requireToken = true
	quay.alwaysChallenge = true

	namespaces := []string{"docker.io", "gcr.io", "ghcr.io", "quay.io", "registry.k8s.io"}
	elected := map[string]string{
		"docker.io":       docker.URL,
		"gcr.io":          gcr.URL,
		"ghcr.io":         ghcr.URL,
		"quay.io":         quay.URL,
		"registry.k8s.io": k8s.URL,
	}

	buf := &logBuffer{}
	c := New(&fakeUpstream{serving: namespaces, elected: elected}, Options{
		Client: testClient(),
		TTL:    time.Minute,
		Logger: slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	candidates := make([]Candidate, 0, len(namespaces))
	for _, ns := range namespaces {
		candidates = append(candidates, Candidate{Namespace: ns, EnginePath: "library/definitely-not-here-rms"})
	}

	_, err := c.Decide(context.Background(), candidates, "library/definitely-not-here-rms", "latest")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound: a mistyped image must be a 404, not a 503", err)
	}

	rec := lastSelectionRecord(t, buf)
	timings, ok := rec["timings"].([]any)
	if !ok || len(timings) != len(namespaces) {
		t.Fatalf("timings = %v, want one entry per candidate", rec["timings"])
	}
	for _, entry := range timings {
		text := fmt.Sprint(entry)
		if strings.Contains(text, "(unreachable") || strings.Contains(text, "(skipped") {
			t.Errorf("candidate %q was not answered, want all five to give a definite answer", text)
		}
	}
	t.Logf("mistyped image across 5 namespaces: %v (all absent ⇒ 404)", timings)
}

// TestDecideMistypedImageSurvivesOneNamespaceDown is the same 103 shape with a
// namespace outage on top, and it is the case the re-ruled 404/503 boundary
// exists for.
//
// Measured before the re-ruling, with two candidates: A unreachable + B 404 →
// 503; A 404 + B unreachable → 503; both unreachable → 503; A unreachable + B
// has it → 200. In other words one flaky namespace made every uncached request
// for a mistyped image answer 503, which is the same thing the user saw from the
// authentication bug — a different route to the same symptom.
//
// Now the four answering namespaces decide, and the silent one does not veto
// them. 503 is reserved for the case where nobody answered at all.
func TestDecideMistypedImageSurvivesOneNamespaceDown(t *testing.T) {
	// The four upstream shapes measured on the real registries.
	docker := newFakeRegistry(t) // open: 404 straight away

	ghcr := newFakeRegistry(t)
	ghcr.requireToken = true
	ghcr.tokenRefusal = http.StatusForbidden

	gcr := newFakeRegistry(t)
	gcr.requireToken = true
	gcr.tokenRefusal = http.StatusForbidden

	quay := newFakeRegistry(t)
	quay.requireToken = true
	quay.alwaysChallenge = true // a ticket is issued, the manifest is refused

	// registry.k8s.io is the one that is down: its address answers nothing.
	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	downURL := down.URL
	down.Close()

	namespaces := []string{"docker.io", "gcr.io", "ghcr.io", "quay.io", "registry.k8s.io"}
	candidates := make([]Candidate, 0, len(namespaces))
	for _, ns := range namespaces {
		candidates = append(candidates, Candidate{Namespace: ns, EnginePath: "library/typo"})
	}

	up := &fakeUpstream{
		serving: namespaces,
		elected: map[string]string{
			"docker.io":       docker.URL,
			"gcr.io":          gcr.URL,
			"ghcr.io":         ghcr.URL,
			"quay.io":         quay.URL,
			"registry.k8s.io": downURL,
		},
	}

	t.Run("four answer and one is down", func(t *testing.T) {
		buf := &logBuffer{}
		c := New(up, Options{
			Client:  testClient(),
			TTL:     time.Minute,
			Timeout: time.Second,
			Logger:  slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		})

		_, err := c.Decide(context.Background(), candidates, "library/typo", "latest")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound: four namespaces said no and one was silent", err)
		}

		// Exactly one candidate may be unanswered, and it must be the down one.
		rec := lastSelectionRecord(t, buf)
		timings, _ := rec["timings"].([]any)
		unreachable := 0
		for _, entry := range timings {
			if strings.Contains(fmt.Sprint(entry), "(unreachable") {
				unreachable++
			}
		}
		if unreachable != 1 {
			t.Errorf("timings = %v, want exactly one unreachable candidate", timings)
		}
		t.Logf("four answered, one down ⇒ 404: %v", timings)
	})

	t.Run("every candidate is down", func(t *testing.T) {
		// The same five namespaces with all of them unreachable: now nothing is
		// known, and that is the only case that may answer 503.
		deadUp := &fakeUpstream{
			serving: namespaces,
			elected: map[string]string{
				"docker.io":       downURL,
				"gcr.io":          downURL,
				"ghcr.io":         downURL,
				"quay.io":         downURL,
				"registry.k8s.io": downURL,
			},
		}
		c := New(deadUp, Options{Client: testClient(), TTL: time.Minute, Timeout: time.Second, Logger: discardLogger()})

		_, err := c.Decide(context.Background(), candidates, "library/typo", "latest")
		if !errors.Is(err, ErrUnreachable) {
			t.Fatalf("err = %v, want ErrUnreachable: not one candidate answered", err)
		}
	})
}

// ------------------------------------------------------------------ (s)(t)(u)

// TestServerErrorsAreNotAbsence is requirements (s) and (u): a 5xx or a 429 on
// the manifest path is the upstream failing to answer, not a statement that the
// repository is missing. Calling it "absent" would hand the client a 404 for an
// image that may well be there, and the client would stop retrying — the same
// class of mistake as reporting a 503 for a typo, in the other direction.
func TestServerErrorsAreNotAbsence(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusTooManyRequests} {
		t.Run(fmt.Sprintf("manifest answers %d", status), func(t *testing.T) {
			a := newFakeRegistry(t, "library/alpine@latest") // it *has* the repository
			a.manifestStatus = status

			c := New(&fakeUpstream{
				serving: []string{"a"},
				elected: map[string]string{"a": a.URL},
			}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

			_, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest")
			if !errors.Is(err, ErrUnreachable) {
				t.Fatalf("err = %v, want ErrUnreachable: %d is not an answer about the repository", err, status)
			}
			if errors.Is(err, ErrNotFound) {
				t.Fatalf("err = %v must not be ErrNotFound: the client would cache 'this image does not exist'", err)
			}
		})
	}

	t.Run("a 500 on the GET fallback is not an answer either", func(t *testing.T) {
		a := newFakeRegistry(t, "library/alpine@latest")
		a.rejectHEAD = true
		a.manifestStatus = http.StatusServiceUnavailable

		c := New(&fakeUpstream{
			serving: []string{"a"},
			elected: map[string]string{"a": a.URL},
		}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

		if _, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest"); !errors.Is(err, ErrUnreachable) {
			t.Fatalf("err = %v, want ErrUnreachable", err)
		}
	})
}

// TestAFailingCandidateStillLosesToAnAnswer is requirement (t): a 5xx makes its
// own candidate unanswered, but it does not erase what a different candidate
// stated. The rule decided after the 103 measurements is "nobody answered ⇒
// 503, otherwise 404", and this case has an answer in it.
//
// The reason it is safe to answer 404 here: the failing candidate could not
// have served the pull either, so the client loses nothing it could have had,
// while a 503 would send it retrying for ever.
func TestAFailingCandidateStillLosesToAnAnswer(t *testing.T) {
	failing := newFakeRegistry(t, "library/alpine@latest")
	failing.manifestStatus = http.StatusInternalServerError

	absent := newFakeRegistry(t) // answers 404 for everything

	c := New(&fakeUpstream{
		serving: []string{"a", "b"},
		elected: map[string]string{"a": failing.URL, "b": absent.URL},
	}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

	_, err := c.Decide(context.Background(), candidatesFor("a", "b"), "alpine", "latest")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound: b answered, so the decision is finished", err)
	}
}

// ------------------------------------------------------------------ (r)

// TestTransportFailuresAreNotAbsence is requirement (r), the other half of the
// corrected rule: what actually leaves the question open is the network, not a
// status. A token endpoint that cannot be reached, or that fails with 5xx, is
// no answer at all — so one such candidate still turns the whole decision into
// 503 rather than a 404 that clients would cache.
func TestTransportFailuresAreNotAbsence(t *testing.T) {
	t.Run("token endpoint unreachable", func(t *testing.T) {
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		realm := dead.URL + "/token"
		dead.Close()

		a := newFakeRegistry(t)
		a.requireToken = true
		a.tokenRealm = realm

		c := New(&fakeUpstream{
			serving: []string{"a"},
			elected: map[string]string{"a": a.URL},
		}, Options{Client: testClient(), TTL: time.Minute, Timeout: time.Second, Logger: discardLogger()})

		_, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest")
		if !errors.Is(err, ErrUnreachable) {
			t.Fatalf("err = %v, want ErrUnreachable", err)
		}
		if errors.Is(err, ErrNotFound) {
			t.Fatal("an unreachable token endpoint is not evidence that the repository is missing")
		}
	})

	t.Run("token endpoint fails with 5xx", func(t *testing.T) {
		a := newFakeRegistry(t)
		a.requireToken = true
		a.tokenRefusal = http.StatusBadGateway

		c := New(&fakeUpstream{
			serving: []string{"a"},
			elected: map[string]string{"a": a.URL},
		}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

		_, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest")
		if !errors.Is(err, ErrUnreachable) {
			t.Fatalf("err = %v, want ErrUnreachable: a broken token service is not a refusal", err)
		}
	})

	// The counterpart, re-ruled after the 103 measurements: a transport failure
	// is not an answer for *that candidate*, but it does not erase the answer
	// another candidate gave. 503 is now reserved for the case where nobody
	// answered at all — the two subtests above.
	t.Run("a failing candidate does not erase another candidate's 404", func(t *testing.T) {
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		realm := dead.URL + "/token"
		dead.Close()

		challenged := newFakeRegistry(t)
		challenged.requireToken = true
		challenged.tokenRealm = realm

		absent := newFakeRegistry(t) // answers 404 for everything

		c := New(&fakeUpstream{
			serving: []string{"a", "b"},
			elected: map[string]string{"a": challenged.URL, "b": absent.URL},
		}, Options{Client: testClient(), TTL: time.Minute, Timeout: time.Second, Logger: discardLogger()})

		_, err := c.Decide(context.Background(), candidatesFor("a", "b"), "alpine", "latest")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound: b answered, so the decision is finished even though a could not be asked", err)
		}
	})

	t.Run("nobody answers at all", func(t *testing.T) {
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		realm := dead.URL + "/token"
		dead.Close()

		one := newFakeRegistry(t)
		one.requireToken = true
		one.tokenRealm = realm

		two := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		twoURL := two.URL
		two.Close()

		c := New(&fakeUpstream{
			serving: []string{"a", "b"},
			elected: map[string]string{"a": one.URL, "b": twoURL},
		}, Options{Client: testClient(), TTL: time.Minute, Timeout: time.Second, Logger: discardLogger()})

		_, err := c.Decide(context.Background(), candidatesFor("a", "b"), "alpine", "latest")
		if !errors.Is(err, ErrUnreachable) {
			t.Fatalf("err = %v, want ErrUnreachable: not one candidate answered", err)
		}
	})
}

// TestDecideFallsBackToGETWhenHEADIsRejected covers the 405/501 degradation:
// the upstream refuses HEAD on manifests, so the probe repeats it as a GET and
// discards the body.
func TestDecideFallsBackToGETWhenHEADIsRejected(t *testing.T) {
	a := newFakeRegistry(t, "library/alpine@latest")
	a.rejectHEAD = true

	c := New(&fakeUpstream{
		serving: []string{"a"},
		elected: map[string]string{"a": a.URL},
	}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

	target, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if target.Namespace != "a" {
		t.Errorf("winner = %q, want a", target.Namespace)
	}
	if got := a.heads.Load(); got != 1 {
		t.Errorf("HEAD requests = %d, want 1", got)
	}
	if got := a.gets.Load(); got != 1 {
		t.Errorf("GET requests = %d, want exactly one degraded retry", got)
	}
}

// ------------------------------------------------- manifest content negotiation

// TestDecideSendsTheManifestAcceptHeader is the regression test for a real
// defect: the probe used to send no Accept at all, so a registry storing OCI
// manifests answered 404 for an image that exists ("OCI manifest found, but
// accept header does not support OCI manifests"), and the candidate was judged
// absent. Measured on ghcr.io for astral-sh/uv:latest: 404 without the header,
// 200 with it.
func TestDecideSendsTheManifestAcceptHeader(t *testing.T) {
	a := newFakeRegistry(t, "library/alpine@latest")
	a.requireManifestAccept = true

	c := New(&fakeUpstream{
		serving: []string{"a"},
		elected: map[string]string{"a": a.URL},
	}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

	target, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest")
	if err != nil {
		t.Fatalf("Decide: %v (a registry that stores OCI manifests must not be judged absent)", err)
	}
	if target.Namespace != "a" {
		t.Errorf("winner = %q, want a", target.Namespace)
	}

	sent := a.sentHeaders()
	if len(sent) == 0 {
		t.Fatal("the upstream saw no manifest request at all")
	}
	for _, h := range sent {
		if h.accept != manifestAccept {
			t.Errorf("%s probe sent Accept %q, want the manifest media types %q", h.method, h.accept, manifestAccept)
		}
	}

	// Control: the fake really does reject a request that negotiates nothing,
	// so this test would go red again if the header were dropped.
	t.Run("the fake rejects a request without the header", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodHead, a.URL+"/v2/library/alpine/manifests/latest", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("User-Agent", probeUserAgent)
		resp, err := testClient().Do(req)
		if err != nil {
			t.Fatalf("HEAD: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: the fake must model the registry that answers 404 without a manifest Accept", resp.StatusCode)
		}
	})
}

// TestDecideSendsTheManifestAcceptHeaderOnTheGetFallback keeps the degraded
// path negotiating identically: a 405 must not silently change what the probe
// asks for.
func TestDecideSendsTheManifestAcceptHeaderOnTheGetFallback(t *testing.T) {
	a := newFakeRegistry(t, "library/alpine@latest")
	a.requireManifestAccept = true
	a.rejectHEAD = true

	c := New(&fakeUpstream{
		serving: []string{"a"},
		elected: map[string]string{"a": a.URL},
	}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

	if _, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest"); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	sent := a.sentHeaders()
	byMethod := map[string]string{}
	for _, h := range sent {
		byMethod[h.method] = h.accept
	}
	if byMethod[http.MethodHead] != manifestAccept {
		t.Errorf("HEAD Accept = %q, want the manifest media types", byMethod[http.MethodHead])
	}
	if byMethod[http.MethodGet] != manifestAccept {
		t.Errorf("GET fallback Accept = %q, want the manifest media types", byMethod[http.MethodGet])
	}
}

// ------------------------------------------------------------------ (e)

// TestDecideCachesUntilTTL is requirement (e): a decision is reused for the TTL
// without probing again, and probed again once it expires.
func TestDecideCachesUntilTTL(t *testing.T) {
	a := newFakeRegistry(t, "library/alpine@latest")

	c := New(&fakeUpstream{
		serving: []string{"a"},
		elected: map[string]string{"a": a.URL},
	}, Options{Client: testClient(), TTL: 80 * time.Millisecond, Logger: discardLogger()})

	first, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	probes := a.manifests.Load()

	second, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest")
	if err != nil {
		t.Fatalf("Decide (cached): %v", err)
	}
	if got := a.manifests.Load(); got != probes {
		t.Errorf("a cached decision probed again: %d probes, want %d", got, probes)
	}
	if second.Namespace != first.Namespace {
		t.Errorf("cached winner = %q, want %q", second.Namespace, first.Namespace)
	}
	if second.ProbeLatency != 0 {
		t.Errorf("cached ProbeLatency = %v, want 0", second.ProbeLatency)
	}

	time.Sleep(120 * time.Millisecond)

	if _, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest"); err != nil {
		t.Fatalf("Decide (expired): %v", err)
	}
	if got := a.manifests.Load(); got <= probes {
		t.Errorf("after the TTL the decision was not re-probed: %d probes, want more than %d", got, probes)
	}
}

// TestInvalidateForcesAReprobeAndDropsAffinity covers the reselection path the
// dispatcher uses: the cached decision and the repository affinity must both go
// away, otherwise the retry would return the same failed namespace.
func TestInvalidateForcesAReprobeAndDropsAffinity(t *testing.T) {
	a := newFakeRegistry(t, "library/alpine@latest")

	c := New(&fakeUpstream{
		serving: []string{"a"},
		elected: map[string]string{"a": a.URL},
	}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

	if _, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if _, ok := c.Affinity("alpine"); !ok {
		t.Fatal("Decide must write a repository-level affinity entry for blob requests")
	}
	probes := a.manifests.Load()

	c.Invalidate("alpine", "latest")

	if _, ok := c.Affinity("alpine"); ok {
		t.Error("Invalidate must drop the repository affinity entry as well")
	}
	if _, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest"); err != nil {
		t.Fatalf("Decide after Invalidate: %v", err)
	}
	if got := a.manifests.Load(); got <= probes {
		t.Errorf("Decide after Invalidate did not re-probe: %d probes, want more than %d", got, probes)
	}
}

// TestAffinityFollowsTheManifestDecision checks the blob path: a request that
// carries no reference still resolves through the repository's decision.
func TestAffinityFollowsTheManifestDecision(t *testing.T) {
	a := newFakeRegistry(t, "library/alpine@latest")

	c := New(&fakeUpstream{
		serving: []string{"a"},
		elected: map[string]string{"a": a.URL},
	}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

	if _, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest"); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	target, ok := c.Affinity("alpine")
	if !ok {
		t.Fatal("Affinity(alpine) = none, want the manifest's namespace")
	}
	if target.Namespace != "a" || target.EnginePath != "library/alpine" {
		t.Errorf("Affinity = %+v, want namespace a and the repository-level path", target)
	}
	if target.ProbeLatency != 0 {
		t.Errorf("affinity ProbeLatency = %v, want 0", target.ProbeLatency)
	}
	if _, ok := c.Affinity("unknown"); ok {
		t.Error("Affinity for a repository that was never decided must be absent")
	}
}

// ------------------------------------------------------------------ (f)

// TestDecideDistinguishesNotFoundFromUnreachable is requirement (f): the two
// failures are different answers and must not be confused.
func TestDecideDistinguishesNotFoundFromUnreachable(t *testing.T) {
	t.Run("all candidates answer 404", func(t *testing.T) {
		a := newFakeRegistry(t)
		b := newFakeRegistry(t)

		c := New(&fakeUpstream{
			serving: []string{"a", "b"},
			elected: map[string]string{"a": a.URL, "b": b.URL},
		}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

		_, err := c.Decide(context.Background(), candidatesFor("a", "b"), "alpine", "latest")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
		if errors.Is(err, ErrUnreachable) {
			t.Fatal("ErrNotFound must not also be ErrUnreachable")
		}
	})

	t.Run("every candidate is unreachable", func(t *testing.T) {
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := dead.URL
		dead.Close() // nothing is listening on that port any more

		c := New(&fakeUpstream{
			serving: []string{"a", "b"},
			elected: map[string]string{"a": url, "b": url},
		}, Options{Client: testClient(), TTL: time.Minute, Timeout: time.Second, Logger: discardLogger()})

		_, err := c.Decide(context.Background(), candidatesFor("a", "b"), "alpine", "latest")
		if !errors.Is(err, ErrUnreachable) {
			t.Fatalf("err = %v, want ErrUnreachable", err)
		}
	})

	// Requirement (k), as re-ruled after the 103 measurements: a silent
	// candidate no longer overrides one that answered. The 404 costs only the
	// case where the silent namespace was the one holding the content, which
	// was unreachable anyway; the 503 it replaces costs every uncached request
	// a retry loop for ever, and does so whenever any single namespace is
	// flaky.
	t.Run("one candidate answers 404 and one is unreachable", func(t *testing.T) {
		answers := newFakeRegistry(t)
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		deadURL := dead.URL
		dead.Close()

		c := New(&fakeUpstream{
			serving: []string{"a", "b"},
			elected: map[string]string{"a": answers.URL, "b": deadURL},
		}, Options{Client: testClient(), TTL: time.Minute, Timeout: time.Second, Logger: discardLogger()})

		_, err := c.Decide(context.Background(), candidatesFor("a", "b"), "alpine", "latest")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound: a said no, and b's silence is not evidence that b has it", err)
		}
	})

	t.Run("one candidate answers 404 and one has no upstream", func(t *testing.T) {
		answers := newFakeRegistry(t)

		c := New(&fakeUpstream{
			serving: []string{"a", "cache-only"},
			elected: map[string]string{"a": answers.URL},
		}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

		_, err := c.Decide(context.Background(), candidatesFor("a", "cache-only"), "alpine", "latest")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound: a answered, and the cache-only candidate had nothing to add", err)
		}
	})

	t.Run("no candidates", func(t *testing.T) {
		c := New(&fakeUpstream{}, Options{Client: testClient(), Logger: discardLogger()})
		if _, err := c.Decide(context.Background(), nil, "alpine", "latest"); !errors.Is(err, ErrUnreachable) {
			t.Fatalf("err = %v, want ErrUnreachable", err)
		}
	})

	t.Run("candidate with no elected upstream", func(t *testing.T) {
		// A namespace serving cached content only has no upstream to probe.
		c := New(&fakeUpstream{serving: []string{"a"}}, Options{Client: testClient(), Logger: discardLogger()})
		if _, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest"); !errors.Is(err, ErrUnreachable) {
			t.Fatalf("err = %v, want ErrUnreachable", err)
		}
	})
}

// ------------------------------------------------------------------ (g)

// TestDecideRecordsProbeTimings is requirement (g): the per-candidate timings
// reach the decision log, which is what the measurement report is written from.
func TestDecideRecordsProbeTimings(t *testing.T) {
	fast := newFakeRegistry(t, "library/alpine@latest")
	fast.delay = 10 * time.Millisecond
	mid := newFakeRegistry(t, "library/alpine@latest")
	mid.delay = 30 * time.Millisecond
	slow := newFakeRegistry(t, "library/alpine@latest")
	slow.delay = 60 * time.Millisecond

	buf := &logBuffer{}
	c := New(&fakeUpstream{
		serving: []string{"fast", "mid", "slow"},
		elected: map[string]string{"fast": fast.URL, "mid": mid.URL, "slow": slow.URL},
	}, Options{
		Client: testClient(),
		TTL:    time.Minute,
		Logger: slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	target, err := c.Decide(context.Background(), candidatesFor("fast", "mid", "slow"), "alpine", "latest")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if target.Namespace != "fast" {
		t.Errorf("winner = %q, want the 10ms candidate", target.Namespace)
	}

	var decision map[string]any
	var probeLines []map[string]any
	for _, rec := range buf.records(t) {
		switch rec["msg"] {
		case "selection":
			decision = rec
		case "selection probe":
			probeLines = append(probeLines, rec)
		}
	}
	if decision == nil {
		t.Fatal("no \"selection\" decision record was logged")
	}
	if decision["cached"] != false {
		t.Errorf("cached = %v, want false", decision["cached"])
	}
	if got := decision["candidates"]; got != float64(3) {
		t.Errorf("candidates = %v, want 3", got)
	}

	rawTimings, ok := decision["timings"].([]any)
	if !ok || len(rawTimings) != 3 {
		t.Fatalf("timings = %v, want one entry per candidate", decision["timings"])
	}
	timings := make([]string, 0, len(rawTimings))
	for _, v := range rawTimings {
		timings = append(timings, fmt.Sprint(v))
	}

	if len(probeLines) != 3 {
		t.Errorf("logged %d per-probe records, want 3", len(probeLines))
	}
	for _, rec := range probeLines {
		if _, ok := rec["started_at"]; !ok {
			t.Errorf("probe record for %v has no started_at", rec["namespace"])
		}
	}

	t.Logf("timings recorded: %v", timings)
	t.Logf("winner latency: %v", target.ProbeLatency.Round(time.Microsecond))
}

// TestDecideLogsACacheHit checks the other half of the decision line: a cached
// decision is reported as cached and carries no timings.
func TestDecideLogsACacheHit(t *testing.T) {
	a := newFakeRegistry(t, "library/alpine@latest")

	buf := &logBuffer{}
	c := New(&fakeUpstream{
		serving: []string{"a"},
		elected: map[string]string{"a": a.URL},
	}, Options{
		Client: testClient(),
		TTL:    time.Minute,
		Logger: slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	for i := 0; i < 2; i++ {
		if _, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest"); err != nil {
			t.Fatalf("Decide: %v", err)
		}
	}

	var decisions []map[string]any
	for _, rec := range buf.records(t) {
		if rec["msg"] == "selection" {
			decisions = append(decisions, rec)
		}
	}
	if len(decisions) != 2 {
		t.Fatalf("logged %d decision records, want 2", len(decisions))
	}
	if decisions[0]["cached"] != false {
		t.Errorf("first decision cached = %v, want false", decisions[0]["cached"])
	}
	if decisions[1]["cached"] != true {
		t.Errorf("second decision cached = %v, want true", decisions[1]["cached"])
	}
	if _, ok := decisions[1]["timings"]; ok {
		t.Error("a cached decision must not carry timings")
	}
}

// ------------------------------------------------- (h)(i)(j): the local cache

// TestDecidePrefersACachedManifestOverAnyProbe is requirement (h): a namespace
// that already holds the reference wins, even when its upstream is dead and a
// healthy candidate is standing right next to it — a cache hit is proof of
// existence and costs one stat.
func TestDecidePrefersACachedManifestOverAnyProbe(t *testing.T) {
	t.Run("a dead upstream is rescued by the cache", func(t *testing.T) {
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		deadURL := dead.URL
		dead.Close()

		// b is healthy and answers, but does not have the repository, so
		// without the cache the decision would be ErrUnreachable (a is
		// unknown) rather than a winner.
		other := newFakeRegistry(t)

		up := &fakeUpstream{
			serving: []string{"a", "b"},
			elected: map[string]string{"a": deadURL, "b": other.URL},
			cached:  map[string]bool{cacheHit("a", "library/alpine", "latest"): true},
		}
		c := New(up, Options{Client: testClient(), TTL: time.Minute, Timeout: time.Second, Logger: discardLogger()})

		target, err := c.Decide(context.Background(), candidatesFor("a", "b"), "alpine", "latest")
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if target.Namespace != "a" {
			t.Errorf("winner = %q, want the namespace that has it cached (a)", target.Namespace)
		}
		if target.ProbeLatency != 0 {
			t.Errorf("ProbeLatency = %v, want 0: nothing was probed", target.ProbeLatency)
		}
		if got := other.manifests.Load(); got != 0 {
			t.Errorf("the healthy candidate was probed %d times, want 0", got)
		}
		t.Logf("cache hit decided the request with %d disk lookups and 0 probes", up.cacheLookups.Load())
	})

	t.Run("no probe is sent when the cache answers", func(t *testing.T) {
		a := newFakeRegistry(t, "library/alpine@latest")
		b := newFakeRegistry(t, "library/alpine@latest")
		other := newFakeRegistry(t, "library/alpine@latest")

		up := &fakeUpstream{
			serving: []string{"a", "b", "c"},
			elected: map[string]string{"a": a.URL, "b": b.URL, "c": other.URL},
			cached:  map[string]bool{cacheHit("b", "library/alpine", "latest"): true},
		}
		c := New(up, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

		target, err := c.Decide(context.Background(), candidatesFor("a", "b", "c"), "alpine", "latest")
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		// Requirement (j): a cache hit means every upstream hit count stays 0.
		for name, reg := range map[string]*fakeRegistry{"a": a, "b": b, "c": other} {
			if got := reg.manifests.Load(); got != 0 {
				t.Errorf("upstream %s received %d probes, want 0", name, got)
			}
		}
		if target.Namespace != "b" {
			t.Errorf("winner = %q, want b (the only cached candidate)", target.Namespace)
		}
		t.Logf("cache hit with 3 candidates: probes a=0 b=0 c=0, %d disk lookups", up.cacheLookups.Load())
	})
}

// TestDecideTakesTheNameOrderAmongCacheHits is requirement (i): with no probe
// there is no measured latency, so several cache hits resolve by namespace name
// and the outcome cannot depend on map iteration order.
func TestDecideTakesTheNameOrderAmongCacheHits(t *testing.T) {
	a := newFakeRegistry(t, "library/alpine@latest")
	b := newFakeRegistry(t, "library/alpine@latest")

	up := &fakeUpstream{
		serving: []string{"bbb", "aaa"},
		elected: map[string]string{"aaa": a.URL, "bbb": b.URL},
		cached: map[string]bool{
			cacheHit("aaa", "library/alpine", "latest"): true,
			cacheHit("bbb", "library/alpine", "latest"): true,
		},
	}
	c := New(up, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

	// The candidate list is handed over in the order the dispatcher would use,
	// which is already name-sorted; the chooser must not depend on that.
	candidates := []Candidate{
		{Namespace: "bbb", EnginePath: "library/alpine"},
		{Namespace: "aaa", EnginePath: "library/alpine"},
	}

	for i := 0; i < 20; i++ {
		c.Invalidate("alpine", "latest")
		target, err := c.Decide(context.Background(), candidates, "alpine", "latest")
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if target.Namespace != "aaa" {
			t.Fatalf("winner = %q, want aaa (the earlier name)", target.Namespace)
		}
	}
	if a.manifests.Load()+b.manifests.Load() != 0 {
		t.Error("cached candidates must not be probed")
	}
}

// TestDecideIgnoresTheCacheForAnotherReference keeps the cache check honest:
// only the exact (repo, ref) that is on disk counts.
func TestDecideIgnoresTheCacheForAnotherReference(t *testing.T) {
	a := newFakeRegistry(t, "library/alpine@v3.20")

	up := &fakeUpstream{
		serving: []string{"a"},
		elected: map[string]string{"a": a.URL},
		cached:  map[string]bool{cacheHit("a", "library/alpine", "v3.19"): true},
	}
	c := New(up, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

	target, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "v3.20")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if target.Namespace != "a" {
		t.Errorf("winner = %q, want a", target.Namespace)
	}
	if got := a.manifests.Load(); got != 1 {
		t.Errorf("probes = %d, want 1: v3.19 being cached must not decide v3.20", got)
	}
}

// ------------------------------------------------------- (n)(o)(p): blob routing

// TestDecideBlobFollowsTheManifestDecision is requirement (n): a blob request
// carries only a digest, so it follows the namespace that just served the
// manifest — and it does so without probing anything.
func TestDecideBlobFollowsTheManifestDecision(t *testing.T) {
	a := newFakeRegistry(t, "library/alpine@latest")

	up := &fakeUpstream{
		serving: []string{"a"},
		elected: map[string]string{"a": a.URL},
	}
	c := New(up, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

	manifest, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	probes := a.manifests.Load()

	const configDigest = "sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6"
	target, err := c.DecideBlob(configDigest)
	if err != nil {
		t.Fatalf("DecideBlob: %v", err)
	}
	if target.Namespace != manifest.Namespace {
		t.Errorf("blob namespace = %q, want the manifest's %q", target.Namespace, manifest.Namespace)
	}
	if got := a.manifests.Load(); got != probes {
		t.Errorf("DecideBlob probed the upstream: %d manifest requests, want %d", got, probes)
	}
	if up.blobLookups.Load() != 0 {
		t.Errorf("DecideBlob fell through to the blob store %d times, want 0: affinity answered first", up.blobLookups.Load())
	}
	t.Logf("blob followed the manifest decision to %s with 0 probes", target.Namespace)
}

// TestDecideBlobRefusesToGuessARepository is requirement (o) as ruled: a digest
// that is on some namespace's disk but has no affinity behind it is *not*
// routed, because a blob can only be addressed inside a namespace as
// /v2/<repo>/blobs/<digest> and the on-disk data records no repository. Naming
// one would fetch the blob through another repository's path, so the answer is
// 503 — and the log says which namespace held it, so the refusal is legible.
func TestDecideBlobRefusesToGuessARepository(t *testing.T) {
	dgst, err := digest.Parse("sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6")
	if err != nil {
		t.Fatalf("parse digest: %v", err)
	}

	t.Run("the digest is on disk and still not routed", func(t *testing.T) {
		a := newFakeRegistry(t)
		b := newFakeRegistry(t)

		up := &fakeUpstream{
			serving: []string{"a", "b"},
			elected: map[string]string{"a": a.URL, "b": b.URL},
			blobs:   map[string]bool{blobHit("b", dgst): true},
		}
		buf := &logBuffer{}
		c := New(up, Options{
			Client: testClient(),
			TTL:    time.Minute,
			Logger: slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		})

		if _, err := c.DecideBlob(dgst.String()); !errors.Is(err, ErrUnreachable) {
			t.Fatalf("err = %v, want ErrUnreachable: a blob with no repository cannot be addressed", err)
		}
		if errors.Is(err, ErrNotFound) {
			t.Fatal("refusing to guess a repository is not evidence that the blob is missing")
		}
		if a.manifests.Load()+b.manifests.Load() != 0 {
			t.Error("DecideBlob must not probe an upstream")
		}

		rec := lastSelectionRecord(t, buf)
		if rec["source"] != sourceBlobDisk {
			t.Errorf("source = %v, want %q", rec["source"], sourceBlobDisk)
		}
		if rec["winner"] != "b" {
			t.Errorf("winner = %v, want b: the log must name the namespace that held the digest", rec["winner"])
		}
		if rec["repo"] != "" {
			t.Errorf("repo = %v, want empty: the repository is exactly what is unknown", rec["repo"])
		}
		t.Logf("disk hit without affinity: namespace b held the digest, no repository to address it by, refused with 503")
	})

	t.Run("no namespace has it either", func(t *testing.T) {
		up := &fakeUpstream{
			serving: []string{"bbb", "aaa"},
			elected: map[string]string{"aaa": "http://127.0.0.1:1", "bbb": "http://127.0.0.1:2"},
		}
		buf := &logBuffer{}
		c := New(up, Options{
			Client: testClient(),
			TTL:    time.Minute,
			Logger: slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		})

		if _, err := c.DecideBlob(dgst.String()); !errors.Is(err, ErrUnreachable) {
			t.Fatalf("err = %v, want ErrUnreachable", err)
		}
		if rec := lastSelectionRecord(t, buf); rec["source"] != sourceUnknown {
			t.Errorf("source = %v, want %q", rec["source"], sourceUnknown)
		}
	})
}

// TestDecideBlobLogsTheAffinitySourceAndRepository covers the operational half
// of the ruling: the normal path must be identifiable in the log, together with
// the repository the blob was addressed by, so that "the blob followed the
// manifest into the same namespace" can be shown from a production log.
func TestDecideBlobLogsTheAffinitySourceAndRepository(t *testing.T) {
	a := newFakeRegistry(t, "library/alpine@latest")

	buf := &logBuffer{}
	c := New(&fakeUpstream{
		serving: []string{"a"},
		elected: map[string]string{"a": a.URL},
	}, Options{
		Client: testClient(),
		TTL:    time.Minute,
		Logger: slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	if _, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if _, err := c.DecideBlob("sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6"); err != nil {
		t.Fatalf("DecideBlob: %v", err)
	}

	rec := lastSelectionRecord(t, buf)
	if rec["source"] != sourceAffinity {
		t.Errorf("source = %v, want %q", rec["source"], sourceAffinity)
	}
	if rec["winner"] != "a" {
		t.Errorf("winner = %v, want a", rec["winner"])
	}
	// The repository is what makes the returned target addressable.
	if rec["repo"] != "library/alpine" {
		t.Errorf("repo = %v, want the repository-level path library/alpine", rec["repo"])
	}
}

// lastSelectionRecord returns the most recent "selection" decision line.
func lastSelectionRecord(t *testing.T, buf *logBuffer) map[string]any {
	t.Helper()

	var last map[string]any
	for _, rec := range buf.records(t) {
		if rec["msg"] == "selection" {
			last = rec
		}
	}
	if last == nil {
		t.Fatal("no \"selection\" decision record was logged")
	}
	return last
}

// TestDecideBlobNeverReportsNotFound is requirement (p): when neither the
// affinity nor any disk holds the digest, the answer is "I do not know" — 503 —
// and never a 404 that a client would cache.
func TestDecideBlobNeverReportsNotFound(t *testing.T) {
	const dgst = "sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6"

	t.Run("nothing resolves it", func(t *testing.T) {
		up := &fakeUpstream{
			serving: []string{"a", "b"},
			elected: map[string]string{"a": "http://127.0.0.1:1", "b": "http://127.0.0.1:2"},
		}
		c := New(up, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

		_, err := c.DecideBlob(dgst)
		if !errors.Is(err, ErrUnreachable) {
			t.Fatalf("err = %v, want ErrUnreachable", err)
		}
		if errors.Is(err, ErrNotFound) {
			t.Fatal("a digest no namespace is known to hold is not evidence that it is missing")
		}
	})

	t.Run("no namespaces are serving", func(t *testing.T) {
		c := New(&fakeUpstream{}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})
		if _, err := c.DecideBlob(dgst); !errors.Is(err, ErrUnreachable) {
			t.Fatalf("err = %v, want ErrUnreachable", err)
		}
	})

	t.Run("an unparsable digest is not looked up", func(t *testing.T) {
		up := &fakeUpstream{serving: []string{"a"}, blobs: map[string]bool{}}
		c := New(up, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

		if _, err := c.DecideBlob("not-a-digest"); !errors.Is(err, ErrUnreachable) {
			t.Fatalf("err = %v, want ErrUnreachable", err)
		}
		if up.blobLookups.Load() != 0 {
			t.Error("an unparsable digest must not reach the blob store")
		}
	})

	t.Run("a failed pull drops the affinity it followed", func(t *testing.T) {
		a := newFakeRegistry(t, "library/alpine@latest")
		up := &fakeUpstream{serving: []string{"a"}, elected: map[string]string{"a": a.URL}}
		c := New(up, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

		if _, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest"); err != nil {
			t.Fatalf("Decide: %v", err)
		}
		c.Invalidate("alpine", "latest")

		// The most-recent-manifest pointer must go with the affinity entry,
		// otherwise blobs would keep being sent to the namespace that just
		// failed.
		if _, err := c.DecideBlob(dgst); !errors.Is(err, ErrUnreachable) {
			t.Fatalf("err = %v, want ErrUnreachable: the invalidated affinity must not answer", err)
		}
	})
}

// ------------------------------------------------- (v)(w)(x): blob paths in a repo

// layerDigest is a layer blob, deliberately *not* a manifest reference.
const layerDigest = "sha256:034d6572bf28e5b7a4b1a2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f607"

// TestDecideBlobInRepoAsksTheBlobPath is requirement (v), and the regression
// test for the defect the 103 deployment exposed: a client fetching a layer at
// /v2/<repo>/blobs/<digest> got a 404 because the probe asked the *manifest*
// path with the blob's digest as the reference. Every candidate then answered
// "(absent)" — truthfully, since no manifest has that digest — for a blob that
// was sitting right there.
func TestDecideBlobInRepoAsksTheBlobPath(t *testing.T) {
	has := newFakeRegistry(t, "library/busybox@"+layerDigest)
	hasNot := newFakeRegistry(t)

	c := New(&fakeUpstream{
		serving: []string{"a", "b"},
		elected: map[string]string{"a": has.URL, "b": hasNot.URL},
	}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

	candidates := []Candidate{
		{Namespace: "a", EnginePath: "library/busybox"},
		{Namespace: "b", EnginePath: "library/busybox"},
	}

	target, err := c.DecideBlobInRepo(context.Background(), candidates, "library/busybox", layerDigest)
	if err != nil {
		t.Fatalf("DecideBlobInRepo: %v", err)
	}
	if target.Namespace != "a" {
		t.Errorf("winner = %q, want a (the namespace that has the blob)", target.Namespace)
	}

	wantPath := "/v2/library/busybox/blobs/" + layerDigest
	for name, reg := range map[string]*fakeRegistry{"a": has, "b": hasNot} {
		paths := reg.probedPaths()
		if len(paths) == 0 {
			t.Fatalf("namespace %s was never asked", name)
		}
		for _, p := range paths {
			if strings.Contains(p, "/manifests/") {
				t.Errorf("namespace %s was asked the manifest path %q: a layer digest is not a manifest reference", name, p)
			}
			if p != wantPath {
				t.Errorf("namespace %s was asked %q, want %q", name, p, wantPath)
			}
		}
	}
	t.Logf("blob probe path: %q (both candidates), winner a", wantPath)
}

// TestDecideBlobInRepoAbsentEverywhereIsNotFound is requirement (w).
func TestDecideBlobInRepoAbsentEverywhereIsNotFound(t *testing.T) {
	a := newFakeRegistry(t)
	b := newFakeRegistry(t)

	c := New(&fakeUpstream{
		serving: []string{"a", "b"},
		elected: map[string]string{"a": a.URL, "b": b.URL},
	}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

	candidates := []Candidate{
		{Namespace: "a", EnginePath: "library/busybox"},
		{Namespace: "b", EnginePath: "library/busybox"},
	}
	_, err := c.DecideBlobInRepo(context.Background(), candidates, "library/busybox", layerDigest)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound: every candidate answered", err)
	}
}

// TestDecideBlobInRepoCachesTheDecision is requirement (x): the blob decision
// is cached for the probe TTL like any other, so a client fetching several
// blobs of one image does not re-probe each time.
func TestDecideBlobInRepoCachesTheDecision(t *testing.T) {
	a := newFakeRegistry(t, "library/busybox@"+layerDigest)

	c := New(&fakeUpstream{
		serving: []string{"a"},
		elected: map[string]string{"a": a.URL},
	}, Options{Client: testClient(), TTL: 80 * time.Millisecond, Logger: discardLogger()})

	candidates := []Candidate{{Namespace: "a", EnginePath: "library/busybox"}}

	first, err := c.DecideBlobInRepo(context.Background(), candidates, "library/busybox", layerDigest)
	if err != nil {
		t.Fatalf("DecideBlobInRepo: %v", err)
	}
	probes := a.blobs.Load()

	second, err := c.DecideBlobInRepo(context.Background(), candidates, "library/busybox", layerDigest)
	if err != nil {
		t.Fatalf("DecideBlobInRepo (cached): %v", err)
	}
	if got := a.blobs.Load(); got != probes {
		t.Errorf("a cached blob decision probed again: %d blob requests, want %d", got, probes)
	}
	if second.Namespace != first.Namespace {
		t.Errorf("cached winner = %q, want %q", second.Namespace, first.Namespace)
	}
	if second.ProbeLatency != 0 {
		t.Errorf("cached ProbeLatency = %v, want 0", second.ProbeLatency)
	}

	time.Sleep(120 * time.Millisecond)
	if _, err := c.DecideBlobInRepo(context.Background(), candidates, "library/busybox", layerDigest); err != nil {
		t.Fatalf("DecideBlobInRepo (expired): %v", err)
	}
	if got := a.blobs.Load(); got <= probes {
		t.Errorf("after the TTL the blob decision was not re-probed: %d requests, want more than %d", got, probes)
	}
}

// TestBlobAndManifestDecisionsDoNotShareACacheKey guards the cache key: a blob
// digest and a manifest reference can be the same string, and one must not
// answer for the other. Here the same digest is both a manifest reference and a
// blob digest; the manifest decision must not be reused for the blob.
func TestBlobAndManifestDecisionsDoNotShareACacheKey(t *testing.T) {
	a := newFakeRegistry(t, "library/busybox@"+layerDigest)

	c := New(&fakeUpstream{
		serving: []string{"a"},
		elected: map[string]string{"a": a.URL},
	}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

	candidates := []Candidate{{Namespace: "a", EnginePath: "library/busybox"}}

	// A manifest *reference* that happens to be that digest.
	if _, err := c.Decide(context.Background(), candidates, "library/busybox", layerDigest); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	manifestProbes := a.manifests.Load()

	if _, err := c.DecideBlobInRepo(context.Background(), candidates, "library/busybox", layerDigest); err != nil {
		t.Fatalf("DecideBlobInRepo: %v", err)
	}
	if got := a.blobs.Load(); got == 0 {
		t.Error("the blob decision reused the manifest decision: it must probe the blob path")
	}
	if got := a.manifests.Load(); got != manifestProbes {
		t.Errorf("the blob decision asked the manifest path again: %d manifest requests, want %d", got, manifestProbes)
	}
}

// ------------------------------------------------- (y)(z)(aa): 2xx is not proof

// TestARedirectToAWebPageIsNotAManifest is requirement (y) and the regression
// test for the defect 103 exposed: dhi.m.daocloud.io answers an unknown
// repository with 302 to https://github.com/DaoCloud/public-image-mirror, an
// HTML page. Followed, that arrived as a 200, the probe called it a manifest,
// the candidate won, and the client got a 404 — for a repository the mirror
// could never have served, potentially shadowing a namespace that had it.
func TestARedirectToAWebPageIsNotAManifest(t *testing.T) {
	// Where the mirror sends us: a web page, served with 200.
	var landingHits atomic.Int64
	landing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		landingHits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<!doctype html><title>DaoCloud public image mirror</title>")
	}))
	t.Cleanup(landing.Close)

	mirror := newFakeRegistry(t)
	mirror.redirectManifest = landing.URL

	c := New(&fakeUpstream{
		serving: []string{"dhi.io", "docker.io"},
		elected: map[string]string{"dhi.io": mirror.URL, "docker.io": mirror.URL},
	}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

	target, err := c.Decide(context.Background(), candidatesFor("dhi.io", "docker.io"), "typo", "latest")
	if err == nil {
		t.Fatalf("Decide returned target %+v and no error: an HTML landing page must not count as a manifest", target)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound: an HTML page is a refusal, not an outage", err)
	}
	t.Logf("redirect to an HTML page: err=%v, landing page fetched %d times", err, landingHits.Load())
}

// TestHTMLWith200IsNotAManifest is requirement (z): the same shape without a
// redirect — a 200 that is a web page.
func TestHTMLWith200IsNotAManifest(t *testing.T) {
	a := newFakeRegistry(t, "library/alpine@latest")
	a.contentType = "text/html; charset=utf-8"

	c := New(&fakeUpstream{
		serving: []string{"a"},
		elected: map[string]string{"a": a.URL},
	}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

	_, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound: a 200 text/html is not a manifest", err)
	}
}

// TestARealManifestMediaTypeStillCounts is requirement (aa), the guard against
// over-fixing: every media type a registry actually serves a manifest with must
// still count as found.
func TestARealManifestMediaTypeStillCounts(t *testing.T) {
	for _, ct := range []string{
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json; charset=utf-8",
		"application/json",
		"",                        // a HEAD is not required to carry one
		"not a media type at all", // unparsable: still no evidence against it
	} {
		name := ct
		if name == "" {
			name = "no-content-type"
		}
		t.Run(name, func(t *testing.T) {
			a := newFakeRegistry(t, "library/alpine@latest")
			a.contentType = ct

			c := New(&fakeUpstream{
				serving: []string{"a"},
				elected: map[string]string{"a": a.URL},
			}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

			target, err := c.Decide(context.Background(), candidatesFor("a"), "alpine", "latest")
			if err != nil {
				t.Fatalf("Decide: %v (a registry serving %q must count as found)", err, ct)
			}
			if target.Namespace != "a" {
				t.Errorf("winner = %q, want a", target.Namespace)
			}
		})
	}
}

// TestBlobRedirectIsJudgedByWhatItDelivers covers the blob half of the rule: a
// redirect on a blob path is normal — it is how a CDN serves the bytes — so the
// probe follows it and judges the destination by its content type. A CDN
// handing over bytes counts; a landing page does not.
func TestBlobRedirectIsJudgedByWhatItDelivers(t *testing.T) {
	t.Run("a redirect to the bytes counts", func(t *testing.T) {
		cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(cdn.Close)

		mirror := newFakeRegistry(t)
		mirror.redirectBlob = cdn.URL

		c := New(&fakeUpstream{
			serving: []string{"a"},
			elected: map[string]string{"a": mirror.URL},
		}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

		target, err := c.DecideBlobInRepo(context.Background(),
			[]Candidate{{Namespace: "a", EnginePath: "library/busybox"}}, "library/busybox", layerDigest)
		if err != nil {
			t.Fatalf("DecideBlobInRepo: %v (a CDN redirect is how blobs are served)", err)
		}
		if target.Namespace != "a" {
			t.Errorf("winner = %q, want a", target.Namespace)
		}
	})

	t.Run("a redirect to a landing page does not", func(t *testing.T) {
		landing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(landing.Close)

		mirror := newFakeRegistry(t)
		mirror.redirectBlob = landing.URL

		c := New(&fakeUpstream{
			serving: []string{"a"},
			elected: map[string]string{"a": mirror.URL},
		}, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

		_, err := c.DecideBlobInRepo(context.Background(),
			[]Candidate{{Namespace: "a", EnginePath: "library/busybox"}}, "library/busybox", layerDigest)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound: a landing page is not blob bytes", err)
		}
	})
}

// TestDecisionLogNamesWhyACandidateLost keeps the diagnostic honest: the
// decision line must say which candidate was refused for which reason, since
// that is what makes "why is ghcr.io unreachable here" answerable from a
// production log instead of by guesswork.
func TestDecisionLogNamesWhyACandidateLost(t *testing.T) {
	html := newFakeRegistry(t, "library/alpine@latest")
	html.contentType = "text/html; charset=utf-8"

	refusing := newFakeRegistry(t)
	refusing.requireToken = true
	refusing.tokenRefusal = http.StatusForbidden

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	buf := &logBuffer{}
	c := New(&fakeUpstream{
		serving: []string{"html", "refusing", "dead"},
		elected: map[string]string{"html": html.URL, "refusing": refusing.URL, "dead": deadURL},
	}, Options{
		Client:  testClient(),
		TTL:     time.Minute,
		Timeout: time.Second,
		Logger:  slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	_, err := c.Decide(context.Background(), candidatesFor("html", "refusing", "dead"), "alpine", "latest")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}

	timings, _ := lastSelectionRecord(t, buf)["timings"].([]any)
	joined := fmt.Sprint(timings)
	for _, want := range []string{"not-manifest-content:text/html", "auth-refused", "unreachable:transport"} {
		if !strings.Contains(joined, want) {
			t.Errorf("timings = %v, want it to contain %q", timings, want)
		}
	}
	t.Logf("timings with reasons: %v", timings)
}

// TestProbeDebugLineCarriesThePath pins the diagnostic the dispatcher side relies
// on: the decision line shows the outcome and its reason, and the per-probe
// debug line shows the exact path that was asked. "Which path did the probe ask
// for" is the question behind a whole class of defects here (a manifest
// reference asked on the manifests path when a blob was meant, and vice versa),
// and it used to be answerable only from the upstream's own logs.
func TestProbeDebugLineCarriesThePath(t *testing.T) {
	a := newFakeRegistry(t, "library/alpine@latest", "library/busybox@"+layerDigest)

	buf := &logBuffer{}
	c := New(&fakeUpstream{
		serving: []string{"a"},
		elected: map[string]string{"a": a.URL},
	}, Options{
		Client: testClient(),
		TTL:    time.Minute,
		Logger: slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	if _, err := c.Decide(context.Background(),
		[]Candidate{{Namespace: "a", EnginePath: "library/alpine"}}, "alpine", "latest"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if _, err := c.DecideBlobInRepo(context.Background(),
		[]Candidate{{Namespace: "a", EnginePath: "library/busybox"}}, "library/busybox", layerDigest); err != nil {
		t.Fatalf("DecideBlobInRepo: %v", err)
	}

	var manifestPath, blobPath string
	for _, rec := range buf.records(t) {
		if rec["msg"] != "selection probe" {
			continue
		}
		switch rec["kind"] {
		case kindManifest:
			manifestPath, _ = rec["path"].(string)
		case kindBlob:
			blobPath, _ = rec["path"].(string)
		}
	}

	if want := a.URL + "/v2/library/alpine/manifests/latest"; manifestPath != want {
		t.Errorf("manifest probe path = %q, want %q", manifestPath, want)
	}
	if want := a.URL + "/v2/library/busybox/blobs/" + layerDigest; blobPath != want {
		t.Errorf("blob probe path = %q, want %q", blobPath, want)
	}
	t.Logf("probe paths logged: manifest=%s blob=%s", manifestPath, blobPath)
}

// TestDecisionLineSaysWhichKindWasProbed pins the field the dispatcher side's
// 103 criterion reads: the decision line must say whether a manifest or a blob
// was probed ("kind"), because that is how "the blob path reached the manifest
// probe" is spotted from a production log. The dispatcher's own test asserts the
// consequence it sees; this asserts the source of the field, so neither side can
// drop it silently and leave the criterion vacuous.
func TestDecisionLineSaysWhichKindWasProbed(t *testing.T) {
	a := newFakeRegistry(t, "library/alpine@latest", "library/busybox@"+layerDigest)

	buf := &logBuffer{}
	c := New(&fakeUpstream{
		serving: []string{"a"},
		elected: map[string]string{"a": a.URL},
	}, Options{
		Client: testClient(),
		TTL:    time.Minute,
		Logger: slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	if _, err := c.Decide(context.Background(),
		[]Candidate{{Namespace: "a", EnginePath: "library/alpine"}}, "alpine", "latest"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if _, err := c.DecideBlobInRepo(context.Background(),
		[]Candidate{{Namespace: "a", EnginePath: "library/busybox"}}, "library/busybox", layerDigest); err != nil {
		t.Fatalf("DecideBlobInRepo: %v", err)
	}

	byKind := map[any]map[string]any{}
	for _, rec := range buf.records(t) {
		if rec["msg"] == "selection" {
			byKind[rec["kind"]] = rec
		}
	}

	manifest := byKind[kindManifest]
	if manifest == nil {
		t.Fatalf("no decision line carried kind=%q: the dispatcher's blob-split criterion reads that field", kindManifest)
	}
	if manifest["ref"] != "latest" {
		t.Errorf("manifest decision ref = %v, want the tag", manifest["ref"])
	}

	blob := byKind[kindBlob]
	if blob == nil {
		t.Fatalf("no decision line carried kind=%q: a blob decision must be distinguishable from a manifest one", kindBlob)
	}
	if blob["ref"] != layerDigest {
		t.Errorf("blob decision ref = %v, want the digest", blob["ref"])
	}
	// The user-visible property the criterion rests on: a blob decision carries a
	// digest reference, so seeing one labelled "manifest" means the split failed.
	if !strings.HasPrefix(fmt.Sprint(blob["ref"]), "sha256:") {
		t.Errorf("blob decision ref = %v, want a digest", blob["ref"])
	}
	t.Logf("decision lines: kind=%s ref=%v | kind=%s ref=%v",
		manifest["kind"], manifest["ref"], blob["kind"], blob["ref"])
}

// ------------------------------------------------------------- unit-level bits

func TestParseBearerChallenge(t *testing.T) {
	got := parseBearerChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/alpine:pull"`)
	want := map[string]string{
		"realm":   "https://auth.docker.io/token",
		"service": "registry.docker.io",
		"scope":   "repository:library/alpine:pull",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}

	if parseBearerChallenge(`Basic realm="rms"`) != nil {
		t.Error("a Basic challenge must not be read as Bearer")
	}
	if parseBearerChallenge("") != nil {
		t.Error("an empty challenge must be nil")
	}
}

func TestEscapeSegmentKeepsDigestColon(t *testing.T) {
	cases := map[string]string{
		"sha256:abc":      "sha256:abc",
		"latest":          "latest",
		"team/app":        "team/app", // slashes are separators, escaped per segment
		"weird name":      "weird%20name",
		"a?b":             "a%3Fb",
		"${injection}":    "%24%7Binjection%7D",
		"UPPER_lower.1-2": "UPPER_lower.1-2",
	}
	for in, want := range cases {
		if in == "team/app" {
			// escapeRepo keeps the slash, escapeSegment escapes it.
			if got := escapeRepo(in); got != want {
				t.Errorf("escapeRepo(%q) = %q, want %q", in, got, want)
			}
			continue
		}
		if got := escapeSegment(in); got != want {
			t.Errorf("escapeSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPickTieBreaksDeterministically exercises the ranking rule directly, since
// two live httptest servers never finish in exactly the same nanosecond: an
// exact tie goes to the namespace name, and the upstream's own probe latency
// (the EWMA fallback) is only consulted while the names are still tied.
func TestPickTieBreaksDeterministically(t *testing.T) {
	probe := func(namespace string, latency time.Duration) probeResult {
		return probeResult{
			namespace:  namespace,
			enginePath: "library/alpine",
			reachable:  true,
			exists:     true,
			latency:    latency,
		}
	}

	t.Run("exact tie goes to the name", func(t *testing.T) {
		c := New(&fakeUpstream{}, Options{Logger: discardLogger()})
		target, err := c.pick([]probeResult{probe("bbb", 5*time.Millisecond), probe("aaa", 5*time.Millisecond)})
		if err != nil {
			t.Fatalf("pick: %v", err)
		}
		if target.Namespace != "aaa" {
			t.Errorf("winner = %q, want aaa (the earlier name)", target.Namespace)
		}
	})

	t.Run("exact tie falls back to the upstream latency when the names do not decide", func(t *testing.T) {
		// Same namespace name is impossible, so make the names sit on either
		// side and check the EWMA weight is what orders them by giving the
		// later name the better upstream latency.
		c := New(&fakeUpstream{ewma: map[string]time.Duration{"aaa": 10 * time.Millisecond}}, Options{Logger: discardLogger()})
		target, err := c.pick([]probeResult{probe("bbb", 5*time.Millisecond), probe("aaa", 5*time.Millisecond)})
		if err != nil {
			t.Fatalf("pick: %v", err)
		}
		// Only one candidate has an EWMA figure, so the names still decide —
		// the important part is that a half-known weight never picks a winner
		// on its own.
		if target.Namespace != "aaa" {
			t.Errorf("winner = %q, want aaa", target.Namespace)
		}
	})

	t.Run("the measurement always beats the fallback", func(t *testing.T) {
		c := New(&fakeUpstream{ewma: map[string]time.Duration{"bbb": time.Millisecond}}, Options{Logger: discardLogger()})
		target, err := c.pick([]probeResult{probe("aaa", 5*time.Millisecond), probe("bbb", 8*time.Millisecond)})
		if err != nil {
			t.Fatalf("pick: %v", err)
		}
		if target.Namespace != "aaa" {
			t.Errorf("winner = %q, want the faster measured probe (aaa)", target.Namespace)
		}
	})

	t.Run("a silent candidate is never a winner", func(t *testing.T) {
		c := New(&fakeUpstream{}, Options{Logger: discardLogger()})
		dead := probeResult{namespace: "aaa", enginePath: "library/alpine", latency: time.Millisecond}
		if _, err := c.pick([]probeResult{dead}); !errors.Is(err, ErrUnreachable) {
			t.Errorf("err = %v, want ErrUnreachable", err)
		}
		absent := probeResult{namespace: "aaa", enginePath: "library/alpine", reachable: true, status: 404, latency: time.Millisecond}
		if _, err := c.pick([]probeResult{absent}); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}
