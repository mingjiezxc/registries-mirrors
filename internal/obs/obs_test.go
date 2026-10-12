package obs

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/watsons/rms/internal/config"
)

func TestSpanPathKeepsCardinalityLow(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "manifest by tag loses the tag",
			path: "/v2/docker.io/library/alpine/manifests/latest",
			want: "/v2/docker.io/library/alpine/manifests",
		},
		{
			name: "manifest by digest loses the digest",
			path: "/v2/docker.io/library/alpine/manifests/sha256:29",
			want: "/v2/docker.io/library/alpine/manifests",
		},
		{
			name: "blob loses the digest",
			path: "/v2/docker.io/library/alpine/blobs/sha256:66",
			want: "/v2/docker.io/library/alpine/blobs",
		},
		{
			name: "upload session",
			path: "/v2/alice/app/blobs/uploads/abc",
			want: "/v2/alice/app/blobs",
		},
		{
			name: "tags list",
			path: "/v2/docker.io/library/alpine/tags/list",
			want: "/v2/docker.io/library/alpine/tags",
		},
		{
			name: "api root is unchanged",
			path: "/v2/",
			want: "/v2/",
		},
		{
			name: "non-registry path is unchanged",
			path: "/healthz",
			want: "/healthz",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := spanPath(tc.path); got != tc.want {
				t.Errorf("spanPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		in      string
		wantOK  bool
		wantStr string
	}{
		{in: "", wantOK: true, wantStr: "INFO"},
		{in: "debug", wantOK: true, wantStr: "DEBUG"},
		{in: "INFO", wantOK: true, wantStr: "INFO"},
		{in: " warn ", wantOK: true, wantStr: "WARN"},
		{in: "warning", wantOK: true, wantStr: "WARN"},
		{in: "error", wantOK: true, wantStr: "ERROR"},
		{in: "verbose", wantOK: false},
	}

	for _, tc := range tests {
		level, err := parseLevel(tc.in)
		if tc.wantOK && err != nil {
			t.Errorf("parseLevel(%q): %v", tc.in, err)
			continue
		}
		if !tc.wantOK {
			if err == nil {
				t.Errorf("parseLevel(%q) should fail", tc.in)
			}
			continue
		}
		if level.String() != tc.wantStr {
			t.Errorf("parseLevel(%q) = %s, want %s", tc.in, level, tc.wantStr)
		}
	}
}

func TestNewLoggerFormats(t *testing.T) {
	var buf bytes.Buffer

	logger, err := NewLogger(config.Logs{Level: "debug", Format: "json"}, &buf)
	if err != nil {
		t.Fatalf("NewLogger(json): %v", err)
	}
	logger.Debug("hello", "key", "value")
	if !strings.Contains(buf.String(), `"msg":"hello"`) {
		t.Errorf("json output = %q", buf.String())
	}

	buf.Reset()
	logger, err = NewLogger(config.Logs{Level: "info", Format: "text"}, &buf)
	if err != nil {
		t.Fatalf("NewLogger(text): %v", err)
	}
	logger.Info("hello")
	if !strings.Contains(buf.String(), "msg=hello") {
		t.Errorf("text output = %q", buf.String())
	}

	if _, err := NewLogger(config.Logs{Format: "logfmt"}, &buf); err == nil {
		t.Error("expected an error for an unsupported format")
	}
	if _, err := NewLogger(config.Logs{Level: "loud", Format: "json"}, &buf); err == nil {
		t.Error("expected an error for an unsupported level")
	}
}

func TestConfigureLogrusRejectsBadLevel(t *testing.T) {
	if err := ConfigureLogrus(config.Logs{Level: "shout", Format: "json"}, &bytes.Buffer{}); err == nil {
		t.Error("expected an error for an unsupported logrus level")
	}
	if err := ConfigureLogrus(config.Logs{Level: "error", Format: "json"}, &bytes.Buffer{}); err != nil {
		t.Errorf("ConfigureLogrus: %v", err)
	}
}

// TestMetricsPathIsValidated covers the mistake that produced a nil-pointer-free
// but wrong deployment: a metrics path without a leading slash.
func TestMetricsPathIsValidated(t *testing.T) {
	if _, err := NewMetrics("metrics"); err == nil {
		t.Fatal("expected an error for a relative metrics path")
	}

	m, err := NewMetrics("/metrics")
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	if m.Handler() == nil {
		t.Fatal("Handler() must not be nil")
	}
}

// TestMetricsEndpointAndSeries checks that the recorder produces scoped series
// and that unknown paths are not answered by the metrics mux.
func TestMetricsEndpointAndSeries(t *testing.T) {
	m, err := NewMetrics("/metrics")
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	m.SetBuildInfo("v9", "abc123")

	m.Request("", "docker.io", http.MethodGet, http.StatusOK, 1500_000_000) // 1.5s
	// Paths that belong to no namespace (the API root, the catalog) must be
	// labelled rather than dropped.
	m.Request("", "", http.MethodGet, http.StatusOK, time.Millisecond)
	m.CacheHit("docker.io")
	m.CacheMiss("ghcr.io")
	m.NamespaceReady("docker.io")
	m.NamespaceDegraded("ghcr.io", nil)
	m.UpstreamElectionChanged("docker.io", "http://mirror")

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", rec.Code)
	}

	body := rec.Body.String()
	for _, want := range []string{
		`rms_http_requests_total{listener="<multi>",method="GET",namespace="docker.io",status="200"} 1`,
		`rms_cache_hits_total{namespace="docker.io"} 1`,
		`rms_cache_misses_total{namespace="ghcr.io"} 1`,
		`rms_namespace_up{namespace="docker.io"} 1`,
		`rms_namespace_up{namespace="ghcr.io"} 0`,
		`rms_upstream_election_changes_total{namespace="docker.io",upstream="http://mirror"} 1`,
		`rms_build_info{commit="abc123",version="v9"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics is missing %q", want)
		}
	}
	if !strings.Contains(body, `rms_http_requests_total{listener="<multi>",method="GET",namespace="none",status="200"} 1`) {
		t.Errorf("/metrics should label a namespace-less request as none: %s", firstLines(body, 3))
	}

	// A scrape endpoint that answers every path leaks metrics accidentally.
	rec = httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("/nope = %d, want 404", rec.Code)
	}
}

// TestMetricsDegradedWithErrorCountsOnce makes sure a degraded namespace with a
// cause also increments the error counter.
func TestMetricsDegradedWithErrorCountsOnce(t *testing.T) {
	m, err := NewMetrics("/metrics")
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	m.NamespaceDegraded("docker.io", errBang{})

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	if !strings.Contains(body, `rms_namespace_errors_total{namespace="docker.io"} 1`) {
		t.Errorf("errors counter missing: %s", firstLines(body, 6))
	}
}

type errBang struct{}

func (errBang) Error() string { return "bang" }

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
