package upstream

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRegistry answers /v2/ like the real thing: only /v2/ is handled, and the
// status it returns is configurable so tests can simulate open (200) and
// credential-requiring (401) registries.
type fakeRegistry struct {
	*httptest.Server
	status atomic.Int32
	up     atomic.Bool
	hits   atomic.Int64
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()

	f := &fakeRegistry{}
	f.status.Store(http.StatusOK)
	f.up.Store(true)

	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if !f.up.Load() {
			// Simulate an unreachable backend.
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					_ = conn.Close()
					return
				}
			}
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.URL.Path != "/v2/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		w.WriteHeader(int(f.status.Load()))
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte("{}"))
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewRejectsBadInput(t *testing.T) {
	if _, err := New("docker.io", nil, nil, Options{Logger: testLogger()}); err == nil {
		t.Error("expected an error for no endpoints")
	}
	if _, err := New("docker.io", []string{"https://a", "  "}, nil, Options{Logger: testLogger()}); err == nil {
		t.Error("expected an error for an empty endpoint")
	}
}

func TestNewDeduplicatesAndTrims(t *testing.T) {
	s, err := New("docker.io", []string{"https://a/", "https://a", "https://b/"}, nil, Options{Logger: testLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := s.Endpoints()
	if len(got) != 2 || got[0] != "https://a" || got[1] != "https://b" {
		t.Errorf("Endpoints() = %v, want [https://a https://b]", got)
	}
}

func TestElectForStartupPicksFirstReachable(t *testing.T) {
	down := newFakeRegistry(t)
	down.up.Store(false)
	up := newFakeRegistry(t)

	s, err := New("docker.io", []string{down.URL, up.URL}, testClient(), Options{Logger: testLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := s.ElectForStartup(context.Background())
	if err != nil {
		t.Fatalf("ElectForStartup: %v", err)
	}
	if got != up.URL {
		t.Errorf("elected %q, want the reachable endpoint %q", got, up.URL)
	}
	if s.Failovers() != 0 {
		t.Errorf("startup election reported %d failovers, want 0", s.Failovers())
	}
	if _, healthy := s.ElectedForRequest(); !healthy {
		t.Error("elected endpoint should be healthy after startup")
	}
}

func TestElectForStartupFailsWhenNothingAnswers(t *testing.T) {
	down := newFakeRegistry(t)
	down.up.Store(false)

	s, err := New("docker.io", []string{down.URL}, testClient(), Options{Logger: testLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := s.ElectForStartup(context.Background()); err == nil {
		t.Fatal("expected startup election to fail")
	}
	if s.Elected() != "" {
		t.Errorf("Elected() = %q, want empty", s.Elected())
	}
	if s.LastError() == nil {
		t.Error("LastError() should be set")
	}
}

func TestUnauthorizedEndpointCountsAsReachable(t *testing.T) {
	auth := newFakeRegistry(t)
	auth.status.Store(http.StatusUnauthorized)

	s, err := New("docker.io", []string{auth.URL}, testClient(), Options{Logger: testLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.ElectForStartup(context.Background()); err != nil {
		t.Fatalf("a 401 endpoint must count as reachable: %v", err)
	}
}

func TestNonRegistryEndpointIsRejected(t *testing.T) {
	// A web server that answers 404 on /v2/ is not a registry.
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(web.Close)

	s, err := New("docker.io", []string{web.URL}, testClient(), Options{Logger: testLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.ElectForStartup(context.Background()); err == nil {
		t.Fatal("expected a 404 endpoint to be rejected")
	}
}

// TestFailoverMovesToNextEndpoint exercises the documented failover semantics:
// after FailThreshold consecutive failures the next healthy endpoint is elected
// and the change is reported through OnElect.
func TestFailoverMovesToNextEndpoint(t *testing.T) {
	a := newFakeRegistry(t)
	b := newFakeRegistry(t)

	s, err := New("docker.io", []string{a.URL, b.URL}, testClient(), Options{
		Logger:        testLogger(),
		FailThreshold: 2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	elections := make(chan string, 4)
	s.OnElect(func(url string) { elections <- url })

	if _, err := s.ElectForStartup(context.Background()); err != nil {
		t.Fatalf("ElectForStartup: %v", err)
	}
	if got := s.Elected(); got != a.URL {
		t.Fatalf("Elected() = %q, want %q", got, a.URL)
	}

	// Take A down. One failure must not move traffic; two must.
	a.up.Store(false)

	if err := s.ProbeAll(context.Background()); err != nil {
		t.Logf("first probe error (expected): %v", err)
	}
	if got := s.Elected(); got != a.URL {
		t.Errorf("after 1 failure Elected() = %q, want to stay on %q (fail_threshold=2)", got, a.URL)
	}

	if err := s.ProbeAll(context.Background()); err != nil {
		t.Logf("second probe error (expected): %v", err)
	}
	if got := s.Elected(); got != b.URL {
		t.Errorf("after 2 failures Elected() = %q, want failover to %q", got, b.URL)
	}
	if s.Failovers() != 1 {
		t.Errorf("Failovers() = %d, want 1", s.Failovers())
	}

	select {
	case got := <-elections:
		if got != b.URL {
			t.Errorf("OnElect reported %q, want %q", got, b.URL)
		}
	case <-time.After(time.Second):
		t.Fatal("OnElect callback was not invoked on failover")
	}
}

// TestNoFailoverWhenThresholdNotReached guards against flapping.
func TestNoFailoverWhenThresholdNotReached(t *testing.T) {
	a := newFakeRegistry(t)
	b := newFakeRegistry(t)

	s, err := New("docker.io", []string{a.URL, b.URL}, testClient(), Options{
		Logger:        testLogger(),
		FailThreshold: 3,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.ElectForStartup(context.Background()); err != nil {
		t.Fatalf("ElectForStartup: %v", err)
	}

	a.up.Store(false)
	for i := 0; i < 2; i++ {
		_ = s.ProbeAll(context.Background())
	}
	if got := s.Elected(); got != a.URL {
		t.Errorf("Elected() = %q, want to stay on %q below the threshold", got, a.URL)
	}
	if s.Failovers() != 0 {
		t.Errorf("Failovers() = %d, want 0", s.Failovers())
	}
}

// TestSuccessThresholdDelaysRecovery verifies an endpoint is not declared
// healthy on a single lucky probe when success_threshold is higher.
func TestSuccessThresholdDelaysRecovery(t *testing.T) {
	a := newFakeRegistry(t)
	b := newFakeRegistry(t)

	s, err := New("docker.io", []string{a.URL, b.URL}, testClient(), Options{
		Logger:           testLogger(),
		FailThreshold:    1,
		SuccessThreshold: 2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.ElectForStartup(context.Background()); err != nil {
		t.Fatalf("ElectForStartup: %v", err)
	}

	a.up.Store(false)
	_ = s.ProbeAll(context.Background())
	if got := s.Elected(); got != b.URL {
		t.Fatalf("Elected() = %q, want %q", got, b.URL)
	}

	// A is back, but one success is not enough to take traffic back.
	a.up.Store(true)
	_ = s.ProbeAll(context.Background())
	if got := s.Elected(); got != b.URL {
		t.Errorf("Elected() = %q, want to stay on %q until success_threshold is reached", got, b.URL)
	}

	_ = s.ProbeAll(context.Background())
	if got := s.Elected(); got != a.URL {
		t.Errorf("Elected() = %q, want recovery to %q after success_threshold successes", got, a.URL)
	}
}

// TestStaysOnElectedWhenEverythingIsDown documents that traffic keeps going to
// the last working endpoint rather than to nothing.
func TestStaysOnElectedWhenEverythingIsDown(t *testing.T) {
	a := newFakeRegistry(t)
	b := newFakeRegistry(t)

	s, err := New("docker.io", []string{a.URL, b.URL}, testClient(), Options{
		Logger:        testLogger(),
		FailThreshold: 1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.ElectForStartup(context.Background()); err != nil {
		t.Fatalf("ElectForStartup: %v", err)
	}

	a.up.Store(false)
	b.up.Store(false)
	_ = s.ProbeAll(context.Background())

	if got := s.Elected(); got == "" {
		t.Error("Elected() must keep the last endpoint so in-flight traffic still has a target")
	}
	if _, healthy := s.ElectedForRequest(); healthy {
		t.Error("ElectedForRequest should report unhealthy")
	}
}

func TestRunStopsWithContext(t *testing.T) {
	reg := newFakeRegistry(t)
	s, err := New("docker.io", []string{reg.URL}, testClient(), Options{
		Logger: testLogger(), Interval: 20 * time.Millisecond, Enabled: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	time.Sleep(80 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop when the context was cancelled")
	}
	if reg.hits.Load() == 0 {
		t.Error("Run should have probed the endpoint")
	}
}

// testClient builds a client that talks straight to the fake registries.
func testClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
}

// TestDeadStartupEndpointIsNotElectedLater guards a bug found against a live
// server: after every endpoint failed the startup probe, the periodic election
// still picked the first endpoint because its failure had not been recorded, so
// each probe tick tried to rebuild an engine against a known-dead upstream.
func TestDeadStartupEndpointIsNotElectedLater(t *testing.T) {
	dead := newFakeRegistry(t)
	dead.up.Store(false)

	s, err := New("docker.io", []string{dead.URL}, testClient(), Options{
		Logger:        testLogger(),
		FailThreshold: 2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var elections int
	s.OnElect(func(string) { elections++ })

	if _, err := s.ElectForStartup(context.Background()); err == nil {
		t.Fatal("expected the startup election to fail")
	}
	if got := s.Elected(); got != "" {
		t.Errorf("Elected() = %q, want empty after a failed startup", got)
	}

	// A probe tick with nothing healthy must not elect the dead endpoint.
	_ = s.ProbeAll(context.Background())
	if got := s.Elected(); got != "" {
		t.Errorf("Elected() = %q, want still empty", got)
	}
	if elections != 0 {
		t.Errorf("OnElect fired %d times, want 0 (no election should change)", elections)
	}
	if s.Failovers() != 0 {
		t.Errorf("Failovers() = %d, want 0", s.Failovers())
	}
}

// TestRecoveryAfterFailedStartup verifies a namespace that came up with no
// reachable endpoint recovers once one answers.
func TestRecoveryAfterFailedStartup(t *testing.T) {
	dead := newFakeRegistry(t)
	dead.up.Store(false)

	s, err := New("docker.io", []string{dead.URL}, testClient(), Options{
		Logger: testLogger(), FailThreshold: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	elected := make(chan string, 4)
	s.OnElect(func(url string) { elected <- url })

	if _, err := s.ElectForStartup(context.Background()); err == nil {
		t.Fatal("expected the startup election to fail")
	}

	dead.up.Store(true)
	_ = s.ProbeAll(context.Background())

	if got := s.Elected(); got != dead.URL {
		t.Fatalf("Elected() = %q, want recovery to %q", got, dead.URL)
	}

	select {
	case got := <-elected:
		if got != dead.URL {
			t.Errorf("OnElect reported %q, want %q", got, dead.URL)
		}
	case <-time.After(time.Second):
		t.Fatal("OnElect was not invoked on recovery")
	}
}

// --- race candidate list and content health --------------------------------

func TestUpstreamsListsEveryEndpointInOrder(t *testing.T) {
	sel, err := New("docker.io", []string{"https://a/", "https://b", "https://a"}, nil,
		Options{Logger: testLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Duplicates collapse and trailing slashes are normalised, exactly as the
	// election sees them.
	got := sel.Upstreams()
	want := []string{"https://a", "https://b"}
	if len(got) != len(want) {
		t.Fatalf("Upstreams() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Upstreams() = %v, want %v", got, want)
		}
	}
}

func TestKnownBadEndpointsLeaveTheRaceWhenTheHealthLoopCanReadmitThem(t *testing.T) {
	a := newFakeRegistry(t)
	b := newFakeRegistry(t)
	b.up.Store(false)

	sel, err := New("docker.io", []string{a.URL, b.URL}, nil, Options{
		Interval:      time.Minute,
		Timeout:       2 * time.Second,
		FailThreshold: 1,
		Enabled:       true, // the health loop will re-probe b
		Logger:        testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := sel.ElectForStartup(context.Background()); err != nil {
		t.Fatalf("ElectForStartup: %v", err)
	}
	// ElectForStartup stops at the first reachable endpoint, so the health loop
	// is what learns about the other one. Its error is expected: b is down.
	if err := sel.ProbeAll(context.Background()); err == nil {
		t.Fatal("expected the health probe of the dead endpoint to report an error")
	}

	got := sel.Upstreams()
	if len(got) != 1 || got[0] != a.URL {
		t.Fatalf("Upstreams() = %v, want only the reachable %s", got, a.URL)
	}
}

func TestEveryEndpointLooksBadSoTheRaceStillGetsThem(t *testing.T) {
	a := newFakeRegistry(t)
	a.up.Store(false)
	b := newFakeRegistry(t)
	b.up.Store(false)

	sel, err := New("docker.io", []string{a.URL, b.URL}, nil, Options{
		Interval:      time.Minute,
		Timeout:       time.Second,
		FailThreshold: 1,
		Enabled:       true,
		Logger:        testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := sel.ElectForStartup(context.Background()); err == nil {
		t.Fatal("expected startup to fail with both endpoints down")
	}

	// A namespace whose every endpoint looks bad must still be probed: the
	// content probe is the only way one of them proves it works again.
	if got := sel.Upstreams(); len(got) != 2 {
		t.Fatalf("Upstreams() = %v, want both endpoints", got)
	}
}

func TestContentFailureDropsAnUpstreamFromTheRaceAndRecoversOnAnswer(t *testing.T) {
	a := newFakeRegistry(t)
	b := newFakeRegistry(t)

	sel, err := New("docker.io", []string{a.URL, b.URL}, nil, Options{
		Interval:      time.Minute,
		Timeout:       time.Second,
		FailThreshold: 2,
		Logger:        testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// One unanswered content probe is not enough.
	sel.ObserveContentProbe(b.URL, false, 0, context.DeadlineExceeded)
	if got := sel.Upstreams(); len(got) != 2 {
		t.Fatalf("after one failure Upstreams() = %v, want both endpoints", got)
	}

	// The threshold latches it out, even though its /v2/ answers.
	sel.ObserveContentProbe(b.URL, false, 0, context.DeadlineExceeded)
	got := sel.Upstreams()
	if len(got) != 1 || got[0] != a.URL {
		t.Fatalf("after the threshold Upstreams() = %v, want only %s", got, a.URL)
	}
	if bad := sel.ContentBad(); len(bad) != 1 || bad[0] != b.URL {
		t.Fatalf("ContentBad() = %v, want [%s]", bad, b.URL)
	}

	// Answering content clears the cooldown immediately.
	sel.ObserveContentProbe(b.URL, true, 5*time.Millisecond, nil)
	if got := sel.Upstreams(); len(got) != 2 {
		t.Fatalf("after recovery Upstreams() = %v, want both endpoints", got)
	}
	if bad := sel.ContentBad(); len(bad) != 0 {
		t.Fatalf("ContentBad() = %v, want none", bad)
	}
}

func TestRefusalsAreAnswersAndNeverDemoteAnUpstream(t *testing.T) {
	a := newFakeRegistry(t)
	b := newFakeRegistry(t)

	sel, err := New("docker.io", []string{a.URL, b.URL}, nil, Options{
		Interval:      time.Minute,
		FailThreshold: 1,
		Logger:        testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// A 404 is an answer: the endpoint is serving content, it just does not
	// have this repository. A 401/403 is the same shape.
	for i := 0; i < 5; i++ {
		sel.ObserveContentProbe(b.URL, true, time.Millisecond, nil)
	}
	if got := sel.Upstreams(); len(got) != 2 {
		t.Fatalf("Upstreams() = %v, want both endpoints after refusals", got)
	}
}
