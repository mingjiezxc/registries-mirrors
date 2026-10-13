// Package upstream elects one upstream endpoint per proxy namespace and fails
// over to the next when it stops answering.
//
// This exists because the embedded distribution proxy accepts exactly one
// RemoteURL, fixed when the namespace's engine is constructed, and its own
// authentication code refuses bearer challenges whose realm is not in the same
// registrable domain as that RemoteURL (registry/proxy/proxyauth.go
// realmAllowed). Pointing RemoteURL at an in-process failover listener is
// therefore impossible for Docker Hub and ghcr.io, so failover is implemented
// one level up: elect an endpoint, then rebuild the namespace's engine when the
// election changes.
//
// The election is the fallback and the legacy path, not the whole story. The
// selector also owns each endpoint's health state, and hands the candidate list
// to the selection layer (Upstreams), which probes every endpoint of a namespace
// concurrently for the repository being served and serves the fastest one. In
// that mode an endpoint's election matters only for the sticky engine a cached
// request uses. The granularity is deliberate and documented: failover happens
// per namespace election, not per blob request.
package upstream

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Options configures a Selector.
type Options struct {
	// Path is the probe path, normally /v2/.
	Path string
	// Interval is the probe period.
	Interval time.Duration
	// Timeout bounds one probe.
	Timeout time.Duration
	// FailThreshold is the number of consecutive failures that evicts an
	// endpoint.
	FailThreshold int
	// SuccessThreshold is the number of consecutive successes that makes an
	// endpoint healthy.
	SuccessThreshold int
	// Enabled turns periodic re-election on for a namespace that already has a
	// working endpoint. A namespace that never got one is always retried, since
	// otherwise it could never come up.
	Enabled bool
	// Logger receives election events.
	Logger *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.Path == "" {
		o.Path = "/v2/"
	}
	if o.Interval <= 0 {
		o.Interval = 30 * time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Second
	}
	if o.FailThreshold <= 0 {
		o.FailThreshold = 2
	}
	if o.SuccessThreshold <= 0 {
		o.SuccessThreshold = 1
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// latencyEWMAAlpha is the weight of the newest successful probe in the moving
// average. It is high enough to react to a mirror degrading within a few probe
// intervals, and low enough that one slow probe does not move the figure much.
const latencyEWMAAlpha = 0.3

type endpoint struct {
	url string
	// ewma is the exponentially weighted moving average of this endpoint's
	// successful probe durations, zero until one succeeds. It is fed by the
	// health probe and by the content probes rms makes while serving, and it is
	// only ever read for its own endpoint, so it is guarded by the selector lock
	// like every other field here.
	ewma time.Duration
	// healthy is the latched health state.
	healthy bool
	// consecSuccess and consecFail drive the thresholds; only the counter
	// matching the current state increments.
	consecSuccess int
	consecFail    int
	// lastErr is the most recent probe failure for this endpoint.
	lastErr error

	// contentFail counts consecutive *content* probes that produced no usable
	// answer — a transport failure, a timeout, a 5xx or a 429. A refusal
	// (401/403/404) is an answer and never counts. contentBadUntil is the
	// cooldown that follows once the threshold is reached.
	//
	// This is deliberately separate from healthy/consecFail: /v2/ answering
	// says nothing about whether the endpoint can serve content, and the two
	// failure modes are exactly the defect this separates — a mirror whose API
	// root is healthy while its manifests and blobs hang or 5xx (measured
	// against docker.m.daocloud.io: /v2/ 401 in 0.17s, large blobs timing out
	// after 60s). Health probes must not undo a content verdict, so the health
	// loop only ever touches the fields above it.
	contentFail     int
	contentBadUntil time.Time
}

// knownBad reports whether the endpoint has failed enough consecutive probes to
// be considered unusable.
func (e *endpoint) knownBad(failThreshold int) bool {
	return !e.healthy && e.consecFail >= failThreshold
}

// contentBad reports whether the endpoint is serving a content-failure
// cooldown.
func (e *endpoint) contentBad(now time.Time) bool {
	return now.Before(e.contentBadUntil)
}

// observeLatency folds one successful probe's duration into the endpoint's
// moving average. Callers must hold the selector lock.
func (e *endpoint) observeLatency(d time.Duration) {
	if d <= 0 {
		return
	}
	if e.ewma == 0 {
		e.ewma = d
		return
	}
	e.ewma = time.Duration(float64(e.ewma)*(1-latencyEWMAAlpha) + float64(d)*latencyEWMAAlpha)
}

// Selector tracks a namespace's candidate endpoints and elects one.
type Selector struct {
	name    string
	opts    Options
	client  *http.Client
	logger  *slog.Logger
	onElect func(url string)

	mu       sync.Mutex
	eps      []endpoint
	elected  int // -1 until an election succeeds
	lastErr  error
	failover int
}

// New builds a Selector for a namespace. Duplicate endpoints are collapsed.
func New(name string, urls []string, client *http.Client, opts Options) (*Selector, error) {
	if len(urls) == 0 {
		return nil, fmt.Errorf("namespace %s: no upstream endpoints configured", name)
	}
	opts = opts.withDefaults()
	if client == nil {
		client = http.DefaultClient
	}

	eps := make([]endpoint, 0, len(urls))
	seen := map[string]bool{}
	for _, u := range urls {
		u = strings.TrimRight(strings.TrimSpace(u), "/")
		if u == "" {
			return nil, fmt.Errorf("namespace %s: empty upstream endpoint", name)
		}
		if seen[u] {
			continue
		}
		seen[u] = true
		eps = append(eps, endpoint{url: u})
	}

	return &Selector{
		name:    name,
		opts:    opts,
		client:  client,
		logger:  opts.Logger.With("namespace", name),
		eps:     eps,
		elected: -1,
	}, nil
}

// OnElect registers a callback invoked whenever the elected endpoint changes.
// It runs without the selector lock held.
func (s *Selector) OnElect(fn func(url string)) { s.onElect = fn }

// Endpoints returns the configured endpoints in order.
func (s *Selector) Endpoints() []string {
	out := make([]string, len(s.eps))
	for i, e := range s.eps {
		out[i] = e.url
	}
	return out
}

// Upstreams returns the endpoints a content race may probe, in configuration
// order.
//
// It is the race's candidate list, and it omits two kinds of endpoint:
//
//   - one serving a content-failure cooldown — /v2/ answers but content does
//     not, which is the only evidence that can tell a real mirror from one that
//     hangs on manifests and blobs (see ObserveContentProbe);
//   - one the health loop has latched as known-bad, because that loop is about
//     to re-probe it anyway. When the health loop is switched off
//     (upstream.health.enabled: false) there is no such re-probe, and the race
//     is the only evidence left, so those endpoints are offered instead.
//
// When every endpoint is filtered out, the full list is returned. The filter
// exists to save a hopeless request, not to make a namespace unserviceable:
// the only way an endpoint proves it works again is by answering a probe, so a
// namespace whose every endpoint looks bad must still be allowed to try.
func (s *Selector) Upstreams() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	out := make([]string, 0, len(s.eps))
	for i := range s.eps {
		e := &s.eps[i]
		if e.contentBad(now) {
			continue
		}
		if s.opts.Enabled && e.knownBad(s.opts.FailThreshold) {
			continue
		}
		out = append(out, e.url)
	}
	if len(out) == 0 {
		for i := range s.eps {
			out = append(out, s.eps[i].url)
		}
	}
	return out
}

// ContentBad returns the endpoints currently serving a content-failure
// cooldown, in configuration order.
func (s *Selector) ContentBad() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	var out []string
	for i := range s.eps {
		if s.eps[i].contentBad(now) {
			out = append(out, s.eps[i].url)
		}
	}
	return out
}

// ObserveContentProbe folds the outcome of one content probe into an endpoint's
// content health.
//
// answered is true when the endpoint produced a usable answer about the content
// it was asked for: "here it is" and "I do not have that repository" both
// count, because both prove the endpoint is serving content. It is false only
// when the question stayed open — a transport failure, a timeout, a 5xx or a
// 429.
//
// fail_threshold consecutive unanswered content probes put the endpoint in a
// cooldown of two health intervals, during which Upstreams leaves it out of the
// race; answering again clears the cooldown immediately. This is the mechanism
// that makes "healthy API root, broken content" fail over, which the /v2/
// health loop cannot do by construction — and must not undo, which is why this
// state is kept apart from the loop's own.
func (s *Selector) ObserveContentProbe(url string, answered bool, latency time.Duration, err error) {
	now := time.Now()
	cooldown := 2 * s.opts.Interval

	s.mu.Lock()
	i := s.indexOfLocked(url)
	if i < 0 {
		s.mu.Unlock()
		return
	}
	e := &s.eps[i]
	wasBad := e.contentBad(now)

	if answered {
		e.contentFail = 0
		e.contentBadUntil = time.Time{}
		if latency > 0 {
			e.observeLatency(latency)
		}
		s.mu.Unlock()

		if wasBad {
			s.logger.Info("upstream is serving content again", "upstream", url)
		}
		return
	}

	e.contentFail++
	entered := false
	switch {
	case e.contentFail >= s.opts.FailThreshold && !wasBad:
		e.contentBadUntil = now.Add(cooldown)
		entered = true
	case wasBad:
		// Still failing: keep it out rather than letting the cooldown lapse
		// while the endpoint is demonstrably broken.
		e.contentBadUntil = now.Add(cooldown)
	}
	failures := e.contentFail
	s.mu.Unlock()

	if entered {
		s.logger.Warn("upstream answers the API root but not content; dropped from the race",
			"upstream", url, "consecutive_failures", failures, "cooldown", cooldown, "error", err)
	}
}

// indexOfLocked returns the index of an endpoint URL, or -1. Callers must hold
// the lock.
func (s *Selector) indexOfLocked(url string) int {
	for i := range s.eps {
		if s.eps[i].url == url {
			return i
		}
	}
	return -1
}

// Elected returns the elected endpoint, or "" if none was ever elected.
func (s *Selector) Elected() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.electedURLLocked()
}

// ElectedForRequest reports the endpoint to use and whether it is healthy.
func (s *Selector) ElectedForRequest() (url string, healthy bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	url = s.electedURLLocked()
	if s.elected >= 0 && s.elected < len(s.eps) {
		healthy = s.eps[s.elected].healthy
	}
	return url, healthy
}

// LastError returns the error from the most recent failing probe.
func (s *Selector) LastError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// LatencyEWMA returns the exponentially weighted moving average of the elected
// endpoint's successful probe durations, or 0 when no probe has succeeded yet.
//
// It is the tie-break weight the selection chooser uses when two candidates'
// content probes measured the same duration. The health probe's latency is a
// proxy for how fast the endpoint serves, which is exactly what the content
// probe measures directly — so it only ever breaks a tie, it never overrides a
// measurement.
func (s *Selector) LatencyEWMA() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.elected < 0 || s.elected >= len(s.eps) {
		return 0
	}
	return s.eps[s.elected].ewma
}

// Failovers returns how many times the election has changed.
func (s *Selector) Failovers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failover
}

// ElectForStartup probes the endpoints in configuration order and elects the
// first one that answers. It must run before a proxy namespace's engine is
// constructed, because handlers.NewApp performs a blocking request against
// <remoteurl>/v2/ and panics when it fails.
//
// A single successful probe is enough to start: the success threshold is a
// runtime stability control, not a startup gate. Failures are recorded as fully
// failed so that a later election cannot pick an endpoint that was already dead
// here.
func (s *Selector) ElectForStartup(ctx context.Context) (string, error) {
	var lastErr error

	for i := range s.eps {
		latency, err := s.probe(ctx, s.eps[i].url)

		s.mu.Lock()
		if err == nil {
			s.eps[i].observeLatency(latency)
			s.eps[i].healthy = true
			s.eps[i].consecSuccess = s.opts.SuccessThreshold
			s.eps[i].consecFail = 0
			s.eps[i].lastErr = nil
			if s.elected >= 0 && s.elected != i {
				s.failover++
			}
			s.elected = i
			s.mu.Unlock()

			s.logger.Info("upstream elected", "upstream", s.eps[i].url, "candidates", len(s.eps))
			return s.eps[i].url, nil
		}

		s.eps[i].healthy = false
		s.eps[i].consecSuccess = 0
		s.eps[i].lastErr = err
		// Jump straight to "known bad": election must not fall back to an
		// endpoint that has just been proven unreachable, otherwise every probe
		// tick would try to rebuild an engine against it.
		if s.eps[i].consecFail < s.opts.FailThreshold {
			s.eps[i].consecFail = s.opts.FailThreshold
		}
		s.lastErr = err
		s.mu.Unlock()

		s.logger.Warn("upstream unreachable", "upstream", s.eps[i].url, "error", err)
		lastErr = err
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no endpoints configured")
	}
	return "", fmt.Errorf("namespace %s: no upstream endpoint is reachable: %w", s.name, lastErr)
}

// Run probes until ctx is cancelled.
func (s *Selector) Run(ctx context.Context) {
	ticker := time.NewTicker(s.opts.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.opts.Enabled && s.hasElection() {
				// Periodic re-election is switched off and this namespace is
				// already serving, so leave it alone.
				continue
			}
			_ = s.ProbeAll(ctx)
		}
	}
}

// ProbeAll probes every endpoint once and re-elects if the election changed.
func (s *Selector) ProbeAll(ctx context.Context) error {
	type result struct {
		latency time.Duration
		err     error
	}

	results := make([]result, len(s.eps))
	var wg sync.WaitGroup
	for i := range s.eps {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			latency, err := s.probe(ctx, s.eps[i].url)
			results[i] = result{latency: latency, err: err}
		}(i)
	}
	wg.Wait()

	s.mu.Lock()
	var firstErr error
	for i := range s.eps {
		e := &s.eps[i]
		if results[i].err == nil {
			e.observeLatency(results[i].latency)
			e.consecSuccess++
			e.consecFail = 0
			e.lastErr = nil
			if !e.healthy && e.consecSuccess >= s.opts.SuccessThreshold {
				e.healthy = true
				s.logger.Info("upstream marked healthy", "upstream", e.url)
			}
		} else {
			e.consecFail++
			e.consecSuccess = 0
			e.lastErr = results[i].err
			if firstErr == nil {
				firstErr = results[i].err
			}
			if e.healthy && e.consecFail >= s.opts.FailThreshold {
				e.healthy = false
				s.logger.Warn("upstream marked unhealthy", "upstream", e.url,
					"consecutive_failures", e.consecFail, "error", results[i].err)
			}
		}
	}
	s.lastErr = firstErr

	before := s.electedURLLocked()
	after := s.electLocked()
	changed := before != after
	s.mu.Unlock()

	if changed {
		if after == "" {
			s.logger.Error("no healthy upstream endpoint", "previous", before, "error", firstErr)
		} else {
			s.logger.Warn("upstream election changed", "from", before, "to", after)
		}
		if s.onElect != nil {
			s.onElect(after)
		}
	}

	return firstErr
}

// hasElection reports whether an endpoint has been elected.
func (s *Selector) hasElection() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.electedURLLocked() != ""
}

// electedURLLocked returns the elected URL. Callers must hold the lock.
func (s *Selector) electedURLLocked() string {
	if s.elected < 0 || s.elected >= len(s.eps) {
		return ""
	}
	return s.eps[s.elected].url
}

// electLocked picks the endpoint to use.
//
// Configuration order is preference order: the first healthy endpoint wins, so
// a cheap local mirror is used again once it recovers. The thresholds provide
// the hysteresis that stops that from flapping.
//
// When nothing is healthy the first endpoint that is not known-bad is chosen,
// so a dead endpoint does not keep receiving traffic. If every endpoint is
// known-bad the previous election is kept — the traffic has to go somewhere,
// and the engine is already built against that endpoint. Callers must hold the
// lock.
func (s *Selector) electLocked() string {
	for i := range s.eps {
		if s.eps[i].healthy {
			return s.setElectedLocked(i)
		}
	}
	for i := range s.eps {
		if !s.eps[i].knownBad(s.opts.FailThreshold) {
			return s.setElectedLocked(i)
		}
	}
	return s.electedURLLocked()
}

// setElectedLocked records a changed election and returns the elected URL.
// Callers must hold the lock.
func (s *Selector) setElectedLocked(i int) string {
	if s.elected != i {
		if s.elected >= 0 {
			s.failover++
		}
		s.elected = i
	}
	return s.eps[i].url
}

// probe checks whether an endpoint speaks the registry v2 protocol. It returns
// how long the probe took, which is what feeds the endpoint's latency average —
// on failure that figure is discarded, since a failure has no service latency.
func (s *Selector) probe(ctx context.Context, base string) (time.Duration, error) {
	start := time.Now()

	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+s.opts.Path, nil)
	if err != nil {
		return time.Since(start), err
	}
	req.Header.Set("User-Agent", "rms/upstream-probe")

	resp, err := s.client.Do(req)
	if err != nil {
		return time.Since(start), err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()

	// 200 is an open registry; 401/403 is a registry that wants credentials.
	// Both prove the endpoint speaks v2.
	switch resp.StatusCode {
	case http.StatusOK, http.StatusUnauthorized, http.StatusForbidden:
		return time.Since(start), nil
	default:
		return time.Since(start), fmt.Errorf("%s: unexpected status %d", base, resp.StatusCode)
	}
}
