// Package selection decides which proxy namespace serves an unqualified
// request — a registry v2 request whose path carries no namespace, such as
// GET /v2/library/alpine/manifests/latest.
//
// There is no configured default namespace. Every unqualified request is
// resolved by probing the configured proxy namespaces concurrently — and every
// upstream of each of them, so the fastest mirror of the namespace that really
// has the content wins:
//
//   - a candidate "has" the repository when its upstream answers the probe for
//     that repository and reference with a 2xx status. The probe path is the
//     real content path (/v2/<repo>/manifests/<ref>), not /v2/, because an
//     upstream whose /v2/ answers can still fail to serve content — that exact
//     gap is why upstream failover is not enough here;
//   - among the candidates that have it, the fastest probe wins. Ties are
//     broken by namespace name so the outcome is deterministic;
//   - when no candidate has it the caller must answer 404 NAME_UNKNOWN, and
//     when every candidate is unreachable it must answer 503 UNAVAILABLE.
//
// The decision is cached per (repository, reference) for the configured probe
// TTL, and a repository-level affinity entry is written alongside it so that
// blob requests — whose URL carries only a digest — follow the manifest's
// decision.
package selection

import (
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/watsons/rms/internal/config"
)

// Errors returned by Decide. Callers map them onto registry v2 responses.
var (
	// ErrNotFound means the candidates rms could ask all said no: none of them
	// has the requested repository and reference, and none of them held it
	// cached either.
	//
	// What counts as a "no" is a refusal, including a refusal to authenticate:
	// with no credentials of its own, a registry that wants them is telling an
	// anonymous cache that it cannot serve the repository, which is a definite
	// answer rather than an outage (measured: ghcr.io, gcr.io and quay.io
	// refuse rms's anonymous requests as a matter of course). The refusals are
	// 401/403/404/400/410 and the like, plus a 405 that survives the GET
	// degradation.
	//
	// What does *not* count is a 5xx, a 429, or a request that never got an
	// answer at all: none of them says anything about the repository, and
	// turning one into a 404 would tell a client that an image it may well have
	// does not exist — the mirror image of the bug that made a mistyped image
	// answer 503.
	//
	// A candidate that never answered does not stand in the way of the ones
	// that did, though: a namespace being unreachable does not make a
	// repository exist, so it must not turn every uncached request into a 503 —
	// clients retry a 503 for ever, while a 404 costs only the case where the
	// silent candidate was the one holding content that was unreachable anyway.
	ErrNotFound = errors.New("no configured namespace has this repository and reference")
	// ErrUnreachable means the answer is not knowable right now: nobody held
	// the reference in its cache, and no candidate produced an answer — no
	// namespace is serving it, every probe failed at the transport level or
	// timed out, or every upstream (manifest path or token endpoint) answered
	// 5xx or 429.
	//
	// It is deliberately reserved for "nobody answered at all": that is the
	// honest "I do not know", and the only case in which a 404 would be a claim
	// rms cannot back.
	ErrUnreachable = errors.New("no configured namespace is reachable")
)

// Candidate is one proxy namespace that may serve an unqualified request.
type Candidate struct {
	// Namespace is the configured proxy namespace name, e.g. "docker.io".
	Namespace string
	// EnginePath is the path the namespace's registry engine must see, with
	// the namespace-specific repository rules already applied — for example
	// "library/alpine" for a single-segment repository in a namespace with
	// official_library enabled.
	EnginePath string
}

// Target is the namespace that serves the request.
type Target struct {
	// Namespace is the winning proxy namespace.
	Namespace string
	// EnginePath is the path to hand to that namespace's engine.
	EnginePath string
	// Upstream is the endpoint of Namespace this particular request must be
	// served by. It is empty for a decision that was answered from the local
	// disk or from the namespace's own election without probing — there is no
	// measurement to choose from — and the caller then uses the namespace's
	// sticky engine.
	Upstream string
	// ProbeLatency is how long the winning probe took. It is zero for a cached
	// decision.
	ProbeLatency time.Duration
}

// sameDecision reports whether two targets describe the same routing decision,
// which is what the affinity bookkeeping compares. Upstream is deliberately not
// part of it: a decision's pool rotates between requests, and the affinity
// means "this repository belongs to this namespace", not "to this endpoint".
func (t Target) sameDecision(o Target) bool {
	return t.Namespace == o.Namespace && t.EnginePath == o.EnginePath
}

// Upstream resolves a namespace to the upstream endpoints that should be probed.
// It is implemented by *registry.Manager.
type Upstream interface {
	// Serving lists the proxy namespaces that can serve a read right now,
	// sorted. Namespaces serving cached content only are included: they answer
	// from disk without an upstream.
	Serving() []string
	// UpstreamsFor lists a namespace's upstream endpoints that a content race
	// may probe, in configuration order. That order is the digest authority
	// order: the first endpoint that answers with content defines what the
	// requested reference is, and a mirror that disagrees with it is not served
	// and not balanced.
	//
	// An empty list means the namespace has no upstream to ask — it is serving
	// cached content only — and the chooser treats it as a candidate that
	// answered nothing rather than as a failure.
	UpstreamsFor(namespace string) []string
	// ObserveContentProbe folds the outcome of one content probe into an
	// endpoint's health state, so a mirror whose API root answers while its
	// manifests and blobs do not is dropped from future races.
	//
	// answered is true whenever the endpoint produced a usable answer about the
	// content, including a refusal ("I do not have that repository").
	ObserveContentProbe(namespace, upstream string, answered bool, latency time.Duration, err error)
	// ElectedUpstream returns the upstream URL currently elected for a
	// namespace, and whether the namespace has one at all. A namespace serving
	// cached content only has none.
	//
	// This is the fallback path, not the racing one: it is what a cached
	// decision with no pool of its own falls back to.
	ElectedUpstream(namespace string) (url string, ok bool)
	// UpstreamCredentials returns the upstream user and password configured for
	// a namespace (both empty when the upstream is public).
	UpstreamCredentials(namespace string) (user, pass string)
	// CachedManifest reports whether a namespace already holds this repository
	// and reference on local disk.
	//
	// repo is the repository as that namespace's storage and engines see it —
	// the candidate's EnginePath — which is not always the name the client
	// wrote. A cached manifest is proof of existence that costs one stat and
	// works with no upstream at all, so it outranks every probe: it is what
	// keeps already-cached images pullable while an upstream is down.
	CachedManifest(namespace, repo, ref string) bool
	// CachedBlob reports whether a namespace already holds this blob on local
	// disk.
	//
	// Blob data is content-addressed and stored once per namespace, so unlike a
	// manifest this needs no repository name — which is what makes it usable
	// for a request that carries only a digest.
	CachedBlob(namespace string, dgst digest.Digest) bool
}

// Observer receives what a Chooser learned while deciding. It is optional: a
// nil Observer changes nothing but turns the metrics off.
//
// Every method is called on the request path, so an implementation must not
// block: rms's own is a handful of Prometheus counter increments.
type Observer interface {
	// UpstreamProbed records one content probe: how long the upstream took to
	// answer about the content, and whether it answered at all. A refusal
	// counts as an answer — the question was answered, the answer was "no".
	UpstreamProbed(namespace, upstream string, answered bool, latency time.Duration)
	// UpstreamServed records which upstream a decision handed a request to.
	// With load balancing on, its distribution is the balancing itself.
	UpstreamServed(namespace, upstream string)
	// UpstreamDigestMismatch records that an upstream answered with content
	// that disagrees with its namespace's authority upstream, so it was not
	// served. A non-zero rate means a mirror of that registry is stale.
	UpstreamDigestMismatch(namespace, upstream string)
	// DecisionSource records where a decision came from: sourceProbe,
	// sourceDecisionCache, sourceCache or sourceAffinity.
	DecisionSource(source string)
}

// Options configures a Chooser.
type Options struct {
	// Client performs the probes. It must be the outbound client the registry
	// engines use, so probes honour the configured proxy and timeouts.
	Client *http.Client
	// TTL is how long a decision is reused. Zero means config.DefaultProbeTTL.
	TTL time.Duration
	// Timeout bounds a single probe. Zero means config.DefaultProbeTimeout.
	Timeout time.Duration
	// DecisionDeadline is the soft deadline on a whole decision: it is armed by
	// the first probe that answers, and when it fires the decision proceeds
	// with what it has. Zero waits for every probe. See config.Selection.
	DecisionDeadline time.Duration
	// Balance reports a namespace's load-balancing mode (a config.Balance*
	// value). Nil means every namespace uses config.BalanceWeighted.
	Balance func(namespace string) string
	// BalanceWindow is the eligibility window for load balancing, as a
	// multiplier of the fastest eligible upstream's latency. Zero means
	// config.DefaultBalanceWindow.
	BalanceWindow float64
	// Observer receives decision and probe outcomes. May be nil.
	Observer Observer
	// Logger receives decision events. A nil value uses slog.Default().
	Logger *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.Client == nil {
		o.Client = http.DefaultClient
	}
	if o.TTL <= 0 {
		o.TTL = config.DefaultProbeTTL
	}
	if o.Timeout <= 0 {
		o.Timeout = config.DefaultProbeTimeout
	}
	if o.BalanceWindow <= 0 {
		o.BalanceWindow = config.DefaultBalanceWindow
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// balanceMode returns the load-balancing mode for a namespace.
func (c *Chooser) balanceMode(namespace string) string {
	if c.opts.Balance == nil {
		return config.BalanceWeighted
	}
	if mode := c.opts.Balance(namespace); config.ValidBalance(mode) {
		return mode
	}
	return config.BalanceWeighted
}

// Chooser resolves unqualified requests. It is safe for concurrent use.
type Chooser struct {
	up     Upstream
	opts   Options
	logger *slog.Logger

	// mu guards the two caches below. Both are keyed by the repository name the
	// client wrote, so a manifest request and a blob request for the same
	// repository meet on the same entry.
	mu sync.Mutex
	// decisions maps (repo, ref) to the namespace chosen for it, until TTL.
	decisions map[string]cachedDecision
	// affinity maps repo to the namespace chosen for its most recent manifest,
	// which is how a blob request — whose path carries only a digest — follows
	// that manifest. It expires with probe_ttl.
	affinity map[string]cachedDecision
	// recent is the most recent manifest decision of all. A blob request that
	// names no repository cannot look itself up by name, so this is the one it
	// follows — see DecideBlob.
	recent cachedDecision
}

// New builds a Chooser over the given upstream resolver.
func New(up Upstream, opts Options) *Chooser {
	opts = opts.withDefaults()
	return &Chooser{
		up:        up,
		opts:      opts,
		logger:    opts.Logger,
		decisions: map[string]cachedDecision{},
		affinity:  map[string]cachedDecision{},
	}
}

// Decide picks the namespace that serves repo at ref among candidates, probing
// them concurrently. It is implemented in probe.go.
//
// err is ErrNotFound when every reachable candidate answered "no such
// repository", ErrUnreachable when none could be probed, and nil otherwise.
//
// repo is the repository name as the client wrote it, and ref a tag, a digest,
// or "" for a request that carries no reference (a blob). The repository the
// probe asks about is each candidate's EnginePath, which already carries that
// namespace's rules.
