package storage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"
)

const (
	testDigest = "sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6"
	testRepo   = "library/alpine"
	testTag    = "latest"
)

// seedLayout creates the subset of distribution's on-disk layout these helpers
// reason about.
func seedLayout(t *testing.T, root string) {
	t.Helper()

	dgst, err := digest.Parse(testDigest)
	if err != nil {
		t.Fatalf("parse digest: %v", err)
	}

	blobPath, err := BlobDataPath(root, dgst)
	if err != nil {
		t.Fatalf("BlobDataPath: %v", err)
	}
	writeFile(t, blobPath, "layer bytes")

	writeFile(t, TagLinkPath(root, testRepo, testTag), testDigest)

	revisionLink := filepath.Join(V2Root(root), "repositories", filepath.FromSlash(testRepo),
		"_manifests", "revisions", dgst.Algorithm().String(), dgst.Encoded(), "link")
	writeFile(t, revisionLink, testDigest)

	// A second repository, to prove Repositories walks the tree.
	writeFile(t, filepath.Join(V2Root(root), "repositories", "watsons", "app",
		"_manifests", "tags", "v1", "current", "link"), testDigest)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustDigest(t *testing.T, s string) digest.Digest {
	t.Helper()
	d, err := digest.Parse(s)
	if err != nil {
		t.Fatalf("parse digest %q: %v", s, err)
	}
	return d
}

func TestBlobDataPath(t *testing.T) {
	p, err := BlobDataPath("/data/docker.io", mustDigest(t, testDigest))
	if err != nil {
		t.Fatalf("BlobDataPath: %v", err)
	}

	// distribution stores blobs as blobs/<algo>/<first two hex>/<hex>/data.
	want := filepath.Join("/data/docker.io", "docker", "registry", "v2",
		"blobs", "sha256", "29", "294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6", "data")
	if p != want {
		t.Errorf("BlobDataPath = %q, want %q", p, want)
	}
}

func TestBlobDataPathRejectsInvalidDigest(t *testing.T) {
	if _, err := BlobDataPath("/data", digest.Digest("not-a-digest")); err == nil {
		t.Fatal("expected an error for an unparsable digest")
	}
}

func TestPresenceChecks(t *testing.T) {
	root := t.TempDir()
	dgst := mustDigest(t, testDigest)

	// Nothing seeded yet: every predicate is false, not an error.
	if HasBlob(root, dgst) {
		t.Error("HasBlob must be false on an empty root")
	}
	if HasTag(root, testRepo, testTag) {
		t.Error("HasTag must be false on an empty root")
	}
	if HasRevision(root, testRepo, dgst) {
		t.Error("HasRevision must be false on an empty root")
	}

	seedLayout(t, root)

	if !HasBlob(root, dgst) {
		t.Error("HasBlob must be true once the data file exists")
	}
	if !HasTag(root, testRepo, testTag) {
		t.Error("HasTag must be true once the tag link exists")
	}
	if !HasRevision(root, testRepo, dgst) {
		t.Error("HasRevision must be true once the revision link exists")
	}
	if HasTag(root, testRepo, "missing") {
		t.Error("HasTag must be false for an unknown tag")
	}
	if HasRevision(root, "other/repo", dgst) {
		t.Error("HasRevision must be false for another repository")
	}
	// A digest with no algorithm cannot be a revision path.
	if HasRevision(root, testRepo, digest.Digest("deadbeef")) {
		t.Error("HasRevision must be false for an algorithm-less digest")
	}
}

func TestRepositories(t *testing.T) {
	root := t.TempDir()

	// An absent layout is not an error: a namespace with no content yet.
	got, err := Repositories(root)
	if err != nil {
		t.Fatalf("Repositories on an empty root: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Repositories() = %v, want empty", got)
	}

	seedLayout(t, root)

	got, err = Repositories(root)
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	want := []string{"library/alpine", "watsons/app"}
	if len(got) != len(want) {
		t.Fatalf("Repositories() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Repositories() = %v, want %v (sorted)", got, want)
		}
	}
}

// TestRepositoriesIgnoresBareDirectories guards the definition of a repository:
// only a directory containing _manifests counts, so intermediate path components
// are never reported.
func TestRepositoriesIgnoresBareDirectories(t *testing.T) {
	root := t.TempDir()

	// A directory tree with no _manifests anywhere.
	if err := os.MkdirAll(filepath.Join(V2Root(root), "repositories", "orphan", "nested"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	got, err := Repositories(root)
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Repositories() = %v, want empty", got)
	}
}

func TestRepositorySize(t *testing.T) {
	root := t.TempDir()

	size, err := RepositorySize(root)
	if err != nil {
		t.Fatalf("RepositorySize: %v", err)
	}
	if size != 0 {
		t.Errorf("size = %d, want 0", size)
	}

	seedLayout(t, root)

	size, err = RepositorySize(root)
	if err != nil {
		t.Fatalf("RepositorySize: %v", err)
	}
	if size == 0 {
		t.Error("size should be non-zero after seeding")
	}

	missing, err := RepositorySize(filepath.Join(root, "does-not-exist"))
	if err != nil {
		t.Fatalf("RepositorySize on a missing root: %v", err)
	}
	if missing != 0 {
		t.Errorf("missing root size = %d, want 0", missing)
	}
}

func TestV2Root(t *testing.T) {
	if got, want := V2Root("/data/me"), filepath.Join("/data/me", "docker", "registry", "v2"); got != want {
		t.Errorf("V2Root = %q, want %q", got, want)
	}
}

func TestNamespaceFromRoot(t *testing.T) {
	if got, want := NamespaceFromRoot("/data", "/data/docker.io"), "docker.io"; got != want {
		t.Errorf("NamespaceFromRoot = %q, want %q", got, want)
	}
	if got := NamespaceFromRoot("/data", "/other/place"); got != "../other/place" {
		t.Errorf("unexpected result for an unrelated root: %q", got)
	}
}
