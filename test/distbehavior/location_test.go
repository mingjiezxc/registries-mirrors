// Package distbehavior pins down behaviours of the embedded
// distribution/v3 registry engine that rms's routing design depends on.
//
// These are not tests of rms itself: they are executable documentation of
// upstream behaviour, so an upgrade of distribution that changes any of them
// fails loudly here instead of silently corrupting push URLs or storage layout.
//
// Findings that shaped internal/namespace and internal/registryapp:
//
//  1. configuration.HTTP.Host is used as scheme+host ONLY. A path in it is
//     silently dropped, so it cannot be used to re-attach a namespace to the
//     URLs the engine generates.
//  2. Upload Location URLs are built from the origin (HTTP.Host, else the
//     request origin) plus the *repository name*. They do not echo the request
//     path. Therefore a namespace that accepts pushes must keep its namespace
//     prefix inside the repository name, or the Location it returns will point
//     at a URL the client cannot use.
//  3. handlers.NewApp panics when a proxy namespace's upstream is unreachable,
//     because it performs an HTTP GET against <remoteurl>/v2/ while
//     constructing the pull-through cache. Callers must probe first and
//     recover.
//  4. A proxy namespace rejects blob uploads (405), which is why push and
//     pull-through caching cannot share one namespace.
package distbehavior

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/distribution/distribution/v3/configuration"
	"github.com/distribution/distribution/v3/registry/handlers"

	// Storage drivers self-register with the driver factory from package
	// init(); without this import handlers.NewApp panics with
	// "StorageDriver not registered: filesystem".
	_ "github.com/distribution/distribution/v3/registry/storage/driver/filesystem"
)

func baseConfig(root string) *configuration.Configuration {
	cfg := &configuration.Configuration{}
	cfg.Log.Level = "error"
	cfg.Storage = configuration.Storage{
		"filesystem": configuration.Parameters{"rootdirectory": root},
	}
	// Blob reads must stream through rms rather than redirect to the backend.
	cfg.Storage["redirect"] = configuration.Parameters{"disable": true}
	return cfg
}

// newLocalApp builds a minimal writable registry app rooted at root.
func newLocalApp(t *testing.T, host, root string) *handlers.App {
	t.Helper()

	cfg := baseConfig(root)
	cfg.HTTP.Host = host
	app := handlers.NewApp(context.Background(), cfg)
	t.Cleanup(func() { _ = app.Shutdown() })
	return app
}

// stubUpstream is a registry endpoint that accepts an unauthenticated /v2/
// probe, which is all distribution needs to construct a proxy namespace.
func stubUpstream(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func startBlobUpload(t *testing.T, baseURL, path string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, baseURL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// Finding 1: a path in HTTP.Host is dropped. rms must therefore never rely on
// it to keep a namespace visible in generated URLs.
func TestHTTPHostPathIsDropped(t *testing.T) {
	app := newLocalApp(t, "http://example.test/alice", t.TempDir())
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)

	resp := startBlobUpload(t, srv.URL, "/v2/alice/app/blobs/uploads/")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST blob upload: got %d, want %d", resp.StatusCode, http.StatusAccepted)
	}

	loc := resp.Header.Get("Location")
	u := parseLocation(t, loc)

	if u.Host != "example.test" {
		t.Errorf("origin should come from HTTP.Host: got host %q, want %q", u.Host, "example.test")
	}
	if strings.HasPrefix(u.Path, "/alice/") {
		t.Errorf("HTTP.Host path was preserved, which contradicts the documented behaviour: %q", loc)
	}
	if want := "/v2/alice/app/blobs/uploads/"; !strings.HasPrefix(u.Path, want) {
		t.Errorf("Location path = %q, want prefix %q", u.Path, want)
	}
}

// Finding 2: the Location path is derived from the repository name, so a
// namespaced repository name yields a namespaced Location. This is the
// mechanism rms uses for pushable namespaces: do not strip the prefix.
func TestLocationIsBuiltFromRepositoryName(t *testing.T) {
	app := newLocalApp(t, "http://example.test", t.TempDir())
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)

	for _, repo := range []string{"alice/app", "me/image"} {
		resp := startBlobUpload(t, srv.URL, "/v2/"+repo+"/blobs/uploads/")
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("POST blob upload for %s: got %d, want %d", repo, resp.StatusCode, http.StatusAccepted)
		}

		u := parseLocation(t, resp.Header.Get("Location"))
		if want := "/v2/" + repo + "/blobs/uploads/"; !strings.HasPrefix(u.Path, want) {
			t.Errorf("repo %q: Location path = %q, want prefix %q", repo, u.Path, want)
		}
	}
}

// Finding 4: a proxy namespace is read-only.
func TestProxyNamespaceRejectsPush(t *testing.T) {
	upstream := stubUpstream(t)

	cfg := baseConfig(t.TempDir())
	cfg.Proxy.RemoteURL = upstream.URL

	app := handlers.NewApp(context.Background(), cfg)
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = app.Shutdown() })

	resp := startBlobUpload(t, srv.URL, "/v2/app/blobs/uploads/")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("proxy namespace accepted a blob upload: got %d, want %d",
			resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

// Finding 3: construction panics when the upstream is unreachable, so rms must
// probe before building a proxy namespace and recover around NewApp.
func TestNewAppPanicsWhenUpstreamUnreachable(t *testing.T) {
	// Port 1 on loopback is virtually guaranteed to refuse connections.
	cfg := baseConfig(t.TempDir())
	cfg.Proxy.RemoteURL = "http://127.0.0.1:1/unused"

	defer func() {
		if recover() == nil {
			t.Fatal("expected handlers.NewApp to panic for an unreachable upstream")
		}
	}()

	app := handlers.NewApp(context.Background(), cfg)
	_ = app.Shutdown()
	t.Fatal("unreachable")
}

func parseLocation(t *testing.T, loc string) *urlParts {
	t.Helper()

	if loc == "" {
		t.Fatal("response has no Location header")
	}
	i := strings.Index(loc, "://")
	if i < 0 {
		t.Fatalf("Location is not absolute: %q", loc)
	}
	rest := loc[i+3:]
	host, path, _ := strings.Cut(rest, "/")
	return &urlParts{Host: host, Path: "/" + path}
}

type urlParts struct {
	Host string
	Path string
}
