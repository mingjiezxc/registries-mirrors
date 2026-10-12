// Package storage knows where the embedded registry keeps things on disk.
//
// rms does not implement storage; distribution does. This package only encodes
// the small, stable part of that layout rms needs in order to answer questions
// the engine does not expose: which repositories a namespace holds (for the
// merged catalog) and whether an object is already cached (for cache hit and
// miss metrics).
//
// The layout is distribution's filesystem driver:
//
//	<root>/docker/registry/v2/blobs/<algo>/<aa>/<hex>/data
//	<root>/docker/registry/v2/repositories/<repo>/_manifests/tags/<tag>/current/link
//	<root>/docker/registry/v2/repositories/<repo>/_manifests/revisions/<algo>/<hex>/link
package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/opencontainers/go-digest"
)

// LayoutPrefix is the path distribution's filesystem driver appends to its
// configured rootdirectory.
const LayoutPrefix = "docker/registry/v2"

// V2Root returns the distribution v2 layout root for a namespace's root
// directory.
func V2Root(namespaceRoot string) string {
	return filepath.Join(namespaceRoot, filepath.FromSlash(LayoutPrefix))
}

// BlobDataPath returns the path of a blob's data file.
//
// The digest is validated first: digest.Digest.Algorithm panics on an
// unrecognised algorithm, so an unparsable value must be rejected as an error
// rather than reaching those accessors.
func BlobDataPath(namespaceRoot string, dgst digest.Digest) (string, error) {
	if err := dgst.Validate(); err != nil {
		return "", fmt.Errorf("invalid digest %q: %w", dgst, err)
	}
	algo := dgst.Algorithm().String()
	hex := dgst.Encoded()
	return filepath.Join(V2Root(namespaceRoot), "blobs", algo, hex[:2], hex, "data"), nil
}

// HasBlob reports whether a blob's data file is present.
func HasBlob(namespaceRoot string, dgst digest.Digest) bool {
	p, err := BlobDataPath(namespaceRoot, dgst)
	if err != nil {
		return false
	}
	return isRegularFile(p)
}

// TagLinkPath returns the path of the file recording which revision a tag
// points at.
func TagLinkPath(namespaceRoot, repo, tag string) string {
	return filepath.Join(V2Root(namespaceRoot), "repositories", filepath.FromSlash(repo),
		"_manifests", "tags", tag, "current", "link")
}

// HasTag reports whether a tag is cached.
func HasTag(namespaceRoot, repo, tag string) bool {
	return isRegularFile(TagLinkPath(namespaceRoot, repo, tag))
}

// HasRevision reports whether a manifest revision is cached.
func HasRevision(namespaceRoot, repo string, dgst digest.Digest) bool {
	if err := dgst.Validate(); err != nil {
		return false
	}
	p := filepath.Join(V2Root(namespaceRoot), "repositories", filepath.FromSlash(repo),
		"_manifests", "revisions", dgst.Algorithm().String(), dgst.Encoded(), "link")
	return isRegularFile(p)
}

// Repositories lists the repositories stored under a namespace root.
//
// A directory counts as a repository when it contains a _manifests directory,
// which is what the driver creates for every repository and never for an
// intermediate path component.
func Repositories(namespaceRoot string) ([]string, error) {
	base := filepath.Join(V2Root(namespaceRoot), "repositories")

	var out []string
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fs.SkipDir
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() != "_manifests" {
			return nil
		}
		rel, err := filepath.Rel(base, filepath.Dir(path))
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return fs.SkipDir
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(out)
	return out, nil
}

// RepositorySize returns the total size in bytes of a namespace root, used for
// the per-namespace cache metrics.
func RepositorySize(namespaceRoot string) (int64, error) {
	var total int64
	err := filepath.WalkDir(namespaceRoot, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// NamespaceFromRoot recovers the namespace from a namespace root path of the
// form <storage.root>/<namespace>.
func NamespaceFromRoot(storageRoot, namespaceRoot string) string {
	rel, err := filepath.Rel(storageRoot, namespaceRoot)
	if err != nil {
		return ""
	}
	return strings.Trim(filepath.ToSlash(rel), "/")
}
