package selection

import (
	"testing"
	"time"

	"github.com/watsons/rms/internal/config"
)

func probeAt(url string, latency time.Duration) probeResult {
	return probeResult{namespace: "docker.io", enginePath: "library/alpine", upstream: url, exists: true, reachable: true, latency: latency}
}

func TestBuildPoolWeightsByInverseLatency(t *testing.T) {
	pool := buildPool([]probeResult{
		probeAt("https://slow", 20*time.Millisecond),
		probeAt("https://fast", 10*time.Millisecond),
	}, 10*time.Millisecond, config.BalanceWeighted, 5)

	if len(pool) != 2 {
		t.Fatalf("pool = %v, want both upstreams", pool)
	}
	if pool[0].url != "https://slow" || pool[1].url != "https://fast" {
		t.Fatalf("pool order = %v, want configuration order preserved", pool)
	}
	// 20ms is half as fast as 10ms, so it carries half the weight.
	if got, want := pool[0].weight*2, pool[1].weight; got != want {
		t.Errorf("weights = %v, want the 20ms mirror to weigh half of the 10ms one", pool)
	}
}

func TestBuildPoolWindowDropsMuchSlowerMirrors(t *testing.T) {
	pool := buildPool([]probeResult{
		probeAt("https://fast", 10*time.Millisecond),
		probeAt("https://outside", 100*time.Millisecond),
	}, 10*time.Millisecond, config.BalanceWeighted, 1.5)

	if len(pool) != 1 || pool[0].url != "https://fast" {
		t.Fatalf("pool = %v, want only the fast mirror inside the window", pool)
	}
}

func TestBuildPoolWindowHasAnAbsoluteSlack(t *testing.T) {
	// A sub-millisecond probe multiplied by the window is still
	// sub-millisecond, which would exclude a mirror that answered a
	// millisecond later — noise, not a slower mirror.
	pool := buildPool([]probeResult{
		probeAt("https://a", 200*time.Microsecond),
		probeAt("https://b", 700*time.Microsecond),
	}, 200*time.Microsecond, config.BalanceWeighted, 1.5)

	if len(pool) != 2 {
		t.Fatalf("pool = %v, want both sub-millisecond mirrors in rotation", pool)
	}
}

func TestBuildPoolModes(t *testing.T) {
	results := []probeResult{
		probeAt("https://a", 10*time.Millisecond),
		probeAt("https://b", 12*time.Millisecond),
	}

	t.Run("round robin weighs everyone equally", func(t *testing.T) {
		pool := buildPool(results, 10*time.Millisecond, config.BalanceRoundRobin, 5)
		if len(pool) != 2 || pool[0].weight != pool[1].weight {
			t.Fatalf("pool = %v, want equal weights", pool)
		}
	})

	t.Run("off keeps only the fastest", func(t *testing.T) {
		pool := buildPool(results, 10*time.Millisecond, config.BalanceOff, 5)
		if len(pool) != 1 || pool[0].url != "https://a" {
			t.Fatalf("pool = %v, want only the fastest upstream", pool)
		}
	})

	t.Run("a single upstream needs no weights", func(t *testing.T) {
		pool := buildPool(results[:1], 10*time.Millisecond, config.BalanceWeighted, 5)
		if len(pool) != 1 || pool[0].weight != 1 {
			t.Fatalf("pool = %v, want one entry of weight 1", pool)
		}
	})
}

func TestPickPoolSpreadsInProportionToWeight(t *testing.T) {
	pool := []poolEntry{{url: "slow", weight: 1}, {url: "fast", weight: 2}}

	var smooth []float64
	seen := map[string]int{}
	for i := 0; i < 30; i++ {
		var url string
		url, smooth = pickPool(pool, smooth)
		seen[url]++
	}

	// Smooth weighted round-robin is deterministic: over 30 picks a 1:2 pool
	// gives exactly 10 and 20, and never more than two of one in a row.
	if seen["fast"] != 20 || seen["slow"] != 10 {
		t.Fatalf("distribution = %v, want the 1:2 weights to hold exactly", seen)
	}
}

func TestPickPoolSingleEntryAndEmptyPool(t *testing.T) {
	if url, _ := pickPool(nil, nil); url != "" {
		t.Errorf("pickPool(nil) = %q, want empty", url)
	}
	if url, _ := pickPool([]poolEntry{{url: "only", weight: 1}}, nil); url != "only" {
		t.Errorf("pickPool(single) = %q, want only", url)
	}
}

func TestPickPoolResetsStateWhenThePoolShrinks(t *testing.T) {
	// A refreshed decision can have a different pool: the rotation state must
	// not be carried over positionally.
	var smooth []float64
	_, smooth = pickPool([]poolEntry{{url: "a", weight: 1}, {url: "b", weight: 1}}, smooth)
	if url, _ := pickPool([]poolEntry{{url: "c", weight: 1}}, smooth); url != "c" {
		t.Fatalf("pickPool = %q, want the single remaining upstream", url)
	}
}
