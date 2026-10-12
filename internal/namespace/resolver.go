// Package namespace maps inbound registry v2 request paths onto the registry
// engine instance that should serve them.
//
// Two request shapes exist:
//
//   - multi-namespace (the primary listener): /v2/<namespace>/<repo>...
//     The namespace is either an upstream domain (docker.io, ghcr.io), a
//     writable local namespace, or absent — in which case the configured
//     default namespace is used, implementing
//     "rms/<user>/<image> -> rms/<default>/<user>/<image>".
//
//   - namespace-bound (an extra listener): /v2/<repo>... where the namespace is
//     implied by the socket. This exists because neither docker's
//     registry-mirrors nor containerd's registries.yaml can express a path
//     between /v2/ and the repository name.
//
// Proxy namespaces are read-only pull-through caches, so the engine must see
// the repository name *without* the namespace prefix: the namespace's
// RemoteURL supplies it by construction. Local namespaces are writable and must
// keep the prefix inside the repository name, because the engine builds upload
// Location URLs from the repository name and dropping the prefix would hand the
// client an unusable URL (see test/distbehavior).
package namespace

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/watsons/rms/internal/config"
)

// ErrNotFound is returned for paths that are not part of the registry v2 API.
var ErrNotFound = errors.New("not a registry v2 path")

// Kind classifies a resolved request.
type Kind int

const (
	// KindBase is GET /v2/: the API version probe docker login uses.
	KindBase Kind = iota
	// KindCatalog is /v2/_catalog: served by rms itself, not by a namespace.
	KindCatalog
	// KindRepository is every other /v2/... request.
	KindRepository
)

func (k Kind) String() string {
	switch k {
	case KindBase:
		return "base"
	case KindCatalog:
		return "catalog"
	case KindRepository:
		return "repository"
	default:
		return "unknown"
	}
}

// Resolution is the outcome of routing a request.
type Resolution struct {
	// Namespace serving the request. Empty for KindBase and KindCatalog.
	Namespace string
	// Mode of the resolved namespace.
	Mode config.Mode
	// Kind classifies the request.
	Kind Kind
	// EnginePath is the path handed to the namespace's registry engine.
	EnginePath string
	// Repo is the repository name as the engine will see it ("" when unknown).
	Repo string
}

// Table is the resolved namespace table. It is immutable once built.
type Table struct {
	defaultNS string
	local     map[string]struct{}
	proxy     map[string]config.ProxyNamespace
}

// NewTable builds a routing table. localNames are additional writable
// namespaces beyond the configuration file — typically the htpasswd usernames
// when auth.user_namespace is enabled.
func NewTable(cfg *config.Config, localNames []string) *Table {
	t := &Table{
		defaultNS: cfg.Namespaces.Default,
		local:     make(map[string]struct{}, len(cfg.Namespaces.Local)+len(cfg.Auth.SharedNamespaces)+len(localNames)),
		proxy:     cfg.Namespaces.Proxy,
	}
	// Local and proxy names cannot overlap: configuration validation requires
	// proxy keys to be domain-like and local names not to be.
	for _, n := range cfg.Namespaces.Local {
		t.local[n] = struct{}{}
	}
	for _, n := range cfg.Auth.SharedNamespaces {
		t.local[n] = struct{}{}
	}
	for _, n := range localNames {
		if n == "" {
			continue
		}
		t.local[n] = struct{}{}
	}
	return t
}

// Default returns the namespace unqualified requests fall back to.
func (t *Table) Default() string { return t.defaultNS }

// IsLocal reports whether name is a writable namespace.
func (t *Table) IsLocal(name string) bool {
	_, ok := t.local[name]
	return ok
}

// IsProxy reports whether name is a pull-through cache namespace.
func (t *Table) IsProxy(name string) bool {
	_, ok := t.proxy[name]
	return ok
}

// Mode returns the namespace's mode and whether the namespace is known.
func (t *Table) Mode(name string) (config.Mode, bool) {
	if t.IsLocal(name) {
		return config.ModeLocal, true
	}
	if t.IsProxy(name) {
		return config.ModeProxy, true
	}
	return "", false
}

// ProxyConfig returns the settings for a proxy namespace.
func (t *Table) ProxyConfig(name string) (config.ProxyNamespace, bool) {
	ns, ok := t.proxy[name]
	return ns, ok
}

// LocalNames returns the writable namespaces, sorted.
func (t *Table) LocalNames() []string {
	out := make([]string, 0, len(t.local))
	for n := range t.local {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Resolve routes a request path.
//
// listenerNS is the namespace a socket is bound to, or "" for the
// multi-namespace primary listener.
func (t *Table) Resolve(listenerNS, urlPath string) (Resolution, error) {
	rest, ok := strings.CutPrefix(urlPath, "/v2/")
	if !ok {
		if urlPath == "/v2" {
			return Resolution{Kind: KindBase}, nil
		}
		return Resolution{}, fmt.Errorf("%w: %q", ErrNotFound, urlPath)
	}

	if rest == "" {
		return Resolution{Kind: KindBase}, nil
	}

	first, tail, hasTail := strings.Cut(rest, "/")

	// The catalog is not a namespace, even though it is shaped like one.
	if first == "_catalog" && !hasTail {
		return Resolution{Kind: KindCatalog}, nil
	}

	ns, repoPath, err := t.route(listenerNS, first, tail, hasTail)
	if err != nil {
		return Resolution{}, err
	}

	repo, op := splitRepoOp(repoPath)

	// Docker Hub treats a single-segment repository as library/<name>. The
	// test is applied to the repository alone, never to the operation suffix.
	if mode, _ := t.Mode(ns); mode == config.ModeProxy && repo != "" && !strings.Contains(repo, "/") {
		repo = t.withOfficialLibrary(ns, repo)
	}

	return Resolution{
		Namespace:  ns,
		Mode:       t.modeOf(ns),
		Kind:       KindRepository,
		EnginePath: "/v2/" + repo + op,
		Repo:       repo,
	}, nil
}

func (t *Table) modeOf(ns string) config.Mode {
	m, _ := t.Mode(ns)
	return m
}

// route decides which namespace serves the request and returns the repository
// path (repository name plus operation suffix) as the engine must see it.
func (t *Table) route(listenerNS, first, tail string, hasTail bool) (ns, repoPath string, err error) {
	if listenerNS != "" {
		if _, known := t.Mode(listenerNS); !known {
			return "", "", fmt.Errorf("listener namespace %q is not configured", listenerNS)
		}
		// The socket supplies the namespace, so the whole remainder is the
		// repository. A redundant leading namespace is tolerated so the same
		// URL works on both listener shapes.
		if first == listenerNS {
			return listenerNS, tail, nil
		}
		return listenerNS, join(first, tail, hasTail), nil
	}

	switch {
	case t.IsProxy(first):
		// Explicit namespace prefix; the engine must not see it.
		return first, tail, nil

	case t.IsLocal(first):
		// Writable namespace: keep the prefix in the repository name so that
		// generated upload URLs carry it.
		return first, join(first, tail, hasTail), nil

	default:
		// Unqualified: apply the default namespace, implementing
		// rms/<user>/<image> -> rms/<default>/<user>/<image>.
		return t.defaultNS, join(first, tail, hasTail), nil
	}
}

// withOfficialLibrary mirrors Docker Hub's rule that a single-segment
// repository means library/<name>.
func (t *Table) withOfficialLibrary(ns, repo string) string {
	if repo == "" {
		return repo
	}
	pc, _ := t.proxy[ns]
	if !pc.OfficialLibrary {
		return repo
	}
	return "library/" + repo
}

func join(first, tail string, hasTail bool) string {
	if !hasTail {
		return first
	}
	return first + "/" + tail
}

// SplitOperation splits an engine path into the repository name and the
// operation suffix (including its leading slash). It is used by callers that
// need to reason about the operation outside the resolver, such as cache
// classification.
func SplitOperation(enginePath string) (repo, op string) {
	rest, ok := strings.CutPrefix(enginePath, "/v2/")
	if !ok {
		return "", ""
	}
	return splitRepoOp(rest)
}

// apiSentinels are the path segments that separate the repository name from the
// operation in a v2 path.
var apiSentinels = map[string]bool{"manifests": true, "blobs": true, "tags": true}

// splitRepoOp splits a repository path into the repository name and the
// operation suffix (including its leading slash). The operation starts at the
// first manifests/blobs/tags segment, which is unambiguous because repository
// names may not consist of those bare segments in a v2 path.
func splitRepoOp(repoPath string) (repo, op string) {
	if repoPath == "" {
		return "", ""
	}
	segments := strings.Split(repoPath, "/")
	for i, s := range segments {
		if apiSentinels[s] {
			return strings.Join(segments[:i], "/"), "/" + strings.Join(segments[i:], "/")
		}
	}
	return repoPath, ""
}
