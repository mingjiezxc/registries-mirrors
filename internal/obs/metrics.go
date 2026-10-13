package obs

import (
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	dockermetrics "github.com/docker/go-metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics implements server.Recorder and exposes the Prometheus endpoint.
//
// It owns a private registry rather than using the global default, so nothing
// else in the process can accidentally register the same names, and the
// embedded registry's own metrics (which live on docker/go-metrics' separate
// registry) are exposed alongside without colliding.
type Metrics struct {
	reg *prometheus.Registry

	requests        *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	cacheHits       *prometheus.CounterVec
	cacheMisses     *prometheus.CounterVec
	elections       *prometheus.CounterVec
	namespaceUp     *prometheus.GaugeVec
	namespaceErrors *prometheus.CounterVec

	upstreamProbes     *prometheus.HistogramVec
	upstreamServed     *prometheus.CounterVec
	upstreamMismatches *prometheus.CounterVec
	selectionDecisions *prometheus.CounterVec

	handler http.Handler
}

// MetricNames are the series rms publishes. They are documented so that
// dashboards and alerts do not have to be reverse engineered from the code.
const (
	metricRequests        = "rms_http_requests_total"
	metricRequestDuration = "rms_http_request_duration_seconds"
	metricCacheHits       = "rms_cache_hits_total"
	metricCacheMisses     = "rms_cache_misses_total"
	metricElections       = "rms_upstream_election_changes_total"
	metricNamespaceUp     = "rms_namespace_up"
	metricNamespaceErrors = "rms_namespace_errors_total"

	metricUpstreamProbes    = "rms_upstream_probe_duration_seconds"
	metricUpstreamServed    = "rms_upstream_wins_total"
	metricUpstreamMismatch  = "rms_upstream_digest_mismatch_total"
	metricSelectionDecision = "rms_selection_decisions_total"
)

// NewMetrics builds the metric set and the handler serving it.
//
// metricsPath is the registry-configured path, for example "/metrics". The
// embedded registry's own series are exposed at the sibling path
// "/registry/metrics".
func NewMetrics(metricsPath string) (*Metrics, error) {
	if metricsPath == "" {
		metricsPath = "/metrics"
	}
	if !strings.HasPrefix(metricsPath, "/") {
		return nil, fmt.Errorf("metrics path %q must start with %q", metricsPath, "/")
	}

	reg := prometheus.NewRegistry()
	m := &Metrics{reg: reg}

	m.requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: metricRequests,
		Help: "Registry API requests served, by listener, namespace, method and status.",
	}, []string{"listener", "namespace", "method", "status"})

	m.requestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    metricRequestDuration,
		Help:    "Registry API request duration in seconds.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 300},
	}, []string{"listener", "namespace", "method"})

	m.cacheHits = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: metricCacheHits,
		Help: "Requests for an object already present in a proxy namespace's local cache, so no content had to be fetched. Tag lookups are counted here when the tag is cached, but distribution revalidates tags against the upstream on every request, so a hit does not imply that no upstream request was made.",
	}, []string{"namespace"})

	m.cacheMisses = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: metricCacheMisses,
		Help: "Requests for an object not present in a proxy namespace's local cache; the engine fetches it from the upstream and caches it.",
	}, []string{"namespace"})

	m.elections = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: metricElections,
		Help: "Upstream election changes for a proxy namespace.",
	}, []string{"namespace", "upstream"})

	m.namespaceUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: metricNamespaceUp,
		Help: "1 when a proxy namespace is a working pull-through cache, 0 when it is degraded (serving cached content only, or unavailable).",
	}, []string{"namespace"})

	m.namespaceErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: metricNamespaceErrors,
		Help: "Namespace lifecycle failures, such as an unreachable upstream.",
	}, []string{"namespace"})

	m.upstreamProbes = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: metricUpstreamProbes,
		Help: "How long an upstream took to answer a content probe, by namespace and upstream. Only probes launched by the selection race are measured; a probe that missed the decision deadline is recorded at the moment the decision gave up on it.",
		// The interesting range is a fast mirror (single-digit ms) to a
		// hanging one (the probe timeout, tens of seconds).
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60},
	}, []string{"namespace", "upstream"})

	m.upstreamServed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: metricUpstreamServed,
		Help: "Requests handed to an upstream, by namespace and upstream. Under load balancing this is the distribution itself; a namespace with one upstream shows that upstream's share of its traffic.",
	}, []string{"namespace", "upstream"})

	m.upstreamMismatches = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: metricUpstreamMismatch,
		Help: "Content probes whose digest disagreed with the namespace's authority upstream, so the answer was not served. A non-zero rate means a mirror of that registry is stale or has been replaced.",
	}, []string{"namespace", "upstream"})

	m.selectionDecisions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: metricSelectionDecision,
		Help: "Selection decisions by where the answer came from: probe, decision-cache, cache (a namespace already held it on disk) or affinity (a digest-only request following the last manifest decision).",
	}, []string{"source"})

	reg.MustRegister(
		m.requests, m.requestDuration, m.cacheHits, m.cacheMisses,
		m.elections, m.namespaceUp, m.namespaceErrors,
		m.upstreamProbes, m.upstreamServed, m.upstreamMismatches, m.selectionDecisions,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m.handler = m.buildHandler(metricsPath)

	return m, nil
}

// buildHandler serves rms's own metrics at metricsPath and the embedded
// registry's series at the sibling /registry/metrics path. Anything else is a
// 404: a scrape endpoint must not answer every path.
func (m *Metrics) buildHandler(metricsPath string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(metricsPath, promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{Registry: m.reg}))

	// The embedded distribution registry publishes its own series (blob and
	// manifest pull/push counters) on a separate registry.
	registryPath := path.Join(path.Dir(metricsPath), "registry", "metrics")
	mux.Handle(registryPath, dockermetrics.Handler())

	return mux
}

// Handler returns the HTTP handler serving all metrics.
func (m *Metrics) Handler() http.Handler { return m.handler }

// Request implements server.Recorder.
func (m *Metrics) Request(listener, ns, method string, status int, d time.Duration) {
	if ns == "" {
		ns = "none"
	}
	if listener == "" {
		listener = "<multi>"
	}

	m.requests.WithLabelValues(listener, ns, method, strconv.Itoa(status)).Inc()
	m.requestDuration.WithLabelValues(listener, ns, method).Observe(d.Seconds())
}

// CacheHit implements server.Recorder.
func (m *Metrics) CacheHit(ns string) { m.cacheHits.WithLabelValues(ns).Inc() }

// CacheMiss implements server.Recorder.
func (m *Metrics) CacheMiss(ns string) { m.cacheMisses.WithLabelValues(ns).Inc() }

// UpstreamElectionChanged implements registry.Recorder.
func (m *Metrics) UpstreamElectionChanged(ns, upstream string) {
	if upstream == "" {
		upstream = "none"
	}
	m.elections.WithLabelValues(ns, upstream).Inc()
}

// NamespaceDegraded implements registry.Recorder.
func (m *Metrics) NamespaceDegraded(ns string, err error) {
	m.namespaceUp.WithLabelValues(ns).Set(0)
	if err != nil {
		m.namespaceErrors.WithLabelValues(ns).Inc()
	}
}

// NamespaceReady implements registry.Recorder.
func (m *Metrics) NamespaceReady(ns string) { m.namespaceUp.WithLabelValues(ns).Set(1) }

// UpstreamProbed implements selection.Observer.
func (m *Metrics) UpstreamProbed(ns, upstream string, _ bool, latency time.Duration) {
	m.upstreamProbes.WithLabelValues(ns, upstream).Observe(latency.Seconds())
}

// UpstreamServed implements selection.Observer.
func (m *Metrics) UpstreamServed(ns, upstream string) {
	m.upstreamServed.WithLabelValues(ns, upstream).Inc()
}

// UpstreamDigestMismatch implements selection.Observer.
func (m *Metrics) UpstreamDigestMismatch(ns, upstream string) {
	m.upstreamMismatches.WithLabelValues(ns, upstream).Inc()
}

// DecisionSource implements selection.Observer.
func (m *Metrics) DecisionSource(source string) {
	if source == "" {
		source = "none"
	}
	m.selectionDecisions.WithLabelValues(source).Inc()
}

// SetBuildInfo publishes the running version as a labelled info gauge.
func (m *Metrics) SetBuildInfo(version, commit string) {
	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "rms_build_info",
		Help: "Build information; the value is always 1.",
	}, []string{"version", "commit"})
	info.WithLabelValues(version, commit).Set(1)
	m.reg.MustRegister(info)
}
