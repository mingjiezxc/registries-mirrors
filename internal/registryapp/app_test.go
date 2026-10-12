package registryapp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/watsons/rms/internal/config"
)

// TestDeriveHTTPSecretIsStable pins the property the derived secret exists for:
// blob uploads must survive a restart, which a per-start random secret would
// silently break.
func TestDeriveHTTPSecretIsStable(t *testing.T) {
	a := DeriveHTTPSecret("/data")
	b := DeriveHTTPSecret("/data")
	if a != b {
		t.Errorf("the derived secret is not stable: %q vs %q", a, b)
	}
	if len(a) != 32 {
		t.Errorf("derived secret length = %d, want 32 hex chars", len(a))
	}
	if c := DeriveHTTPSecret("/other"); c == a {
		t.Error("different storage roots should not share a secret")
	}
}

func TestBuildRejectsIncompleteSpecs(t *testing.T) {
	tests := []struct {
		name    string
		spec    Spec
		wantErr string
	}{
		{
			name:    "empty namespace",
			spec:    Spec{Mode: config.ModeLocal, Root: t.TempDir()},
			wantErr: "namespace must not be empty",
		},
		{
			name:    "missing root",
			spec:    Spec{Namespace: "me", Mode: config.ModeLocal},
			wantErr: "storage root must not be empty",
		},
		{
			name:    "proxy namespace without an upstream",
			spec:    Spec{Namespace: "docker.io", Mode: config.ModeProxy, Root: t.TempDir()},
			wantErr: "requires a remote URL",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			app, err := Build(tc.spec)
			if err == nil {
				_ = app.Shutdown()
				t.Fatalf("expected an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestBuildLocalServesTheApiRoot checks that a local namespace engine is
// constructible and answers the registry API root.
func TestBuildLocalServesTheApiRoot(t *testing.T) {
	root := t.TempDir()

	app, err := Build(Spec{
		Namespace:     "me",
		Mode:          config.ModeLocal,
		Root:          root,
		HTTPSecret:    "test-secret",
		DeleteEnabled: true,
		UploadPurge: config.UploadPurge{
			Enabled: true,
			Age:     168 * 3600e9, // 168h
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Shutdown() })

	if app.Namespace != "me" || app.Mode != config.ModeLocal {
		t.Errorf("app identity = %q/%q", app.Namespace, app.Mode)
	}

	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v2/")
	if err != nil {
		t.Fatalf("GET /v2/: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/v2/ = %d, want 200", resp.StatusCode)
	}
}

// TestBuildReadOnlyRejectsWrites covers the engine used for the degraded
// cache-only mode: it must serve reads and refuse uploads.
func TestBuildReadOnlyRejectsWrites(t *testing.T) {
	app, err := Build(Spec{
		Namespace: "docker.io",
		Mode:      config.ModeLocal,
		Root:      t.TempDir(),
		ReadOnly:  true,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Shutdown() })

	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v2/")
	if err != nil {
		t.Fatalf("GET /v2/: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("read-only /v2/ = %d, want 200", resp.StatusCode)
	}

	resp, err = http.Post(srv.URL+"/v2/me/app/blobs/uploads/", "", nil)
	if err != nil {
		t.Fatalf("POST upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("upload to a read-only engine = %d, want 405", resp.StatusCode)
	}
}

// TestBuildUsesTheGivenRoot proves the per-namespace storage boundary: after an
// upload the bytes must be under the requested root and nowhere else.
func TestBuildUsesTheGivenRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "docker.io")

	app, err := Build(Spec{
		Namespace:  "docker.io",
		Mode:       config.ModeLocal,
		Root:       root,
		HTTPSecret: "test-secret",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Shutdown() })

	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)

	// Starting an upload is enough to make the driver create its layout.
	resp, err := http.Post(srv.URL+"/v2/me/app/blobs/uploads/", "", nil)
	if err != nil {
		t.Fatalf("POST upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST upload = %d, want 202", resp.StatusCode)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read root %s: %v", root, err)
	}
	if len(entries) == 0 {
		t.Errorf("nothing was written under %s", root)
	}
}

// TestBuildProxyNamespaceNeedsAReachableUpstream documents the upstream panic
// that Build converts into an error.
func TestBuildProxyNamespaceNeedsAReachableUpstream(t *testing.T) {
	// Port 1 on loopback refuses connections.
	app, err := Build(Spec{
		Namespace: "docker.io",
		Mode:      config.ModeProxy,
		Root:      t.TempDir(),
		RemoteURL: "http://127.0.0.1:1",
	})
	if err == nil {
		_ = app.Shutdown()
		t.Fatal("expected an error for an unreachable upstream")
	}
	if !strings.Contains(err.Error(), "registry engine init failed") {
		t.Errorf("error = %v, want it to mention the engine init failure", err)
	}
}

// TestBuildIsIndependentPerRoot guards against engine state leaking between
// namespaces: a second engine on its own root must not see the first's content.
func TestBuildIsIndependentPerRoot(t *testing.T) {
	newEngine := func(t *testing.T, root string) *httptest.Server {
		t.Helper()
		app, err := Build(Spec{Namespace: "local", Mode: config.ModeLocal, Root: root, HTTPSecret: "s"})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		t.Cleanup(func() { _ = app.Shutdown() })
		srv := httptest.NewServer(app)
		t.Cleanup(srv.Close)
		return srv
	}

	base := t.TempDir()
	one := newEngine(t, filepath.Join(base, "one"))
	two := newEngine(t, filepath.Join(base, "two"))

	if resp, err := http.Post(one.URL+"/v2/local/app/blobs/uploads/", "", nil); err != nil {
		t.Fatalf("upload to one: %v", err)
	} else {
		resp.Body.Close()
	}

	if _, err := os.Stat(filepath.Join(base, "one")); err != nil {
		t.Errorf("namespace one wrote nothing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "two")); err == nil {
		t.Error("namespace two should not have been touched")
	}
	_ = two
	_ = context.Background()
}
