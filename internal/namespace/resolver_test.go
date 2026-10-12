package namespace

import (
	"errors"
	"testing"

	"github.com/watsons/rms/internal/config"
)

// testConfig mirrors the shape the README documents: docker.io as the default
// proxy namespace with library/ injection, two more proxy namespaces, and two
// writable local namespaces.
func testConfig() *config.Config {
	return &config.Config{
		Namespaces: config.Namespaces{
			Default: "docker.io",
			Local:   []string{"me"},
			Proxy: map[string]config.ProxyNamespace{
				"docker.io":       {Upstreams: []string{"https://registry-1.docker.io"}, OfficialLibrary: true},
				"ghcr.io":         {Upstreams: []string{"https://ghcr.io"}},
				"registry.k8s.io": {Upstreams: []string{"https://registry.k8s.io"}},
			},
		},
		Auth: config.Auth{
			SharedNamespaces: []string{"shared"},
		},
	}
}

func testTable(extraLocal ...string) *Table {
	cfg := testConfig()
	local := append([]string{"alice"}, extraLocal...)
	return NewTable(cfg, local)
}

func TestResolve(t *testing.T) {
	tbl := testTable()

	tests := []struct {
		name       string
		listenerNS string
		path       string
		wantNS     string
		wantMode   config.Mode
		wantKind   Kind
		wantEngine string
		wantRepo   string
	}{
		// --- base -----------------------------------------------------------
		{
			name: "api version probe", path: "/v2/",
			wantKind: KindBase,
		},
		{
			name: "api version probe without slash", path: "/v2",
			wantKind: KindBase,
		},

		// --- catalog --------------------------------------------------------
		{
			name: "catalog", path: "/v2/_catalog",
			wantKind: KindCatalog,
		},

		// --- explicit proxy namespace prefix --------------------------------
		{
			name: "explicit docker.io pull", path: "/v2/docker.io/library/alpine/manifests/latest",
			wantNS: "docker.io", wantMode: config.ModeProxy, wantKind: KindRepository,
			wantEngine: "/v2/library/alpine/manifests/latest", wantRepo: "library/alpine",
		},
		{
			name: "explicit docker.io blob", path: "/v2/docker.io/library/alpine/blobs/sha256:abc",
			wantNS: "docker.io", wantMode: config.ModeProxy, wantKind: KindRepository,
			wantEngine: "/v2/library/alpine/blobs/sha256:abc", wantRepo: "library/alpine",
		},
		{
			name: "explicit ghcr.io pull keeps owner", path: "/v2/ghcr.io/watsons/app/manifests/v1",
			wantNS: "ghcr.io", wantMode: config.ModeProxy, wantKind: KindRepository,
			wantEngine: "/v2/watsons/app/manifests/v1", wantRepo: "watsons/app",
		},
		{
			name:   "explicit prefix with single segment injects library for docker.io",
			path:   "/v2/docker.io/alpine/manifests/latest",
			wantNS: "docker.io", wantMode: config.ModeProxy, wantKind: KindRepository,
			wantEngine: "/v2/library/alpine/manifests/latest", wantRepo: "library/alpine",
		},
		{
			name:   "no library injection for a namespace without official_library",
			path:   "/v2/ghcr.io/app/manifests/latest",
			wantNS: "ghcr.io", wantMode: config.ModeProxy, wantKind: KindRepository,
			wantEngine: "/v2/app/manifests/latest", wantRepo: "app",
		},
		{
			name: "namespace with nothing after it", path: "/v2/docker.io",
			wantNS: "docker.io", wantMode: config.ModeProxy, wantKind: KindRepository,
			wantEngine: "/v2/", wantRepo: "",
		},
		{
			name: "tags list", path: "/v2/docker.io/library/alpine/tags/list",
			wantNS: "docker.io", wantMode: config.ModeProxy, wantKind: KindRepository,
			wantEngine: "/v2/library/alpine/tags/list", wantRepo: "library/alpine",
		},

		// --- default namespace rewrite (requirement 4) -----------------------
		{
			name:   "unqualified user repo falls back to docker.io",
			path:   "/v2/someuser/app/manifests/v1",
			wantNS: "docker.io", wantMode: config.ModeProxy, wantKind: KindRepository,
			wantEngine: "/v2/someuser/app/manifests/v1", wantRepo: "someuser/app",
		},
		{
			name:   "unqualified single segment becomes a library image",
			path:   "/v2/alpine/manifests/latest",
			wantNS: "docker.io", wantMode: config.ModeProxy, wantKind: KindRepository,
			wantEngine: "/v2/library/alpine/manifests/latest", wantRepo: "library/alpine",
		},
		{
			name: "unqualified official library explicit", path: "/v2/library/nginx/manifests/latest",
			wantNS: "docker.io", wantMode: config.ModeProxy, wantKind: KindRepository,
			wantEngine: "/v2/library/nginx/manifests/latest", wantRepo: "library/nginx",
		},

		// --- local namespaces keep the prefix (pushable) --------------------
		{
			name:   "local namespace keeps the prefix for uploads",
			path:   "/v2/alice/app/blobs/uploads/",
			wantNS: "alice", wantMode: config.ModeLocal, wantKind: KindRepository,
			wantEngine: "/v2/alice/app/blobs/uploads/", wantRepo: "alice/app",
		},
		{
			name:   "local namespace manifest push",
			path:   "/v2/alice/app/manifests/v1",
			wantNS: "alice", wantMode: config.ModeLocal, wantKind: KindRepository,
			wantEngine: "/v2/alice/app/manifests/v1", wantRepo: "alice/app",
		},
		{
			name: "configured local namespace", path: "/v2/me/image/manifests/v1",
			wantNS: "me", wantMode: config.ModeLocal, wantKind: KindRepository,
			wantEngine: "/v2/me/image/manifests/v1", wantRepo: "me/image",
		},
		{
			name: "shared namespace", path: "/v2/shared/image/manifests/v1",
			wantNS: "shared", wantMode: config.ModeLocal, wantKind: KindRepository,
			wantEngine: "/v2/shared/image/manifests/v1", wantRepo: "shared/image",
		},
		{
			name:   "local namespace wins over the default rewrite",
			path:   "/v2/alice/manifests/v1",
			wantNS: "alice", wantMode: config.ModeLocal, wantKind: KindRepository,
			wantEngine: "/v2/alice/manifests/v1", wantRepo: "alice",
		},

		// --- namespace-bound listeners (containerd registries.yaml) ---------
		{
			name: "bound listener supplies the namespace", listenerNS: "docker.io",
			path:   "/v2/library/alpine/manifests/latest",
			wantNS: "docker.io", wantMode: config.ModeProxy, wantKind: KindRepository,
			wantEngine: "/v2/library/alpine/manifests/latest", wantRepo: "library/alpine",
		},
		{
			name: "bound listener injects library for a single segment", listenerNS: "docker.io",
			path:   "/v2/alpine/manifests/latest",
			wantNS: "docker.io", wantMode: config.ModeProxy, wantKind: KindRepository,
			wantEngine: "/v2/library/alpine/manifests/latest", wantRepo: "library/alpine",
		},
		{
			name: "bound listener tolerates a redundant namespace prefix", listenerNS: "docker.io",
			path:   "/v2/docker.io/library/alpine/manifests/latest",
			wantNS: "docker.io", wantMode: config.ModeProxy, wantKind: KindRepository,
			wantEngine: "/v2/library/alpine/manifests/latest", wantRepo: "library/alpine",
		},
		{
			name: "bound ghcr listener keeps the owner path", listenerNS: "ghcr.io",
			path:   "/v2/watsons/app/manifests/v1",
			wantNS: "ghcr.io", wantMode: config.ModeProxy, wantKind: KindRepository,
			wantEngine: "/v2/watsons/app/manifests/v1", wantRepo: "watsons/app",
		},
		{
			name: "bound listener still exposes the api root", listenerNS: "docker.io",
			path: "/v2/", wantKind: KindBase,
		},
		{
			name: "bound listener handles catalog", listenerNS: "docker.io",
			path: "/v2/_catalog", wantKind: KindCatalog,
		},
		{
			name: "bound local listener keeps the prefix", listenerNS: "alice",
			path:   "/v2/app/manifests/v1",
			wantNS: "alice", wantMode: config.ModeLocal, wantKind: KindRepository,
			wantEngine: "/v2/app/manifests/v1", wantRepo: "app",
		},
		// A repository legitimately named "manifests-tool" must not be
		// truncated by the best-effort repo extractor.
		{
			name: "repo containing an api-like segment", path: "/v2/docker.io/myorg/manifests-tool/manifests/v1",
			wantNS: "docker.io", wantMode: config.ModeProxy, wantKind: KindRepository,
			wantEngine: "/v2/myorg/manifests-tool/manifests/v1", wantRepo: "myorg/manifests-tool",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tbl.Resolve(tc.listenerNS, tc.path)
			if err != nil {
				t.Fatalf("Resolve(%q, %q): %v", tc.listenerNS, tc.path, err)
			}
			if got.Namespace != tc.wantNS {
				t.Errorf("Namespace = %q, want %q", got.Namespace, tc.wantNS)
			}
			if got.Mode != tc.wantMode {
				t.Errorf("Mode = %q, want %q", got.Mode, tc.wantMode)
			}
			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %v, want %v", got.Kind, tc.wantKind)
			}
			if got.EnginePath != tc.wantEngine {
				t.Errorf("EnginePath = %q, want %q", got.EnginePath, tc.wantEngine)
			}
			if got.Repo != tc.wantRepo {
				t.Errorf("Repo = %q, want %q", got.Repo, tc.wantRepo)
			}
		})
	}
}

func TestResolveRejectsNonV2Paths(t *testing.T) {
	tbl := testTable()
	for _, p := range []string{"/", "/healthz", "/api/version", "/v3/", ""} {
		_, err := tbl.Resolve("", p)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("Resolve(%q) error = %v, want ErrNotFound", p, err)
		}
	}
}

func TestResolveUnknownListenerNamespace(t *testing.T) {
	tbl := testTable()
	_, err := tbl.Resolve("nope", "/v2/app/manifests/v1")
	if err == nil {
		t.Fatal("expected an error for an unconfigured listener namespace")
	}
}

// TestNoLibraryInjectionForLocalNamespaces guards requirement 3/4 ordering: a
// writable namespace is never rewritten into a library image.
func TestNoLibraryInjectionForLocalNamespaces(t *testing.T) {
	tbl := testTable()
	got, err := tbl.Resolve("", "/v2/me/alpine/manifests/v1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Namespace != "me" {
		t.Fatalf("Namespace = %q, want me", got.Namespace)
	}
	if want := "/v2/me/alpine/manifests/v1"; got.EnginePath != want {
		t.Errorf("EnginePath = %q, want %q", got.EnginePath, want)
	}
}

func TestTablePredicates(t *testing.T) {
	tbl := testTable()

	if !tbl.IsLocal("alice") {
		t.Error("htpasswd-derived namespace alice should be local")
	}
	if !tbl.IsLocal("me") {
		t.Error("configured namespace me should be local")
	}
	if !tbl.IsLocal("shared") {
		t.Error("shared namespace should be local")
	}
	if tbl.IsLocal("docker.io") {
		t.Error("docker.io must not be local")
	}
	if !tbl.IsProxy("docker.io") {
		t.Error("docker.io should be a proxy namespace")
	}
	if got, want := tbl.Default(), "docker.io"; got != want {
		t.Errorf("Default() = %q, want %q", got, want)
	}

	want := []string{"alice", "me", "shared"}
	got := tbl.LocalNames()
	if len(got) != len(want) {
		t.Fatalf("LocalNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("LocalNames() = %v, want %v (sorted)", got, want)
		}
	}
}

func TestNewTableSkipsEmptyLocalNames(t *testing.T) {
	tbl := NewTable(testConfig(), []string{"", "bob"})
	if tbl.IsLocal("") {
		t.Error("empty namespace must not enter the table")
	}
	if !tbl.IsLocal("bob") {
		t.Error("bob should be local")
	}
}
