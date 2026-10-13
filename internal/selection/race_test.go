package selection

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Upstream racing: one namespace, several upstreams, probed concurrently.
//
// These are the tests for the behaviour the namespace race already had, one
// level down: every upstream of a namespace is asked at the same time, the
// fastest one that actually has the content wins, and mirrors that disagree
// about what a tag points at are never alternated.
// ---------------------------------------------------------------------------

// deadURL returns an upstream URL nothing is listening on any more: the shape
// of a mirror that answers nothing at all.
func deadURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

// racingChooser builds a chooser over one namespace with the given upstreams.
func racingChooser(t *testing.T, ns string, upstreams []string, opts Options) (*Chooser, *fakeUpstream) {
	t.Helper()

	up := &fakeUpstream{
		serving:   []string{ns},
		elected:   map[string]string{ns: upstreams[0]},
		upstreams: map[string][]string{ns: upstreams},
	}
	if opts.Client == nil {
		opts.Client = testClient()
	}
	if opts.TTL == 0 {
		opts.TTL = time.Minute
	}
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = discardLogger()
	}
	return New(up, opts), up
}

func TestRaceProbesEveryUpstreamAndServesTheFastest(t *testing.T) {
	slow := newFakeRegistry(t, "library/alpine@latest")
	fast := newFakeRegistry(t, "library/alpine@latest")
	slow.delay = 120 * time.Millisecond
	fast.delay = 5 * time.Millisecond

	c, up := racingChooser(t, "docker.io", []string{slow.URL, fast.URL}, Options{})

	target, err := c.Decide(context.Background(), candidatesFor("docker.io"), "alpine", "latest")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if target.Namespace != "docker.io" {
		t.Errorf("namespace = %q, want docker.io", target.Namespace)
	}
	if target.Upstream != fast.URL {
		t.Errorf("upstream = %q, want the fast mirror %q", target.Upstream, fast.URL)
	}

	// Both were asked: the fan-out is the point, not a fallback chain.
	if slow.manifests.Load() == 0 || fast.manifests.Load() == 0 {
		t.Errorf("manifests probed: slow=%d fast=%d, want both", slow.manifests.Load(), fast.manifests.Load())
	}

	// The decision line counts probes, not namespaces.
	probes := up.observedProbes()
	if len(probes) != 2 {
		t.Fatalf("content probe verdicts = %d, want one per upstream", len(probes))
	}
	for _, p := range probes {
		if !p.answered {
			t.Errorf("upstream %s answered with content but was reported as unanswered", p.upstream)
		}
	}
}

func TestRaceServesTheWinningNamespaceAndItsFastestUpstream(t *testing.T) {
	// Two namespaces, one of them with two mirrors: the namespace race and the
	// upstream race happen in the same decision.
	slowMirror := newFakeRegistry(t, "library/alpine@latest")
	fastMirror := newFakeRegistry(t, "library/alpine@latest")
	slowMirror.delay = 100 * time.Millisecond
	fastMirror.delay = 5 * time.Millisecond
	other := newFakeRegistry(t, "library/alpine@latest")
	other.delay = 60 * time.Millisecond

	up := &fakeUpstream{
		serving: []string{"docker.io", "ghcr.io"},
		elected: map[string]string{"docker.io": slowMirror.URL, "ghcr.io": other.URL},
		upstreams: map[string][]string{
			"docker.io": {slowMirror.URL, fastMirror.URL},
			"ghcr.io":   {other.URL},
		},
	}
	c := New(up, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

	target, err := c.Decide(context.Background(), candidatesFor("docker.io", "ghcr.io"), "alpine", "latest")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if target.Namespace != "docker.io" || target.Upstream != fastMirror.URL {
		t.Fatalf("target = %s/%s, want docker.io/%s (the fast mirror of the faster namespace)",
			target.Namespace, target.Upstream, fastMirror.URL)
	}
}

func TestDigestAuthorityIgnoresAFasterMirrorThatDisagrees(t *testing.T) {
	// The configured order is the authority order: the first upstream that
	// answers defines what the tag is. A mirror holding something else for the
	// same tag is stale or has been replaced, and must not serve it — however
	// fast it is.
	authority := newFakeRegistry(t, "library/alpine@latest")
	authority.digest = "sha256:" + repeat("1", 64)
	authority.delay = 90 * time.Millisecond

	stale := newFakeRegistry(t, "library/alpine@latest")
	stale.digest = "sha256:" + repeat("2", 64)
	stale.delay = 5 * time.Millisecond

	c, _ := racingChooser(t, "docker.io", []string{authority.URL, stale.URL}, Options{})

	target, err := c.Decide(context.Background(), candidatesFor("docker.io"), "alpine", "latest")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if target.Upstream != authority.URL {
		t.Fatalf("upstream = %q, want the configured-first mirror %q", target.Upstream, authority.URL)
	}

	// And it stays that way across the requests the decision serves: the stale
	// mirror is not alternated in even though it is much faster.
	for i := 0; i < 6; i++ {
		got, err := c.Decide(context.Background(), candidatesFor("docker.io"), "alpine", "latest")
		if err != nil {
			t.Fatalf("Decide %d: %v", i, err)
		}
		if got.Upstream != authority.URL {
			t.Fatalf("request %d served by %q, want only %q", i, got.Upstream, authority.URL)
		}
	}
}

func TestDigestAuthorityWhenMirrorsAgree(t *testing.T) {
	// Same bytes on both: the fast one serves first, and both are in the pool.
	// The window is widened here on purpose — these two mirrors are two
	// milliseconds apart, and the point of the test is the rotation, not the
	// window (which has its own test).
	slow := newFakeRegistry(t, "library/alpine@latest")
	slow.delay = 10 * time.Millisecond
	fast := newFakeRegistry(t, "library/alpine@latest")
	fast.delay = 5 * time.Millisecond

	c, _ := racingChooser(t, "docker.io", []string{slow.URL, fast.URL}, Options{
		BalanceWindow: 5,
	})

	target, err := c.Decide(context.Background(), candidatesFor("docker.io"), "alpine", "latest")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if target.Upstream != fast.URL {
		t.Errorf("first decision = %q, want the fastest eligible mirror %q", target.Upstream, fast.URL)
	}

	// Traffic is spread, with the weight on the faster mirror.
	seen := map[string]int{}
	for i := 0; i < 30; i++ {
		got, err := c.Decide(context.Background(), candidatesFor("docker.io"), "alpine", "latest")
		if err != nil {
			t.Fatalf("Decide %d: %v", i, err)
		}
		seen[got.Upstream]++
	}
	if seen[slow.URL] == 0 || seen[fast.URL] == 0 {
		t.Fatalf("load balancing did not spread traffic: %v", seen)
	}
	if seen[fast.URL] <= seen[slow.URL] {
		t.Errorf("distribution = %v, want the faster mirror to take the larger share", seen)
	}
}

func TestBalanceOffKeepsOnlyTheFastest(t *testing.T) {
	first := newFakeRegistry(t, "library/alpine@latest")
	second := newFakeRegistry(t, "library/alpine@latest")
	first.delay = 20 * time.Millisecond
	second.delay = 5 * time.Millisecond

	c, _ := racingChooser(t, "docker.io", []string{first.URL, second.URL}, Options{
		Balance: func(string) string { return "off" },
	})

	for i := 0; i < 10; i++ {
		got, err := c.Decide(context.Background(), candidatesFor("docker.io"), "alpine", "latest")
		if err != nil {
			t.Fatalf("Decide %d: %v", i, err)
		}
		if got.Upstream != second.URL {
			t.Fatalf("request %d served by %q, want only the fastest %q", i, got.Upstream, second.URL)
		}
	}
}

func TestBalanceWindowKeepsAMuchSlowerMirrorOutOfRotation(t *testing.T) {
	fast := newFakeRegistry(t, "library/alpine@latest")
	slow := newFakeRegistry(t, "library/alpine@latest")
	fast.delay = 5 * time.Millisecond
	// Five times slower: outside the default 1.5x window.
	slow.delay = 30 * time.Millisecond

	c, _ := racingChooser(t, "docker.io", []string{fast.URL, slow.URL}, Options{})

	seen := map[string]int{}
	for i := 0; i < 20; i++ {
		got, err := c.Decide(context.Background(), candidatesFor("docker.io"), "alpine", "latest")
		if err != nil {
			t.Fatalf("Decide %d: %v", i, err)
		}
		seen[got.Upstream]++
	}
	if seen[slow.URL] != 0 {
		t.Fatalf("distribution = %v, want the mirror outside the balancing window to take no traffic", seen)
	}
}

func TestDecisionDeadlineBoundsTheFanOut(t *testing.T) {
	fast := newFakeRegistry(t, "library/alpine@latest")
	fast.delay = 5 * time.Millisecond
	stuck := newFakeRegistry(t, "library/alpine@latest")
	stuck.delay = 2 * time.Second

	c, _ := racingChooser(t, "docker.io", []string{fast.URL, stuck.URL}, Options{
		Timeout:          5 * time.Second,
		DecisionDeadline: 20 * time.Millisecond,
	})

	start := time.Now()
	target, err := c.Decide(context.Background(), candidatesFor("docker.io"), "alpine", "latest")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if target.Upstream != fast.URL {
		t.Errorf("upstream = %q, want the mirror that answered in time", target.Upstream)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("decision took %v, want it bounded by the deadline rather than the slow mirror", elapsed)
	}
}

// TestRaceDoesNotLetASilentUpstreamDecide checks the failure semantics survive
// the fan-out: an upstream that answered "no such repository" makes the answer
// a 404, and one that said nothing at all does not.
func TestRaceDoesNotLetASilentUpstreamDecide(t *testing.T) {
	absent := newFakeRegistry(t) // holds nothing: every path is a 404
	c, _ := racingChooser(t, "docker.io", []string{absent.URL, deadURL(t)}, Options{
		Timeout: time.Second,
	})

	_, err := c.Decide(context.Background(), candidatesFor("docker.io"), "alpine", "latest")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound: one upstream answered, the other said nothing", err)
	}

	// The other way round: nobody answered, so the answer is unknown, not "no".
	c2, _ := racingChooser(t, "docker.io", []string{deadURL(t), deadURL(t)}, Options{
		Timeout: time.Second,
	})
	if _, err := c2.Decide(context.Background(), candidatesFor("docker.io"), "alpine", "latest"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
}

func TestRefusalsAreReportedAsAnswersAndSilenceAsFailure(t *testing.T) {
	refusing := newFakeRegistry(t) // 404: an answer about the content
	c, up := racingChooser(t, "docker.io", []string{refusing.URL, deadURL(t)}, Options{
		Timeout: time.Second,
	})

	if _, err := c.Decide(context.Background(), candidatesFor("docker.io"), "alpine", "latest"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}

	verdicts := map[string]bool{}
	for _, p := range up.observedProbes() {
		verdicts[p.upstream] = p.answered
	}
	if got, ok := verdicts[refusing.URL]; !ok || !got {
		t.Errorf("verdict for the refusing upstream = %v (present %v), want answered=true: a 404 is an answer", got, ok)
	}
	for url, answered := range verdicts {
		if url != refusing.URL && answered {
			t.Errorf("verdict for %s = true, want answered=false for an upstream that said nothing", url)
		}
	}
}

func TestQualifiedDecisionStaysInsideItsNamespace(t *testing.T) {
	mine := newFakeRegistry(t, "library/alpine@latest")
	other := newFakeRegistry(t, "library/alpine@latest")

	up := &fakeUpstream{
		serving: []string{"docker.io", "ghcr.io"},
		elected: map[string]string{"docker.io": mine.URL, "ghcr.io": other.URL},
	}
	c := New(up, Options{Client: testClient(), TTL: time.Minute, Logger: discardLogger()})

	target, err := c.DecideQualified(context.Background(), "docker.io", "library/alpine", "latest")
	if err != nil {
		t.Fatalf("DecideQualified: %v", err)
	}
	if target.Namespace != "docker.io" {
		t.Fatalf("namespace = %q, want docker.io", target.Namespace)
	}
	if other.manifests.Load() != 0 {
		t.Errorf("the other namespace was probed %d times, want none: a qualified request asks one namespace",
			other.manifests.Load())
	}

	// The qualified decision is cached under its own key, so an unqualified
	// request for the same repository still runs its own race.
	before := other.manifests.Load()
	if _, err := c.Decide(context.Background(), candidatesFor("docker.io", "ghcr.io"), "alpine", "latest"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if other.manifests.Load() == before {
		t.Error("the unqualified request reused the qualified decision: it must race every namespace")
	}
}

func TestQualifiedBlobFollowsItsManifestDecision(t *testing.T) {
	first := newFakeRegistry(t, "library/alpine@latest")
	second := newFakeRegistry(t, "library/alpine@latest")
	first.delay = 60 * time.Millisecond
	second.delay = 5 * time.Millisecond

	c, _ := racingChooser(t, "docker.io", []string{first.URL, second.URL}, Options{})

	manifest, err := c.DecideQualified(context.Background(), "docker.io", "library/alpine", "latest")
	if err != nil {
		t.Fatalf("DecideQualified: %v", err)
	}

	blob, ok := c.QualifiedUpstream("docker.io", "library/alpine")
	if !ok {
		t.Fatal("no qualified affinity was recorded for the repository")
	}
	if blob.Upstream == "" {
		t.Fatal("the qualified affinity carries no upstream")
	}
	// It follows the same pool, so it is one of the two mirrors and not a
	// third thing invented for blobs.
	if blob.Upstream != first.URL && blob.Upstream != second.URL {
		t.Fatalf("blob upstream = %q, want one of the repository's mirrors", blob.Upstream)
	}
	if manifest.Namespace != blob.Namespace {
		t.Errorf("blob namespace = %q, want the manifest's %q", blob.Namespace, manifest.Namespace)
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
