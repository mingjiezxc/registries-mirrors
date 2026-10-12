package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/opencontainers/go-digest"

	"github.com/watsons/rms/internal/config"
	"github.com/watsons/rms/internal/namespace"
	"github.com/watsons/rms/internal/storage"
)

// catalogResponse is the registry v2 catalog envelope.
type catalogResponse struct {
	Repositories []string `json:"repositories"`
}

// serveCatalog answers /v2/_catalog with a merged view of the writable
// namespaces.
//
// A repository in a local namespace is stored under its namespaced name, so the
// primary listener can list it as-is. A namespace-bound listener strips the
// namespace it implies, because clients on that socket must not repeat it.
func (d *dispatcher) serveCatalog(w http.ResponseWriter, r *http.Request) {
	namespaces := d.catalogNamespaces()
	repos := make([]string, 0, 16)
	seen := map[string]bool{}

	for _, ns := range namespaces {
		mode, _ := d.table.Mode(ns)
		stored, err := storage.Repositories(d.storageFor(ns))
		if err != nil {
			d.log.Error("catalog listing failed", "namespace", ns, "error", err)
			writeError(w, http.StatusInternalServerError, "UNKNOWN", "failed to list the catalog")
			return
		}

		for _, repo := range stored {
			switch {
			case d.listenerNS != "":
				if mode == config.ModeLocal {
					repo = strings.TrimPrefix(repo, ns+"/")
				}
			case mode == config.ModeProxy:
				// On the primary listener the namespace has to be written out,
				// because it is part of the repository name the client uses.
				repo = ns + "/" + repo
			}

			if repo == "" || seen[repo] {
				continue
			}
			seen[repo] = true
			repos = append(repos, repo)
		}
	}

	sort.Strings(repos)

	repos = applyCatalogPaging(r, repos)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Docker-Distribution-Api-Version", registryAPIVersion)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(catalogResponse{Repositories: repos})
}

// catalogNamespaces returns the namespaces a listener's catalog covers.
func (d *dispatcher) catalogNamespaces() []string {
	if d.listenerNS != "" {
		return []string{d.listenerNS}
	}
	return d.table.LocalNames()
}

// applyCatalogPaging honours the n and last query parameters.
func applyCatalogPaging(r *http.Request, repos []string) []string {
	q := r.URL.Query()

	if last := q.Get("last"); last != "" {
		i := sort.SearchStrings(repos, last)
		if i < len(repos) && repos[i] == last {
			i++
		}
		repos = repos[i:]
	}

	if raw := q.Get("n"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n < len(repos) {
			repos = repos[:n]
		}
	}

	if repos == nil {
		repos = []string{}
	}
	return repos
}

// classifyCache reports whether a proxy-namespace read can be answered from
// local storage, and whether the request is one this can be determined for.
//
// It inspects distribution's layout rather than asking the engine, which does
// not expose cache state. Only manifests and blobs can be classified; upload
// sessions and other paths report known=false so they do not pollute the
// hit/miss metrics.
func classifyCache(namespaceRoot string, res namespace.Resolution) (cached, known bool) {
	repo, op := namespace.SplitOperation(res.EnginePath)
	if repo == "" || op == "" {
		return false, false
	}
	op = strings.TrimPrefix(op, "/")

	switch {
	case strings.HasPrefix(op, "manifests/"):
		ref := strings.TrimPrefix(op, "manifests/")
		if ref == "" {
			return false, false
		}
		// A reference is either a digest (immutable revision) or a tag.
		if dgst, err := digest.Parse(ref); err == nil && dgst.Validate() == nil {
			return storage.HasRevision(namespaceRoot, repo, dgst), true
		}
		return storage.HasTag(namespaceRoot, repo, ref), true

	case strings.HasPrefix(op, "blobs/"):
		ref := strings.TrimPrefix(op, "blobs/")
		if strings.HasPrefix(ref, "uploads") {
			return false, false
		}
		dgst, err := digest.Parse(ref)
		if err != nil {
			return false, false
		}
		// Blob data is stored once per namespace and linked from each
		// repository, so its presence is exactly the condition under which the
		// engine can answer without contacting the upstream.
		return storage.HasBlob(namespaceRoot, dgst), true

	default:
		return false, false
	}
}
