package selection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	godigest "github.com/opencontainers/go-digest"
)

// probeUserAgent identifies a selection probe in upstream access logs. It is
// deliberately different from the health probe's "rms/upstream-probe": the two
// ask different questions (does this upstream speak v2 vs does this upstream
// have this repository), and an operator reading a registry's logs needs to
// tell them apart.
const probeUserAgent = "rms/selection-probe"

// maxDiscardBody bounds how much of a response body is read before the
// connection is closed. Manifest HEAD responses have no body; the GET fallback
// does, and a probe must never buffer a manifest.
const maxDiscardBody = 1 << 16

// maxTokenBody bounds the token endpoint's response body.
const maxTokenBody = 1 << 20

// maxManifestBody bounds the manifest a digest fallback will read. A manifest
// is a small JSON document; the bound only exists so that a mirror answering
// the manifest path with something else cannot make rms buffer it.
const maxManifestBody = 8 << 20

// manifestAccept is the Accept a real client sends when it asks for a manifest,
// and the probe has to send it too. These are exactly the four media types the
// embedded registry's own manifest handler recognises (distribution's
// registry/handlers/manifests.go maps these four onto the manifest schemas it
// supports); the values are spelled out here rather than taken from the
// image-spec constants so this package keeps depending on internal/config
// alone.
//
// It is not optional politeness. A registry that stores OCI manifests answers a
// manifest request whose Accept does not list them with 404 NAME_UNKNOWN — "OCI
// manifest found, but accept header does not support OCI manifests" — so a
// probe that omits this header reports an image that exists as absent. Measured
// against real upstreams for astral-sh/uv:latest: ghcr.io answers 404 without
// the header and 200 with it, while Docker Hub and quay.io answer 200 either
// way. A wrong "absent" is the worst outcome this package can produce: the
// caller answers 404 for a repository that exists and the client caches that,
// or another namespace silently serves a same-named repository instead.
const manifestAccept = "application/vnd.oci.image.index.v1+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.docker.distribution.manifest.v2+json"

// decisionKey builds the cache key for one (repository, reference) decision.
// NUL cannot appear in either, so the two parts cannot be confused.
func decisionKey(repo, ref string) string { return repo + "\x00" + ref }

// cachedDecision is one entry of the decision cache.
type cachedDecision struct {
	target  Target
	expires time.Time
	// pool is the set of upstreams eligible to serve this decision: same
	// digests as the authority upstream, within the balancing window. It is
	// empty for a decision that never probed (a disk hit), in which case the
	// target carries no upstream either and the caller falls back to the
	// namespace's sticky engine.
	pool []poolEntry
	// smooth is the running weight of pickPool's smooth weighted round-robin,
	// so a cached decision keeps spreading requests instead of pinning the
	// first upstream it chose.
	smooth []float64
}

// pick returns the target with an upstream chosen from its pool, updating the
// entry's rotation state. Callers must hold the chooser lock.
func (d *cachedDecision) pick() Target {
	t := d.target
	t.ProbeLatency = 0
	if len(d.pool) > 0 {
		t.Upstream, d.smooth = pickPool(d.pool, d.smooth)
	}
	return t
}

// probeResult is what one candidate's probe produced.
type probeResult struct {
	namespace  string
	enginePath string
	upstream   string

	// skipped marks a pair that was never asked: a namespace serving cached
	// content only has no upstream, and a probe that missed a decision deadline
	// is abandoned rather than judged. It is neither reachable nor unreachable:
	// nothing was answered.
	skipped bool
	// skipReason explains a skipped probe on the debug line.
	skipReason string

	// reachable reports that the candidate gave a definitive answer: it
	// answered, and — when it challenged us — we completed the challenge
	// first. It separates "this registry said no" from "we do not know", which
	// Decide maps onto ErrNotFound and ErrUnreachable respectively.
	//
	// An unanswered 401 is deliberately not definitive. A registry that
	// demands authentication and does not get it tells us nothing about
	// whether it has the repository, and reporting that as "does not exist"
	// would let clients cache a 404 for an image that exists.
	reachable bool
	// exists reports that the upstream answered 2xx for this repository and
	// reference — the only answer that means "I can serve this".
	exists bool

	// digest is the content digest of what the upstream served, and
	// digestKnown whether it could be established. It is what makes "same hash"
	// a decidable question: two upstreams of one namespace may be alternated
	// only when they agree on the bytes, and the first upstream in
	// configuration order that answers defines what those bytes are.
	//
	// For a blob (and for a digest reference) the digest is the requested one,
	// so it is always known. For a tag it comes from the upstream's
	// Docker-Content-Digest header, or from hashing the manifest rms fetched
	// when the header is missing.
	digest      string
	digestKnown bool

	// status is the last HTTP status seen, 0 when no request got an answer.
	status int
	// path is the request path this probe actually asked for. It is reported on
	// the per-probe debug line because "which path did the probe ask?" is the
	// question behind a whole class of defects here — a manifest reference
	// asked on the manifests path, a blob digest asked there too, a repository
	// rule applied to the wrong candidate.
	path string
	// reason is a short, log-safe explanation of a non-yes outcome
	// ("redirect", "not-manifest-content:text/html", "timeout", ...), so an
	// operator can tell why a candidate lost without turning on debug logging.
	reason string
	// latency is the whole probe's duration, measured even when it failed.
	latency time.Duration
	// startedAt is when the probe was launched, so the report can show that
	// the candidates really were probed in parallel.
	startedAt time.Time
	err       error
}

// Decide picks the namespace that serves repo at ref among candidates.
//
// It looks in two places, in this order:
//
//  1. each candidate's local cache, with one stat per candidate. A manifest
//     already on disk is proof that the namespace can serve it, and it costs
//     nothing — so a cache hit wins outright and nothing is probed. This is
//     what keeps already-cached images pullable while every upstream is down,
//     including from a namespace serving cached content only.
//  2. the candidates' upstreams, probed concurrently, with the fastest
//     definitive "yes" winning.
//
// err is ErrNotFound only when every candidate gave a definitive "no";
// ErrUnreachable when the answer is not knowable — no cache hit anywhere and
// no candidate that could be asked. ref must be a tag or a digest: a request
// that carries no reference at all (a blob) resolves through DecideBlob or
// DecideBlobInRepo instead.
func (c *Chooser) Decide(ctx context.Context, candidates []Candidate, repo, ref string) (Target, error) {
	return c.resolve(ctx, lookup{
		repo:     repo,
		probeRef: ref,
		cacheRef: ref,
		op:       opManifests,
		kind:     kindManifest,
	}, candidates, func() (Target, bool) {
		// The local cache is consulted before anything is sent anywhere.
		return c.cachedOnDisk(candidates, ref)
	})
}

// DecideBlobInRepo resolves an unqualified blob request whose path DOES carry
// the repository: GET /v2/<repo>/blobs/<digest>.
//
// The probe asks the blob path, not the manifest path — a layer digest is not a
// manifest reference, so asking /v2/<repo>/manifests/<layer-digest> gets a 404
// from every candidate for a blob that is sitting right there. Measured on the
// 103 deployment: a client fetching a layer of library/busybox got a 404 with
// every candidate reported "(absent)", because the dispatcher handed the blob
// path to Decide and the probe asked for a manifest.
//
// Everything else matches Decide: the candidates are probed concurrently, only
// a 2xx counts as "has it", the fastest wins, the decision is cached for the
// probe TTL, and the repository-level affinity is written for the same
// repository.
func (c *Chooser) DecideBlobInRepo(ctx context.Context, candidates []Candidate, repo, digest string) (Target, error) {
	if repo == "" || digest == "" {
		// Nothing to ask: a blob path needs both parts to be a question.
		return Target{}, ErrUnreachable
	}
	return c.resolve(ctx, lookup{
		repo:     repo,
		probeRef: digest,
		cacheRef: blobCacheRef(digest),
		op:       opBlobs,
		kind:     kindBlob,
	}, candidates, nil)
}

// DecideQualified resolves a read whose path names its namespace explicitly —
// GET /v2/docker.io/library/alpine/manifests/latest — over that namespace's
// upstreams.
//
// It runs the same race and keeps the same decision cache as an unqualified
// request, restricted to one namespace: a deployment where docker.io is served
// by a local mirror and the official registry should get the fastest of the two
// whichever way the client asked for it, and an operator who wrote the
// namespace into the path is expressing a preference for the namespace, not for
// a particular mirror of it.
//
// Its decision is cached under a namespace-scoped key. Reusing the unqualified
// entry would be wrong: that entry says "this is the namespace that serves the
// repository", which the caller here has already decided for itself.
func (c *Chooser) DecideQualified(ctx context.Context, namespace, repo, ref string) (Target, error) {
	if namespace == "" || repo == "" || ref == "" {
		return Target{}, ErrUnreachable
	}
	return c.resolve(ctx, lookup{
		repo:      repo,
		namespace: namespace,
		probeRef:  ref,
		cacheRef:  ref,
		op:        opManifests,
		kind:      kindManifest,
	}, []Candidate{{Namespace: namespace, EnginePath: repo}}, func() (Target, bool) {
		return c.cachedOnDisk([]Candidate{{Namespace: namespace, EnginePath: repo}}, ref)
	})
}

// Operations a probe can ask about, and the kinds they are reported as.
const (
	opManifests = "/manifests/"
	opBlobs     = "/blobs/"

	kindManifest = "manifest"
	kindBlob     = "blob"
)

// blobCacheRef namespaces a blob decision inside the (repo, ref) key space, so
// a blob whose digest happens to equal a manifest reference cannot collide with
// that manifest's decision.
func blobCacheRef(digest string) string { return "blobs/" + digest }

// lookup is one thing to resolve: the repository the client named, the path
// segment to probe, and how the outcome is keyed and logged.
type lookup struct {
	// repo is the repository as the client wrote it: cache key, log field and
	// token scope.
	repo string
	// namespace, when set, scopes the decision to one namespace: a request whose
	// path already names its namespace asks only that namespace's upstreams, and
	// its decision must not be reused for an unqualified request (which races
	// every namespace and may well pick another one).
	namespace string
	// probeRef is the path segment after the operation — a tag, a digest.
	probeRef string
	// cacheRef is the reference the decision is cached under, which differs
	// from probeRef for blobs.
	cacheRef string
	// op is opManifests or opBlobs.
	op string
	// kind is what the decision log calls this: kindManifest or kindBlob.
	kind string
}

// cacheRepo is the repository part of this lookup's decision key. The NUL
// separator cannot appear in a repository or namespace name, so a qualified
// decision can never collide with an unqualified one.
func (q lookup) cacheRepo() string {
	if q.namespace == "" {
		return q.repo
	}
	return qualifiedRepo(q.namespace, q.repo)
}

// qualifiedRepo is the affinity key of a namespace-qualified decision.
func qualifiedRepo(namespace, repo string) string { return namespace + "\x00" + repo }

// resolve performs a lookup: cache, then an optional local-disk check, then a
// concurrent race over every (namespace, upstream) pair. onDisk may be nil when
// the caller has no local evidence to offer.
func (c *Chooser) resolve(ctx context.Context, q lookup, candidates []Candidate, onDisk func() (Target, bool)) (Target, error) {
	if len(candidates) == 0 || c.up == nil {
		return Target{}, ErrUnreachable
	}

	if target, ok := c.cached(q); ok {
		c.observeDecision(sourceDecisionCache)
		c.observeServed(target)
		c.logDecision(q, target, 0, nil, sourceDecisionCache)
		return target, nil
	}

	if onDisk != nil {
		if target, ok := onDisk(); ok {
			c.store(q, target, nil)
			c.observeDecision(sourceCache)
			c.observeServed(target)
			c.logDecision(q, target, 0, nil, sourceCache)
			return target, nil
		}
	}

	pairs := c.pairs(candidates)
	results := c.race(ctx, pairs, q)

	for _, r := range results {
		c.logProbe(q, r)
		if r.skipped || r.upstream == "" {
			// Nothing was asked, so there is no health verdict to record.
			continue
		}
		if c.opts.Observer != nil {
			c.opts.Observer.UpstreamProbed(r.namespace, r.upstream, r.reachable, r.latency)
		}
		// The content probe is the only evidence that can tell a mirror which
		// answers /v2/ from one that can actually serve content.
		c.up.ObserveContentProbe(r.namespace, r.upstream, r.reachable, r.latency, r.err)
	}

	target, pool, err := c.pickPool(results)
	if err != nil {
		c.observeDecision(sourceProbe)
		c.logDecision(q, Target{}, len(pairs), timings(results), sourceProbe)
		return Target{}, err
	}

	c.store(q, target, pool)
	c.observeDecision(sourceProbe)
	c.observeServed(target)
	c.logDecision(q, target, len(pairs), timings(results), sourceProbe)
	return target, nil
}

// observeDecision reports where a decision came from.
func (c *Chooser) observeDecision(source string) {
	if c.opts.Observer != nil {
		c.opts.Observer.DecisionSource(source)
	}
}

// observeServed reports which upstream a request was handed to.
func (c *Chooser) observeServed(target Target) {
	if c.opts.Observer != nil && target.Namespace != "" && target.Upstream != "" {
		c.opts.Observer.UpstreamServed(target.Namespace, target.Upstream)
	}
}

// pair is one thing to probe: a namespace and one of its upstreams. A pair with
// an empty upstream is a namespace that has nothing to ask (it is serving
// cached content only).
type pair struct {
	cand     Candidate
	upstream string
}

// pairs expands every candidate namespace into one probe per upstream it may
// use.
//
// Namespaces are ordered by name so the outcome never depends on map order, and
// the upstreams inside one keep the configuration order UpstreamsFor reports —
// which is the digest authority order, so "the first one that answers defines
// the bytes" is the operator's configured order and not an accident of
// goroutine scheduling.
func (c *Chooser) pairs(candidates []Candidate) []pair {
	ordered := append([]Candidate(nil), candidates...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Namespace < ordered[j].Namespace })

	out := make([]pair, 0, len(ordered))
	for _, cand := range ordered {
		upstreams := c.up.UpstreamsFor(cand.Namespace)
		if len(upstreams) == 0 {
			out = append(out, pair{cand: cand})
			continue
		}
		for _, upstream := range upstreams {
			out = append(out, pair{cand: cand, upstream: upstream})
		}
	}
	return out
}

// race probes every pair concurrently and returns one result per pair.
//
// All probes share one deadline and start together, so the decision costs the
// slowest probe rather than their sum — and the fan-out is the point: one
// request becomes as many upstream connections as there are pairs, and the
// fastest one that actually has the content wins.
//
// When DecisionDeadline is set it is armed by the first probe that answers: from
// then on the decision waits at most that much longer for the rest. Probes that
// miss it are reported as skipped with a reason, never as an answer — a mirror
// that stayed silent must not decide that a repository is absent, and must not
// become the digest authority either.
//
// ctx is the request's context, so a client that hangs up stops the probes.
func (c *Chooser) race(ctx context.Context, pairs []pair, q lookup) []probeResult {
	results := make([]probeResult, len(pairs))
	answered := make([]bool, len(pairs))
	pending := 0
	for i, p := range pairs {
		results[i] = probeResult{
			namespace:  p.cand.Namespace,
			enginePath: p.cand.EnginePath,
			upstream:   p.upstream,
			startedAt:  time.Now(),
		}
		if p.upstream == "" {
			results[i].skipped = true
			results[i].skipReason = "no upstream configured"
			continue
		}
		pending++
	}
	if pending == 0 {
		return results
	}

	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type outcome struct {
		i int
		r probeResult
	}
	ch := make(chan outcome, pending)
	for i, p := range pairs {
		if p.upstream == "" {
			continue
		}
		go func(i int, p pair) { ch <- outcome{i, c.probe(raceCtx, p.cand, p.upstream, q)} }(i, p)
	}

	var (
		timer     *time.Timer
		timerC    <-chan time.Time
		received  int
		deadlined bool
	)
	for received < pending {
		select {
		case o := <-ch:
			results[o.i] = o.r
			answered[o.i] = true
			received++
			if timer == nil && c.opts.DecisionDeadline > 0 && received < pending {
				timer = time.NewTimer(c.opts.DecisionDeadline)
				timerC = timer.C
			}
		case <-timerC:
			deadlined = true
			received = pending
		}
	}
	if timer != nil {
		timer.Stop()
	}
	if deadlined {
		for i := range results {
			if results[i].upstream == "" || answered[i] {
				continue
			}
			results[i].skipped = true
			results[i].skipReason = "decision deadline"
			results[i].latency = time.Since(results[i].startedAt)
		}
	}
	return results
}

// cachedOnDisk returns the first candidate in namespace-name order whose local
// storage already holds ref.
//
// Name order, not probe order: no probe happens here, so there is no measured
// latency to rank by, and a deterministic rule is what keeps two equal mirrors
// from alternating. The check itself is a stat per candidate.
func (c *Chooser) cachedOnDisk(candidates []Candidate, ref string) (Target, bool) {
	if ref == "" {
		// Without a reference there is nothing to look for on disk.
		return Target{}, false
	}

	ordered := append([]Candidate(nil), candidates...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Namespace < ordered[j].Namespace })

	for _, cand := range ordered {
		if c.up.CachedManifest(cand.Namespace, cand.EnginePath, ref) {
			return Target{Namespace: cand.Namespace, EnginePath: cand.EnginePath}, true
		}
	}
	return Target{}, false
}

// probe asks one candidate's elected upstream whether it has what the lookup
// asks for: a manifest reference, or a blob digest.
//
// The probe is the real content path, not /v2/: an upstream whose API root
// answers can still fail to serve content, and that is exactly the gap this
// package exists to close. Which content path matters: a manifest and a blob
// are different questions with different paths, and asking the wrong one gets a
// 404 that is not about the content at all.
func (c *Chooser) probe(ctx context.Context, cand Candidate, upstreamURL string, q lookup) probeResult {
	res := probeResult{
		namespace:  cand.Namespace,
		enginePath: cand.EnginePath,
		upstream:   upstreamURL,
		startedAt:  time.Now(),
	}

	probeCtx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()

	// cand.EnginePath is repository-level and already carries this namespace's
	// rules — "library/alpine" for a single-segment repository in a namespace
	// with official_library enabled. q.repo is the client's name and only keys
	// the cache, the log and the token scope, so two candidates can probe
	// different paths for the same repo and both be right.
	target := strings.TrimRight(upstreamURL, "/") + "/v2/" + escapeRepo(cand.EnginePath) + q.op + escapeSegment(q.probeRef)
	res.path = target

	// The probe asks one question and may have to spend up to three requests
	// getting an answer to it:
	//
	//	HEAD                  — the question itself, no manifest transferred
	//	HEAD + token          — after a 401, whose challenge we answer
	//	GET  + token          — when the upstream refuses HEAD on manifests
	//
	// Redirects ARE followed, exactly as the registry engines follow them when
	// they serve a real pull — a probe that refused to follow would call a
	// namespace absent that the engine can serve. Measured: registry.k8s.io
	// answers a manifest path with 307 to
	// us-east4-docker.pkg.dev/v2/k8s-artifacts-prod/images/…, which then returns
	// the manifest. That is why the guard against the dhi.io defect is the
	// content type below, not the redirect: what a mirror's "302 → an HTML
	// landing page" has in common with a real registry is nothing except the
	// 200 at the end, and the media type is what tells them apart.
	//
	// What counts as an answer, and what does not, is the whole of this
	// function. Three classes, one policy, shared with the token endpoint's
	// classification above:
	//
	//	2xx                     — an answer: the content is here
	//	refusals (401/403/404,
	//	  400/410, 405 on both
	//	  methods, ...)         — an answer: rms cannot serve this repository
	//	5xx, 429, transport
	//	  failure, timeout      — NOT an answer: try again later
	//
	// A refusal is an answer even though it is not a "yes": for these upstreams
	// "this needs authentication and rms is an anonymous read-only cache" *is*
	// "rms cannot serve this repository", which is a definite no, not an
	// unknown. Measured: ghcr.io, gcr.io and quay.io all refuse rms's anonymous
	// requests — ghcr/gcr answer their token endpoint with 403, quay answers the
	// manifest with 401 even after issuing a ticket — and treating that as a
	// failure made every uncached unqualified request for a mistyped image
	// answer 503 instead of the 404 it is.
	//
	// A server error is the opposite mistake: 500 or 429 says nothing about
	// whether the repository exists, and calling it "absent" hands the client a
	// 404 it will not retry, for an image that may well be there.
	method := http.MethodHead
	token := ""
	authenticated := false

	for attempt := 0; attempt < 3; attempt++ {
		resp, err := c.probeOnce(probeCtx, method, target, token)
		if err != nil {
			return res.failed(err)
		}
		status := resp.status

		switch {
		case status >= 200 && status < 300:
			// A 2xx is not enough on its own: an error page or a mirror's HTML
			// landing page also arrives with 200. What is being probed has to
			// look like what it claims to be.
			if !looksLikeContent(q.kind, resp.contentType) {
				return res.absentWith(status, "not-"+q.kind+"-content:"+mediaTypeOf(resp.contentType))
			}
			res = res.found(status)
			res.digest, res.digestKnown = c.contentDigest(probeCtx, q, target, token, resp.digest)
			return res

		case status == http.StatusUnauthorized:
			if authenticated {
				// We presented a ticket and were challenged again. The
				// registry refuses us, which is its answer.
				return res.absent(status)
			}
			// Anonymous tickets are the normal path for public registries —
			// Docker Hub works exactly this way — so the endpoint is tried
			// even with no configured credentials.
			fresh, err := c.fetchToken(probeCtx, cand.Namespace, resp.challenge, q.repo)
			if err != nil {
				if errors.Is(err, errAuthRefused) {
					// The registry turned our credentials down, or never gave
					// us a way to present them. Also an answer.
					return res.absentWith(status, "auth-refused")
				}
				return res.failed(err)
			}
			token = fresh
			authenticated = true

		case (status == http.StatusMethodNotAllowed || status == http.StatusNotImplemented) && method == http.MethodHead:
			// This upstream does not answer manifests on HEAD. Ask the same
			// question with a GET before judging anything.
			method = http.MethodGet

		case status == http.StatusMethodNotAllowed:
			// Refused on both methods: this path serves no manifests here.
			return res.absent(status)

		case status >= 500 || status == http.StatusTooManyRequests:
			// The upstream could not serve the request — it failed, or it is
			// asking us to back off. Neither is a statement about the
			// repository.
			return res.failed(fmt.Errorf("%w: %s answered %d for the %s path",
				errUpstreamStatus, cand.Namespace, status, strings.Trim(q.op, "/")))

		case status >= 300 && status < 400:
			// Redirects are followed (see probeOnce), so a 3xx here is one the
			// client would not follow — a code it does not implement, or a
			// chain it abandoned. That leaves the question open rather than
			// answered.
			return res.failed(fmt.Errorf("%w: %s answered %d that could not be followed",
				errRedirectUnfollowed, cand.Namespace, status))

		default:
			// Every other status is a definitive answer: the repository or
			// reference is not served here.
			return res.absent(status)
		}
	}

	// Unreachable in practice: every path through the loop settles within the
	// three requests above. If one ever did not, the question would still be
	// open, so it is not an absence.
	return res.failed(fmt.Errorf("%s: probe did not settle within three requests", cand.Namespace))
}

// found is the result of a probe that found the repository and reference.
func (r probeResult) found(status int) probeResult {
	r.status = status
	r.exists = true
	r.reachable = true
	r.latency = time.Since(r.startedAt)
	return r
}

// contentDigest establishes which bytes a 2xx response was for.
//
// A blob and a digest reference name their content, so the answer is the
// reference itself. A tag does not: the upstream's Docker-Content-Digest header
// is the answer when it sends one, and when it does not — some mirrors omit it
// — rms fetches the manifest once and hashes it. That is one extra small
// request per decision, and it buys the digest gate: without a digest there is
// nothing to compare two mirrors of one namespace against, and they could not
// be balanced safely.
func (c *Chooser) contentDigest(ctx context.Context, q lookup, target, token, header string) (string, bool) {
	if q.kind == kindBlob {
		return q.probeRef, true
	}
	if dgst, err := godigest.Parse(q.probeRef); err == nil && dgst.Validate() == nil {
		return dgst.String(), true
	}
	if header != "" {
		if dgst, err := godigest.Parse(header); err == nil && dgst.Validate() == nil {
			return dgst.String(), true
		}
	}
	return c.hashManifest(ctx, target, token)
}

// hashManifest GETs a manifest and returns its digest. It reports false rather
// than an error: a digest that cannot be established leaves the namespace
// servable but not balanceable, which is the safe direction.
func (c *Chooser) hashManifest(ctx context.Context, target, token string) (string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("User-Agent", probeUserAgent)
	req.Header.Set("Accept", manifestAccept)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.opts.Client.Do(req)
	if err != nil {
		return "", false
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		drain(resp)
		return "", false
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBody+1))
	_ = resp.Body.Close()
	if err != nil || len(body) > maxManifestBody {
		return "", false
	}
	return godigest.FromBytes(body).String(), true
}

// absent is the result of a probe the upstream answered with a refusal: the
// answer is "not here", which Decide may turn into a 404.
func (r probeResult) absent(status int) probeResult {
	return r.absentWith(status, "")
}

// absentWith is absent, with a short reason for the decision log.
func (r probeResult) absentWith(status int, reason string) probeResult {
	r.status = status
	r.exists = false
	r.reachable = true
	r.reason = reason
	r.latency = time.Since(r.startedAt)
	return r
}

// failed marks a probe that produced no usable answer. The error is kept for
// the log; it never reaches the caller of Decide, which only learns that the
// answer is unknown.
func (r probeResult) failed(err error) probeResult {
	r.err = err
	r.reachable = false
	r.exists = false
	r.reason = reasonFor(err)
	r.latency = time.Since(r.startedAt)
	return r
}

// probeResponse is what one probe request produced.
type probeResponse struct {
	status      int
	challenge   string
	contentType string
	// digest is the Docker-Content-Digest the upstream advertised, empty when
	// it did not send one.
	digest string
}

// probeOnce performs one request, discards its body and reports the status, the
// WWW-Authenticate challenge and the media type, if any.
func (c *Chooser) probeOnce(ctx context.Context, method, target, token string) (probeResponse, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return probeResponse{}, err
	}
	req.Header.Set("User-Agent", probeUserAgent)
	// Both the HEAD and the GET it may degrade to must negotiate content the
	// same way a real client does; a degraded request is still the same
	// question.
	req.Header.Set("Accept", manifestAccept)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.opts.Client.Do(req)
	if err != nil {
		return probeResponse{}, err
	}
	drain(resp)

	return probeResponse{
		status:      resp.StatusCode,
		challenge:   resp.Header.Get("WWW-Authenticate"),
		contentType: resp.Header.Get("Content-Type"),
		digest:      resp.Header.Get("Docker-Content-Digest"),
	}, nil
}

// looksLikeContent reports whether a 2xx response's media type is consistent
// with what was probed.
//
// It exists because a 2xx is not proof on its own: the 103 deployment showed a
// mirror answering an unknown repository with a redirect to an HTML page, which
// arrived as a 200 text/html and was taken for a manifest. The rule is
// deliberately tolerant of a missing media type — a HEAD response is not
// required to carry one, and the status is then the only evidence there is —
// but it will not accept a page where JSON (a manifest) or opaque bytes (a
// blob) belongs.
func looksLikeContent(kind, contentType string) bool {
	mediaType := mediaTypeOf(contentType)
	if mediaType == "" {
		// No usable media type: no evidence either way, and the status is all
		// there is. Rejecting here would call a working namespace absent — the
		// error that costs the most.
		return true
	}
	if strings.Contains(mediaType, "html") {
		// The shape that caused the defect: an error or mirror landing page.
		return false
	}
	if kind != kindManifest {
		// Blob bytes: octet-stream, gzip, or whatever the CDN labels them.
		return true
	}
	// A manifest is JSON — including the docker and OCI media types, and any
	// media type added later, without this list having to keep up. Anything
	// else positively labelled is not something the engines can unmarshal as a
	// manifest, so calling it absent matches what a pull would have done.
	return strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "/json")
}

// mediaTypeOf returns the lower-case type/subtype without parameters. It returns
// "" both for a missing header and for one it cannot parse: neither is evidence
// of what the body is.
func mediaTypeOf(contentType string) string {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	return strings.ToLower(mediaType)
}

// reasonFor reduces a probe failure to a short, log-safe token: no URLs, no
// headers, no credentials. It is what makes "why is ghcr.io unreachable on this
// deployment" answerable from a production log instead of by guesswork.
func reasonFor(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, errTokenTransport):
		return "token-transport"
	case errors.Is(err, errTokenBody):
		return "token-answer"
	case errors.Is(err, errTokenStatus):
		return "token-status"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, errUpstreamStatus):
		return "upstream-status"
	case errors.Is(err, errRedirectUnfollowed):
		return "redirect"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	return "transport"
}

// errAuthRefused marks a credential demand the registry answered with a
// refusal, rather than something we failed to carry out: the token endpoint
// turned our credentials down (401/403), or the challenge never named a realm
// we could present them to.
//
// It is deliberately separate from a transport failure. A refusal is the
// registry's answer — "rms, with the credentials it has, may not read this
// repository" — which is a definite no for an anonymous read-only cache. An
// unreachable token endpoint is not an answer at all. Measured on ghcr.io and
// gcr.io, whose token endpoints answer anonymous requests with 403.
var errAuthRefused = errors.New("registry requires credentials it did not accept")

// Sentinels that tell the failure classes apart in the log. They carry no
// details of their own; the wrapped error does, and reasonFor reduces it to a
// short token.
var (
	errTokenTransport     = errors.New("token endpoint unreachable")
	errTokenStatus        = errors.New("token endpoint did not issue a ticket")
	errTokenBody          = errors.New("token endpoint answered with no usable ticket")
	errUpstreamStatus     = errors.New("upstream could not serve the request")
	errRedirectUnfollowed = errors.New("upstream redirected somewhere the probe would not follow")
)

// fetchToken performs the bearer token handshake a 401 challenge describes:
// GET the challenge's realm with the namespace's configured credentials, and
// return the token it issues.
//
// Errors wrapping errAuthRefused mean the registry refused; every other error
// is a transport-level failure or a broken token service. The credentials never
// reach a log or an error string: only the realm's host and the status appear.
func (c *Chooser) fetchToken(ctx context.Context, namespace, challenge, repo string) (string, error) {
	params := parseBearerChallenge(challenge)
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("%w: no usable Bearer challenge", errAuthRefused)
	}

	u, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("%w: token realm is not a URL", errAuthRefused)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%w: token realm %q is not http(s)", errAuthRefused, u.Scheme)
	}

	q := u.Query()
	if service := params["service"]; service != "" {
		q.Set("service", service)
	}
	scope := params["scope"]
	if scope == "" {
		// The challenge usually carries the scope; derive the pull scope when
		// it does not, because a token without one cannot pull anything.
		scope = "repository:" + repo + ":pull"
	}
	q.Set("scope", scope)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", probeUserAgent)
	req.Header.Set("Accept", "application/json")
	if user, pass := c.up.UpstreamCredentials(namespace); user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
	}

	// Redirects are followed here: a token service that bounces us elsewhere is
	// not one, and seeing where it lands is how the content-type check below
	// can tell.
	resp, err := c.opts.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: token endpoint %s: %w", errTokenTransport, u.Host, err)
	}
	defer drain(resp)

	if resp.StatusCode != http.StatusOK {
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			// The endpoint exists and turned us down: an answer, not an
			// outage.
			return "", fmt.Errorf("%w: token endpoint %s: status %d", errAuthRefused, u.Host, resp.StatusCode)
		default:
			// Including 5xx and 429: the endpoint did not render a verdict on
			// our credentials, so the question stays open.
			return "", fmt.Errorf("%w: token endpoint %s: status %d", errTokenStatus, u.Host, resp.StatusCode)
		}
	}

	if strings.Contains(mediaTypeOf(resp.Header.Get("Content-Type")), "html") {
		// A 200 that is a web page is not a token service. This is the same
		// shape as the manifest defect below: a mirror that redirects
		// everything to an HTML landing page can never authenticate us, so the
		// namespace cannot serve an anonymous cache — a refusal, not an outage.
		return "", fmt.Errorf("%w: token endpoint %s answered with a web page", errAuthRefused, u.Host)
	}

	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTokenBody)).Decode(&body); err != nil {
		return "", fmt.Errorf("%w: token endpoint %s: %w", errTokenBody, u.Host, err)
	}
	if body.Token != "" {
		return body.Token, nil
	}
	if body.AccessToken != "" {
		return body.AccessToken, nil
	}
	return "", fmt.Errorf("%w: token endpoint %s answered with no ticket", errTokenBody, u.Host)
}

// pick chooses the winner among the probe results, without its upstream pool.
// It is pickPool without the load-balancing half, for callers that only ask
// "which namespace".
func (c *Chooser) pick(results []probeResult) (Target, error) {
	target, _, err := c.pickPool(results)
	return target, err
}

// pickPool chooses the namespace, the upstream pool that serves it, and the
// upstream this request goes to. The two differ because the pool is what makes
// "same content, share the load" possible: the pool is computed once per
// decision and rotated per request by the caller (see cachedDecision.pick).
//
// Only a 2xx counts as "has it". Within one namespace the results pass the
// digest gate first — the first upstream in configuration order that answered
// defines the bytes, and a mirror that disagrees is not served and not balanced
// — then the fastest eligible probe wins the namespace competition, an exact
// tie falls back to the upstream's own recent probe latency when the Upstream
// reports one, and then to namespace name, so the outcome never depends on
// goroutine scheduling.
//
// When nobody has it, the two failures are told apart by who answered at all:
//
//   - at least one candidate said "not here" → ErrNotFound. The answer is "the
//     candidates we could ask all say no", which is what a 404 has always meant
//     here;
//   - nobody answered — every candidate failed at the transport level, timed
//     out, was abandoned at the decision deadline, or had no upstream to ask →
//     ErrUnreachable. That is the honest "I do not know".
//
// The asymmetry is deliberate. Answering 503 whenever *any* candidate stayed
// silent makes a client retry for ever over one flaky namespace, and it does so
// for every uncached request: the user sees a broken service for an image that
// simply does not exist. Answering 404 costs only the case where the silent
// candidate was the one holding the content — and there the content was not
// reachable anyway. Silence is not evidence that something is there.
func (c *Chooser) pickPool(results []probeResult) (Target, []poolEntry, error) {
	answered, exists := 0, 0
	for _, r := range results {
		if r.reachable {
			answered++
		}
		if r.exists {
			exists++
		}
	}
	if exists == 0 {
		if answered == 0 {
			return Target{}, nil, ErrUnreachable
		}
		return Target{}, nil, ErrNotFound
	}

	// Group by namespace. The race produced the results namespace by namespace,
	// in name order, with each namespace's upstreams in configuration order — so
	// consecutive runs are groups, and the order inside one is the digest
	// authority order.
	type group struct {
		namespace  string
		enginePath string
		results    []probeResult
	}
	var groups []group
	for _, r := range results {
		if n := len(groups); n > 0 && groups[n-1].namespace == r.namespace {
			groups[n-1].results = append(groups[n-1].results, r)
			continue
		}
		groups = append(groups, group{namespace: r.namespace, enginePath: r.enginePath, results: []probeResult{r}})
	}

	type candidate struct {
		namespace  string
		enginePath string
		latency    time.Duration
		pool       []poolEntry
	}
	candidates := make([]candidate, 0, len(groups))
	for _, g := range groups {
		eligible, mismatched := digestEligible(g.results)
		for _, r := range mismatched {
			if c.opts.Observer != nil {
				c.opts.Observer.UpstreamDigestMismatch(r.namespace, r.upstream)
			}
		}
		if len(eligible) == 0 {
			continue
		}
		fastest := eligible[0]
		for _, r := range eligible {
			if r.latency < fastest.latency {
				fastest = r
			}
		}
		candidates = append(candidates, candidate{
			namespace:  g.namespace,
			enginePath: g.enginePath,
			latency:    fastest.latency,
			pool: buildPool(eligible, fastest.latency,
				c.balanceMode(g.namespace), c.opts.BalanceWindow),
		})
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.latency != b.latency {
			return a.latency < b.latency
		}
		la, oka := c.upstreamLatency(a.namespace)
		lb, okb := c.upstreamLatency(b.namespace)
		if oka && okb && la != lb {
			return la < lb
		}
		return a.namespace < b.namespace
	})

	w := candidates[0]
	upstream, _ := pickPool(w.pool, nil)
	return Target{
		Namespace:    w.namespace,
		EnginePath:   w.enginePath,
		Upstream:     upstream,
		ProbeLatency: w.latency,
	}, w.pool, nil
}

// digestEligible applies the digest gate to one namespace's probe results: the
// content served is the content the namespace's authority upstream served.
//
// The authority is the first upstream in configuration order that answered with
// content. Mirrors of one upstream registry are supposed to hold the same bytes
// for a tag; when they do not, one of them is stale or has been replaced, and
// the configured order is the only tie-break an operator controls. Alternating
// between them would alternate between two different images of the same name,
// and would make the shared tag link in rms's cache flap between them.
//
// A result whose digest could not be established is never served alongside a
// known-digest one: "same hash" is the licence to balance, and an unknown hash
// is not evidence of sameness. It is still served when it is all there is —
// refusing would break mirrors that do not send Docker-Content-Digest at all —
// but then the namespace is served from its fastest upstream and balanced not
// at all.
//
// It returns the eligible results in configuration order, plus the results that
// were excluded because their digest disagrees with the authority — which is
// what the digest-mismatch metric counts, since a non-zero rate means a mirror
// of that registry is stale. Either slice is nil when there is nothing to
// report.
func digestEligible(results []probeResult) (eligible, mismatched []probeResult) {
	var authority *probeResult
	for i := range results {
		if results[i].exists {
			authority = &results[i]
			break
		}
	}
	if authority == nil {
		return nil, nil
	}
	if !authority.digestKnown {
		return []probeResult{fastestExisting(results)}, nil
	}

	out := make([]probeResult, 0, len(results))
	for _, r := range results {
		switch {
		case !r.exists:
			// A refusal is not a disagreement about content.
		case !r.digestKnown:
			// "Same hash" is the licence to balance, and an unknown hash is not
			// evidence of sameness: the mirror is excluded from the pool but is
			// not a mismatch either.
		case r.digest == authority.digest:
			out = append(out, r)
		default:
			mismatched = append(mismatched, r)
		}
	}
	if len(out) == 0 {
		// The authority is itself eligible by construction; this is defence in
		// depth against a future filter that is not.
		return []probeResult{*authority}, mismatched
	}
	return out, mismatched
}

// fastestExisting returns the fastest result that answered with content.
func fastestExisting(results []probeResult) probeResult {
	var best probeResult
	found := false
	for _, r := range results {
		if !r.exists {
			continue
		}
		if !found || r.latency < best.latency {
			best, found = r, true
		}
	}
	return best
}

// latencyReporter is the optional interface an Upstream implements when it
// tracks how long its own health probes take. It is a tie-break weight only:
// the Chooser works without it, and the measured probe latency always wins.
type latencyReporter interface {
	UpstreamLatency(namespace string) (time.Duration, bool)
}

func (c *Chooser) upstreamLatency(namespace string) (time.Duration, bool) {
	lr, ok := c.up.(latencyReporter)
	if !ok {
		return 0, false
	}
	d, ok := lr.UpstreamLatency(namespace)
	if !ok || d <= 0 {
		return 0, false
	}
	return d, true
}

// cached returns a live decision for this lookup, with the upstream chosen from
// its pool for this request and ProbeLatency cleared because nothing was probed
// this time.
//
// Choosing here, rather than when the decision was made, is what makes the load
// balancing real: the decision lives for probe_ttl, and every request it serves
// rotates through the pool instead of pinning the upstream that happened to be
// fastest when the race ran.
func (c *Chooser) cached(q lookup) (Target, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := decisionKey(q.cacheRepo(), q.cacheRef)
	d, ok := c.decisions[key]
	if !ok {
		return Target{}, false
	}
	if !time.Now().Before(d.expires) {
		delete(c.decisions, key)
		return Target{}, false
	}
	target := d.pick()
	c.decisions[key] = d
	return target, true
}

// store records the decision and the repository-level affinity entry that blob
// requests follow. Both expire with the TTL: a blob request carries no
// repository name, so it can only follow the repository's most recent manifest
// decision, and that association must not outlive the decision it came from.
//
// pool is empty for a decision that did not probe (a disk hit): it carries no
// upstream, and the caller falls back to the namespace's sticky engine.
func (c *Chooser) store(q lookup, target Target, pool []poolEntry) {
	expires := time.Now().Add(c.opts.TTL)
	entry := cachedDecision{target: target, expires: expires, pool: pool}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.decisions[decisionKey(q.cacheRepo(), q.cacheRef)] = entry

	// A namespace-qualified decision must not become the repository's global
	// affinity: that entry answers "which namespace serves this repository" for
	// a blob request that named none, and this decision was never allowed to
	// ask. It gets its own entry instead, so a namespace-qualified blob request
	// follows the manifest decision of the same namespace and repository.
	if q.namespace != "" {
		c.affinity[qualifiedRepo(q.namespace, q.repo)] = entry
		return
	}

	c.affinity[q.repo] = entry
	// The same decision is also the answer to "which namespace was the last
	// manifest served from", which is all a blob request that names no
	// repository has to go on. A blob decision must not overwrite it: the
	// pointer means "the manifest", and a digest-only request follows it
	// precisely because it has nothing else to go on.
	if q.op == opManifests {
		c.recent = entry
	}
}

// Invalidate drops the cached decision for (repo, ref) and the repository-level
// affinity entry that decision wrote, so the next Decide probes again.
//
// The dispatcher calls this when the chosen namespace fails to serve the
// content it was chosen for: the decision was made on a probe, and a failed
// pull is the evidence that the probe no longer holds.
func (c *Chooser) Invalidate(repo, ref string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.decisions, decisionKey(repo, ref))
	c.forgetAffinityLocked(repo)
}

// InvalidateQualified drops the cached decision of a namespace-qualified
// request, and the affinity entry it wrote, so the next one probes again.
func (c *Chooser) InvalidateQualified(namespace, repo, ref string) {
	if namespace == "" {
		c.Invalidate(repo, ref)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := qualifiedRepo(namespace, repo)
	delete(c.decisions, decisionKey(key, ref))
	delete(c.affinity, key)
}

// forgetAffinityLocked drops a repository's affinity entry, and also the
// most-recent-manifest pointer when it points at that same decision — leaving
// it behind would keep sending blob requests to the namespace that was just
// found not to serve them. Callers must hold the lock.
func (c *Chooser) forgetAffinityLocked(repo string) {
	entry, ok := c.affinity[repo]
	delete(c.affinity, repo)
	if ok && c.recent.target.sameDecision(entry.target) {
		c.recent = cachedDecision{}
	}
}

// InvalidateRepo drops only the repository-level affinity entry. A failed blob
// request knows its repository but not the manifest reference the affinity was
// decided from, so this is the entry it can drop.
func (c *Chooser) InvalidateRepo(repo string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.forgetAffinityLocked(repo)
}

// Affinity returns the namespace currently responsible for a repository's
// content, which is how a blob request — whose path carries only a digest —
// follows the manifest decision for the same repository. The upstream is picked
// from the decision's pool like any other request served by it.
func (c *Chooser) Affinity(repo string) (Target, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	d, ok := c.affinity[repo]
	if !ok || !time.Now().Before(d.expires) {
		if ok {
			delete(c.affinity, repo)
		}
		return Target{}, false
	}
	target := d.pick()
	c.affinity[repo] = d
	return target, true
}

// QualifiedUpstream returns the upstream a namespace-qualified repository is
// currently served from, which is how a qualified blob request — whose path
// carries the repository and a digest — follows the manifest decision made for
// that same namespace and repository instead of probing the blob path again.
func (c *Chooser) QualifiedUpstream(namespace, repo string) (Target, bool) {
	if namespace == "" || repo == "" {
		return Target{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	key := qualifiedRepo(namespace, repo)
	d, ok := c.affinity[key]
	if !ok || !time.Now().Before(d.expires) {
		if ok {
			delete(c.affinity, key)
		}
		return Target{}, false
	}
	target := d.pick()
	c.affinity[key] = d
	c.observeServed(target)
	return target, true
}

// DecideBlob resolves an unqualified request that carries only a digest: a
// blob HEAD/GET, and the upload-session paths that follow it. Those requests
// name no repository, so there is nothing to probe by name — the caller cannot
// even tell which namespace owns the digest.
//
// It answers only from the repository affinity written by the last manifest
// decision: a client asks for a manifest and then for the blobs it names, in
// that order, so the blob belongs with that manifest — and, crucially, the
// affinity carries the repository the blob has to be addressed by.
//
// Without an affinity the answer is ErrUnreachable (503), never ErrNotFound,
// for two reasons:
//
//   - a blob is addressed inside a namespace as /v2/<repo>/blobs/<digest>, and
//     on-disk blob data is content-addressed with no repository recorded
//     anywhere. Naming a repository would mean guessing one, and fetching the
//     data through another repository's path.
//   - "I do not know which repository this digest belongs to" is not evidence
//     that the content is missing, so it must never become a cacheable 404.
//
// The local blob store is still consulted first, purely so the log can tell the
// two ways of not knowing apart: a digest that sits on some namespace's disk
// and could not be addressed is a different operational situation from one no
// namespace has at all.
func (c *Chooser) DecideBlob(digest string) (Target, error) {
	if c.up == nil {
		return Target{}, ErrUnreachable
	}

	if target, ok := c.recentManifest(); ok {
		c.observeDecision(sourceAffinity)
		c.observeServed(target)
		c.logBlob(digest, target.Namespace, target.EnginePath, sourceAffinity)
		return target, nil
	}

	if dgst, err := godigest.Parse(digest); err == nil && dgst.Validate() == nil {
		for _, namespace := range c.servableNamespaces() {
			if c.up.CachedBlob(namespace, dgst) {
				// The namespace is known; the repository it would have to be
				// fetched through is not. Report which namespace holds it so
				// the log says why this was unanswerable, and still refuse.
				c.logBlob(digest, namespace, "", sourceBlobDisk)
				return Target{}, ErrUnreachable
			}
		}
	}

	c.logBlob(digest, "", "", sourceUnknown)
	return Target{}, ErrUnreachable
}

// recentManifest returns the target of the most recent manifest decision, if it
// has not expired. See DecideBlob for why this, and not a per-repository
// lookup, is the right key.
func (c *Chooser) recentManifest() (Target, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.recent.target.Namespace == "" || !time.Now().Before(c.recent.expires) {
		return Target{}, false
	}
	target := c.recent.pick()
	return target, true
}

// servableNamespaces lists the namespaces a digest could be sitting in, sorted
// so that "several of them have it" resolves the same way every time.
func (c *Chooser) servableNamespaces() []string {
	out := append([]string(nil), c.up.Serving()...)
	sort.Strings(out)
	return out
}

// Sources of a decision, reported as the "source" attribute of the decision
// line so an operator can see where a winner came from.
const (
	// sourceDecisionCache: the (repo, ref) decision was still within its TTL.
	sourceDecisionCache = "decision-cache"
	// sourceCache: a candidate already held the reference on local disk.
	sourceCache = "cache"
	// sourceProbe: the candidates were probed.
	sourceProbe = "probe"
	// sourceAffinity: a blob followed the last manifest decision.
	sourceAffinity = "affinity"
	// sourceBlobDisk: a blob was found on a namespace's local disk but its
	// repository is unknown, so it could not be addressed and was refused.
	sourceBlobDisk = "blob-disk"
	// sourceUnknown: nothing could resolve the request.
	sourceUnknown = "none"
)

// logBlob writes the decision line for a request that carried only a digest.
// repo is the repository the winner was addressed through, and is empty exactly
// when the blob could not be addressed at all — which is the case an operator
// needs to see.
func (c *Chooser) logBlob(digest, winner, repo, source string) {
	c.logger.Info("selection",
		"digest", digest,
		"winner", winner,
		"repo", repo,
		"source", source,
	)
}

// logDecision writes the one line per real decision the operator reads.
//
// winner is the target the decision produced: its Namespace is what the
// namespace competition chose, and its Upstream the endpoint this particular
// request was handed. candidates counts the *probes* that were launched, which
// is the fan-out width — one namespace with three upstreams and two others with
// two each is seven, and that is the number an operator comparing this to their
// upstream access logs wants.
func (c *Chooser) logDecision(q lookup, winner Target, candidates int, timings []string, source string) {
	attrs := []any{
		"repo", q.repo,
		"ref", q.probeRef,
		"kind", q.kind,
		"winner", winner.Namespace,
		"upstream", winner.Upstream,
		"candidates", candidates,
		"source", source,
		"cached", source == sourceDecisionCache,
	}
	if q.namespace != "" {
		attrs = append(attrs, "scoped_namespace", q.namespace)
	}
	if timings != nil {
		attrs = append(attrs, "timings", timings)
	}
	c.logger.Info("selection", attrs...)
}

// logProbe records one probe's arrival time, duration and outcome, so a report
// can show what each candidate actually cost.
func (c *Chooser) logProbe(q lookup, r probeResult) {
	attrs := []any{
		"repo", q.repo,
		"ref", q.probeRef,
		"kind", q.kind,
		"namespace", r.namespace,
		"upstream", r.upstream,
		"path", r.path,
		"started_at", r.startedAt.UTC().Format(time.RFC3339Nano),
		"latency", r.latency,
	}
	switch {
	case r.skipped:
		attrs = append(attrs, "skipped", r.skipReason)
	case r.err != nil:
		attrs = append(attrs, "status", r.status, "reachable", r.reachable, "exists", false, "error", r.err)
	default:
		attrs = append(attrs, "status", r.status, "reachable", r.reachable, "exists", r.exists)
	}
	if r.digestKnown {
		attrs = append(attrs, "digest", r.digest)
	}
	c.logger.Debug("selection probe", attrs...)
}

// reasonSuffix renders a probe's short reason for the decision log, or nothing.
func reasonSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return ":" + reason
}

// timings renders the per-candidate probe durations for the decision line, with
// the outcome and — where there is one — the short reason for it. The reason is
// what lets an operator answer "why is this candidate unreachable on this
// deployment" from the production log, which is how the difference between a
// refused credential, a timeout and a redirect gets settled without guesswork.
//
// A namespace with more than one upstream contributes one entry per upstream,
// labelled host by host: the whole point of the race is that those entries can
// disagree, and "docker.io=40ms" twice would not say which mirror that was.
func timings(results []probeResult) []string {
	perNamespace := map[string]int{}
	for _, r := range results {
		perNamespace[r.namespace]++
	}

	out := make([]string, 0, len(results))
	for _, r := range results {
		name := r.namespace
		if perNamespace[r.namespace] > 1 {
			name += "/" + upstreamShort(r.upstream)
		}
		label := name + "=" + r.latency.Round(time.Microsecond).String()
		switch {
		case r.skipped:
			label += "(skipped" + reasonSuffix(r.skipReason) + ")"
		case r.exists:
		case r.reachable:
			// Answered, but without the content: a refusal, a redirected
			// manifest path, or a 2xx that was not content at all.
			label += "(absent" + reasonSuffix(r.reason) + ")"
		default:
			label += "(unreachable" + reasonSuffix(r.reason) + ")"
		}
		out = append(out, label)
	}
	return out
}

// upstreamShort renders an upstream URL as host:port for the decision line. The
// scheme and any path are noise there; what an operator needs is which mirror
// the candidate was.
func upstreamShort(raw string) string {
	if raw == "" {
		return "none"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host
}

// drain reads a bounded amount of the body and closes it, so the connection
// can be reused without any response body being buffered.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDiscardBody))
	_ = resp.Body.Close()
}

// parseBearerChallenge pulls the parameters out of a WWW-Authenticate header.
// It returns nil when the header is absent or does not describe a Bearer
// challenge.
func parseBearerChallenge(header string) map[string]string {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return nil
	}

	out := map[string]string{}
	for _, part := range splitChallengeParams(rest) {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.Trim(strings.TrimSpace(value), `"`)
		if key != "" && value != "" {
			out[key] = value
		}
	}
	return out
}

// splitChallengeParams splits on commas that are not inside a quoted value, so
// a scope or realm containing a comma survives.
func splitChallengeParams(s string) []string {
	var (
		out     []string
		current strings.Builder
		quoted  bool
	)
	for _, r := range s {
		switch {
		case r == '"':
			quoted = !quoted
			current.WriteRune(r)
		case r == ',' && !quoted:
			out = append(out, current.String())
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		out = append(out, current.String())
	}
	return out
}

// escapeRepo escapes each segment of a repository name, keeping the slashes
// that separate them.
func escapeRepo(repo string) string {
	segments := strings.Split(repo, "/")
	for i, s := range segments {
		segments[i] = escapeSegment(s)
	}
	return strings.Join(segments, "/")
}

// escapeSegment escapes one path segment. It is narrower than url.PathEscape,
// which encodes the ":" of a digest reference ("sha256:...") that registries
// expect to receive literally.
func escapeSegment(s string) string {
	const hex = "0123456789ABCDEF"

	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if isSegmentByte(ch) {
			b.WriteByte(ch)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[ch>>4])
		b.WriteByte(hex[ch&0x0f])
	}
	return b.String()
}

func isSegmentByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '-', c == '.', c == '_', c == '~', c == ':', c == '@', c == '+':
		return true
	}
	return false
}
