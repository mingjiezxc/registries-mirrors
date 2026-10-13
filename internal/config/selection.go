package config

import "time"

// Load-balancing modes for the upstream pool of one namespace.
//
// Balancing only ever happens inside a namespace, and only between upstreams
// that agree on the content digest of the reference being served: two mirrors
// that disagree about what a tag points at must not be alternated. See the
// selection package for the algorithm.
const (
	// BalanceWeighted spreads requests over the eligible upstreams in
	// proportion to 1/latency, so the fastest mirror carries the most traffic
	// without the others sitting idle. This is the default.
	BalanceWeighted = "weighted"
	// BalanceRoundRobin spreads them evenly, ignoring latency.
	BalanceRoundRobin = "round_robin"
	// BalanceOff never spreads: the fastest eligible upstream serves every
	// request until the decision is refreshed.
	BalanceOff = "off"
)

// Selection configures how a read request picks the proxy namespace that serves
// it and, within that namespace, which of its upstreams does the fetching.
//
// There is no `namespaces.default` any more: every unqualified request is
// resolved by probing the configured proxy namespaces concurrently and serving
// it from the fastest one that actually has the requested repository. Inside a
// namespace, every configured upstream is probed concurrently as well, the
// fastest one wins, and upstreams that serve identical content share the load.
// The knobs below bound that decision.
type Selection struct {
	// ProbeTTL is how long a decision for one (repository, reference) is reused
	// before it is probed again. It is also the repository affinity TTL: a blob
	// request carries no repository name, so it follows the decision made for
	// the manifest of the same repository, and that entry expires with probe_ttl.
	//
	// It bounds how often the fan-out happens. Within the TTL the chosen
	// namespace and its upstream pool are reused, so a pull that fetches many
	// layers does not multiply upstream connections by the number of layers.
	ProbeTTL Duration `yaml:"probe_ttl"`
	// ProbeTimeout bounds one candidate probe. It also bounds how long a
	// request waits for the decision when nothing is cached, so it should stay
	// in the same order as the upstream health timeout.
	ProbeTimeout Duration `yaml:"probe_timeout"`
	// DecisionDeadline is a soft deadline on the whole decision: it is armed
	// when the first probe of the race answers, and when it fires the decision
	// proceeds with the answers collected so far instead of waiting for the
	// slowest mirror. Probes that missed it are reported as skipped, never as
	// an answer — a mirror that did not answer in time must not turn a live
	// repository into a 404, and must not become the digest authority either.
	//
	// 0 waits for every probe (bounded by probe_timeout each). The trade-off is
	// decision latency on the cold path: with one dead mirror, waiting for all
	// costs probe_timeout per (repository, reference) that is not cached, while
	// a deadline costs at most decision_deadline and loses the digest gate for
	// a namespace whose authority upstream was the slow one.
	DecisionDeadline Duration `yaml:"decision_deadline"`
	// Balance is the default load-balancing mode for proxy namespaces:
	// BalanceWeighted, BalanceRoundRobin or BalanceOff. A namespace can
	// override it with its own `balance` key.
	Balance string `yaml:"balance"`
	// BalanceWindow is how much slower than the fastest eligible upstream an
	// upstream may be and still receive load-balanced traffic, as a multiplier.
	// 1.5 means "within 50% of the fastest". Upstreams outside the window are
	// kept as fallbacks only: they are not alternated in, so a mirror that is
	// an order of magnitude slower never gets a share of the traffic.
	BalanceWindow float64 `yaml:"balance_window"`
}

// Defaults for namespaces.selection.
const (
	DefaultProbeTTL     = 30 * time.Second
	DefaultProbeTimeout = 10 * time.Second
	// DefaultBalanceWindow keeps balancing to upstreams within 1.5x of the
	// fastest one.
	DefaultBalanceWindow = 1.5
)

// withDefaults fills the zero values.
func (s Selection) withDefaults() Selection {
	if s.ProbeTTL == 0 {
		s.ProbeTTL = Duration(DefaultProbeTTL)
	}
	if s.ProbeTimeout == 0 {
		s.ProbeTimeout = Duration(DefaultProbeTimeout)
	}
	if s.Balance == "" {
		s.Balance = BalanceWeighted
	}
	if s.BalanceWindow == 0 {
		s.BalanceWindow = DefaultBalanceWindow
	}
	return s
}

// Std returns the probe TTL as a time.Duration.
func (s Selection) Std() time.Duration { return s.ProbeTTL.Std() }

// ValidBalance reports whether mode names a known load-balancing mode.
func ValidBalance(mode string) bool {
	switch mode {
	case BalanceWeighted, BalanceRoundRobin, BalanceOff:
		return true
	}
	return false
}
