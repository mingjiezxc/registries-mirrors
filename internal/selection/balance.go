package selection

import (
	"time"

	"github.com/watsons/rms/internal/config"
)

// balanceSlack is the absolute difference added to the balancing window, so
// that two upstreams answered within milliseconds of each other both stay in
// rotation. The window is a multiplier, and a multiplier of a very small
// latency is still a very small latency: without this, a 0.3ms probe would keep
// a 0.5ms mirror out of the pool, which is measurement noise rather than a
// slower mirror.
const balanceSlack = 2 * time.Millisecond

// poolEntry is one upstream eligible to serve a decision, with the weight the
// smooth weighted round-robin spreads requests by.
type poolEntry struct {
	url    string
	weight float64
}

// buildPool turns the digest-eligible probe results of one namespace into the
// pool that every request served by this decision picks from.
//
// eligible is in configuration order and every entry answered with content;
// fastest is the lowest latency among them. Being eligible — same content — is
// not the same as being worth alternating in, so the window drops the mirrors
// that are far slower than the fastest one: they stay as fallbacks (the
// dispatcher re-selects when the chosen upstream fails) without taking a share
// of the traffic.
//
// mode is a config.Balance* value: BalanceWeighted weights by 1/latency,
// BalanceRoundRobin gives everyone the same weight, and BalanceOff collapses
// the pool to its fastest member.
func buildPool(eligible []probeResult, fastest time.Duration, mode string, window float64) []poolEntry {
	if len(eligible) == 0 {
		return nil
	}

	fastestResult := eligible[0]
	for _, r := range eligible {
		if r.latency < fastestResult.latency {
			fastestResult = r
		}
	}

	if mode == config.BalanceOff || len(eligible) == 1 {
		return []poolEntry{{url: fastestResult.upstream, weight: 1}}
	}

	limit := time.Duration(float64(fastest) * window)
	if limit < fastest+balanceSlack {
		limit = fastest + balanceSlack
	}

	out := make([]poolEntry, 0, len(eligible))
	for _, r := range eligible {
		if r.latency > limit {
			continue
		}
		weight := 1.0
		if mode == config.BalanceWeighted {
			// Weight by inverse latency, in units of one millisecond. Latencies
			// below a millisecond are treated as a millisecond: at that scale
			// the difference between two mirrors is measurement noise, and
			// dividing by it would hand one of them almost all the traffic on
			// the strength of a rounding error.
			latency := r.latency
			if latency < time.Millisecond {
				latency = time.Millisecond
			}
			weight = float64(time.Millisecond) / float64(latency)
		}
		out = append(out, poolEntry{url: r.upstream, weight: weight})
	}

	if len(out) == 0 {
		return []poolEntry{{url: fastestResult.upstream, weight: 1}}
	}
	return out
}

// pickPool chooses one upstream from a pool with a smooth weighted round-robin
// (the algorithm nginx uses), and returns the updated running weights.
//
// Smoothness is the point: plain weighted random or a "send N then switch"
// scheme both produce bursts, which a pull-through cache feels — a client
// fetching an image's layers wants them from the upstream that is fastest right
// now, not from a mirror that won a coin toss. The state is per decision, so it
// is reused by every request the cached decision serves and spreads them.
func pickPool(pool []poolEntry, smooth []float64) (string, []float64) {
	if len(pool) == 0 {
		return "", smooth
	}
	if len(pool) == 1 {
		return pool[0].url, smooth
	}
	if len(smooth) != len(pool) {
		smooth = make([]float64, len(pool))
	}

	total := 0.0
	best := 0
	for i, e := range pool {
		smooth[i] += e.weight
		total += e.weight
		if smooth[i] > smooth[best] {
			best = i
		}
	}
	smooth[best] -= total
	return pool[best].url, smooth
}
