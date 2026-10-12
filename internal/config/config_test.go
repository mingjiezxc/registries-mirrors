package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// minimal returns a configuration that passes validation with auth disabled.
func minimal() string {
	return `
server:
  listen: "0.0.0.0:5000"
  external_url: "http://localhost:5000"
auth:
  type: none
namespaces:
  default: docker.io
  local: [me]
  proxy:
    docker.io:
      upstreams: ["https://registry-1.docker.io"]
      official_library: true
`
}

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rms.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

func TestLoadMinimalAppliesDefaults(t *testing.T) {
	cfg, err := Load(writeTemp(t, minimal()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got, want := cfg.Storage.Root, "/data"; got != want {
		t.Errorf("storage.root = %q, want %q", got, want)
	}
	if got, want := cfg.Auth.Realm, "rms"; got != want {
		t.Errorf("auth.realm = %q, want %q", got, want)
	}
	if got, want := cfg.Upstream.Health.Interval.Std(), 30*time.Second; got != want {
		t.Errorf("upstream.health.interval = %v, want %v", got, want)
	}
	if got, want := cfg.Upstream.Health.FailThreshold, 2; got != want {
		t.Errorf("upstream.health.fail_threshold = %d, want %d", got, want)
	}
	if got, want := cfg.Upstream.Health.Path, "/v2/"; got != want {
		t.Errorf("upstream.health.path = %q, want %q", got, want)
	}
	if got, want := cfg.Observability.Traces.Endpoint, "192.168.188.103:4317"; got != want {
		t.Errorf("traces.endpoint = %q, want %q", got, want)
	}
	if got, want := cfg.Observability.Metrics.Path, "/metrics"; got != want {
		t.Errorf("metrics.path = %q, want %q", got, want)
	}
	if got, want := cfg.NamespaceRoot("docker.io"), "/data/docker.io"; got != want {
		t.Errorf("NamespaceRoot(docker.io) = %q, want %q", got, want)
	}
	if !cfg.IsLocal("me") {
		t.Error("IsLocal(me) = false, want true")
	}
	if cfg.IsLocal("docker.io") {
		t.Error("IsLocal(docker.io) = true, want false")
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	body := minimal() + "\nzuperfluous: true\n"
	_, err := Load(writeTemp(t, body))
	if err == nil {
		t.Fatal("expected an error for an unknown key")
	}
	if !strings.Contains(err.Error(), "zuperfluous") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

func TestDurationAcceptsStringsAndSeconds(t *testing.T) {
	body := minimal() + `
upstream:
  health:
    interval: 45s
    timeout: 7
`
	cfg, err := Load(writeTemp(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Upstream.Health.Interval.Std(), 45*time.Second; got != want {
		t.Errorf("interval = %v, want %v", got, want)
	}
	if got, want := cfg.Upstream.Health.Timeout.Std(), 7*time.Second; got != want {
		t.Errorf("timeout = %v, want %v", got, want)
	}
}

func TestDurationRejectsGarbage(t *testing.T) {
	body := minimal() + `
upstream:
  health:
    interval: soon
`
	if _, err := Load(writeTemp(t, body)); err == nil {
		t.Fatal("expected an error for a non-duration value")
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("RMS_TEST_PW", "s3cret")

	got, err := ExpandEnv("a: ${RMS_TEST_PW}\nb: ${RMS_TEST_MISSING:-fallback}\nc: ${RMS_TEST_EMPTY:-x}")
	if err != nil {
		t.Fatalf("ExpandEnv: %v", err)
	}
	for _, want := range []string{"a: s3cret", "b: fallback", "c: x"} {
		if !strings.Contains(got, want) {
			t.Errorf("ExpandEnv output missing %q:\n%s", want, got)
		}
	}
}

func TestExpandEnvFailsOnUndefinedVariable(t *testing.T) {
	_, err := ExpandEnv("password: ${RMS_TEST_ABSENT_XYZ}")
	if err == nil {
		t.Fatal("expected an error for an undefined variable")
	}
	if !strings.Contains(err.Error(), "RMS_TEST_ABSENT_XYZ") {
		t.Errorf("error should name the variable, got: %v", err)
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:    "default namespace not defined",
			mutate:  func(c *Config) { c.Namespaces.Default = "ghcr.io" },
			wantErr: "namespaces.default",
		},
		{
			name: "proxy key is not domain-like",
			mutate: func(c *Config) {
				c.Namespaces.Proxy["dockerhub"] = ProxyNamespace{Upstreams: []string{"https://example.com"}}
			},
			wantErr: "must look like a registry host",
		},
		{
			name: "local namespace looks like a domain",
			mutate: func(c *Config) {
				c.Namespaces.Local = append(c.Namespaces.Local, "foo.bar")
			},
			wantErr: "must not look like a registry host",
		},
		{
			name: "external url with a path",
			mutate: func(c *Config) {
				c.Server.ExternalURL = "http://localhost:5000/alice"
			},
			wantErr: "must not contain a path",
		},
		{
			name: "external url with a bad scheme",
			mutate: func(c *Config) {
				c.Server.ExternalURL = "ftp://localhost:5000"
			},
			wantErr: "scheme must be http or https",
		},
		{
			name: "upstream with a path",
			mutate: func(c *Config) {
				ns := c.Namespaces.Proxy["docker.io"]
				ns.Upstreams = []string{"https://registry-1.docker.io/v2"}
				c.Namespaces.Proxy["docker.io"] = ns
			},
			wantErr: "must not contain a path",
		},
		{
			name: "upstream with no scheme",
			mutate: func(c *Config) {
				ns := c.Namespaces.Proxy["docker.io"]
				ns.Upstreams = []string{"registry-1.docker.io"}
				c.Namespaces.Proxy["docker.io"] = ns
			},
			wantErr: "scheme must be http or https",
		},
		{
			name: "no upstreams",
			mutate: func(c *Config) {
				ns := c.Namespaces.Proxy["docker.io"]
				ns.Upstreams = nil
				c.Namespaces.Proxy["docker.io"] = ns
			},
			wantErr: "at least one upstream",
		},
		{
			name:    "no proxy namespaces at all",
			mutate:  func(c *Config) { c.Namespaces.Proxy = map[string]ProxyNamespace{} },
			wantErr: "at least one proxy namespace",
		},
		{
			name: "duplicate listener",
			mutate: func(c *Config) {
				c.Listeners = []Listener{{Listen: "0.0.0.0:5000", Namespace: "docker.io"}}
			},
			wantErr: "declared twice",
		},
		{
			name: "listener namespace unknown",
			mutate: func(c *Config) {
				c.Listeners = []Listener{{Listen: "0.0.0.0:5001", Namespace: "quay.io"}}
			},
			wantErr: "is not defined",
		},
		{
			name: "metrics listener clashes with the registry",
			mutate: func(c *Config) {
				c.Observability.Metrics.Listen = c.Server.Listen
			},
			wantErr: "must differ",
		},
		{
			name: "invalid listener address",
			mutate: func(c *Config) {
				c.Server.Listen = "not-an-address"
			},
			wantErr: "invalid address",
		},
		{
			name: "bad log format",
			mutate: func(c *Config) {
				c.Observability.Logs.Format = "logfmt"
			},
			wantErr: "logs.format",
		},
		{
			name: "htpasswd file missing",
			mutate: func(c *Config) {
				c.Auth.Type = "htpasswd"
				c.Auth.Htpasswd = filepath.Join(t.TempDir(), "nope")
			},
			wantErr: "auth.htpasswd",
		},
		{
			name: "user namespaces without htpasswd",
			mutate: func(c *Config) {
				c.Auth.Type = "none"
				c.Auth.UserNamespace = true
			},
			wantErr: "requires auth.type",
		},
		{
			name: "bad auth type",
			mutate: func(c *Config) {
				c.Auth.Type = "ldap"
			},
			wantErr: `must be "none" or "htpasswd"`,
		},
		{
			name: "relative storage root",
			mutate: func(c *Config) {
				c.Storage.Root = "data"
			},
			wantErr: "absolute path",
		},
		{
			name: "negative ttl",
			mutate: func(c *Config) {
				ns := c.Namespaces.Proxy["docker.io"]
				ns.TTL = Duration(-time.Hour)
				c.Namespaces.Proxy["docker.io"] = ns
			},
			wantErr: "must not be negative",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(minimal()))
			if err != nil {
				t.Fatalf("baseline config must parse: %v", err)
			}
			tc.mutate(cfg)

			err = cfg.Validate()
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v\nwant it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestEffectiveListeners(t *testing.T) {
	body := minimal() + `
listeners:
  - listen: "0.0.0.0:5000"
    namespace: ""
  - listen: "0.0.0.0:5001"
    namespace: docker.io
  - listen: "0.0.0.0:5002"
    external_url: "http://mirror.test:9000"
`
	cfg, err := Load(writeTemp(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got := cfg.EffectiveListeners()
	if len(got) != 3 {
		t.Fatalf("got %d listeners, want 3: %+v", len(got), got)
	}
	if got[0].Listen != "0.0.0.0:5000" || got[0].Namespace != "" {
		t.Errorf("primary listener = %+v", got[0])
	}
	if got[0].ExternalURL != "http://localhost:5000" {
		t.Errorf("primary external url = %q", got[0].ExternalURL)
	}
	// 5001 is namespace-bound: its external URL inherits the primary host and
	// takes the listener's own port.
	if got[1].Namespace != "docker.io" {
		t.Errorf("listener 5001 namespace = %q", got[1].Namespace)
	}
	if want := "http://localhost:5001"; got[1].ExternalURL != want {
		t.Errorf("listener 5001 external url = %q, want %q", got[1].ExternalURL, want)
	}
	if want := "http://mirror.test:9000"; got[2].ExternalURL != want {
		t.Errorf("listener 5002 external url = %q, want %q", got[2].ExternalURL, want)
	}
}

func TestNamespaceHTTPOverrideInheritsDefaults(t *testing.T) {
	// Written out in full rather than appended to minimal(): YAML forbids a
	// duplicated mapping key, so "namespaces" cannot appear twice.
	body := `
server:
  listen: "0.0.0.0:5000"
auth:
  type: none
upstream:
  http:
    proxy: "http://192.168.188.1:3128"
    no_proxy: "10.82.0.0/16"
    dial_timeout: 3s
namespaces:
  default: docker.io
  proxy:
    docker.io:
      upstreams: ["https://registry-1.docker.io"]
    ghcr.io:
      upstreams: ["https://ghcr.io"]
      http:
        dial_timeout: 12s
`
	cfg, err := Load(writeTemp(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	ghcr := cfg.HTTPFor("ghcr.io")
	if got, want := ghcr.DialTimeout.Std(), 12*time.Second; got != want {
		t.Errorf("ghcr dial_timeout = %v, want %v", got, want)
	}
	if ghcr.Proxy != "http://192.168.188.1:3128" {
		t.Errorf("ghcr proxy should inherit the default, got %q", ghcr.Proxy)
	}
	if ghcr.NoProxy != "10.82.0.0/16" {
		t.Errorf("ghcr no_proxy should inherit the default, got %q", ghcr.NoProxy)
	}

	docker := cfg.HTTPFor("docker.io")
	if got, want := docker.DialTimeout.Std(), 3*time.Second; got != want {
		t.Errorf("docker.io dial_timeout = %v, want %v", got, want)
	}
}

// TestCommentsAreNotExpanded guards the failure mode where a config file that
// merely documents "${VAR}" in a comment refuses to start.
func TestCommentsAreNotExpanded(t *testing.T) {
	body := "# reference values as ${VAR} or ${VAR:-default}\n" +
		"# and secrets as ${RMS_TEST_NOT_SET}\n" + minimal()

	cfg, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse must ignore environment references inside comments: %v", err)
	}
	if cfg.Namespaces.Default != "docker.io" {
		t.Errorf("Default = %q", cfg.Namespaces.Default)
	}
}

// TestKeysAreNotExpanded verifies an environment variable cannot rename a
// configuration key.
func TestKeysAreNotExpanded(t *testing.T) {
	t.Setenv("RMS_TEST_KEY", "listen")
	body := `
server:
  "${RMS_TEST_KEY}": "0.0.0.0:5000"
auth:
  type: none
namespaces:
  default: docker.io
  proxy:
    docker.io:
      upstreams: ["https://registry-1.docker.io"]
`
	_, err := Parse([]byte(body))
	if err == nil {
		t.Fatal("expected an unknown-key error for the unexpanded key")
	}
	if !strings.Contains(err.Error(), "RMS_TEST_KEY") {
		t.Errorf("error should show the literal key, got: %v", err)
	}
}

// TestDurationAcceptsNumericString covers ${VAR:-0}, which expands into a quoted
// scalar and therefore stays a YAML string.
func TestDurationAcceptsNumericString(t *testing.T) {
	body := `
server:
  listen: "0.0.0.0:5000"
auth:
  type: none
namespaces:
  default: docker.io
  proxy:
    docker.io:
      upstreams: ["https://registry-1.docker.io"]
      ttl: "120"
`
	cfg, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := cfg.Namespaces.Proxy["docker.io"].TTL.Std(), 120*time.Second; got != want {
		t.Errorf("ttl = %v, want %v", got, want)
	}
}

func TestIsDomainLike(t *testing.T) {
	for _, tc := range []struct {
		first string
		want  bool
	}{
		{"docker.io", true},
		{"ghcr.io", true},
		{"localhost", true},
		{"registry:5000", true},
		{"me", false},
		{"alice", false},
		{"library", false},
	} {
		if got := IsDomainLike(tc.first); got != tc.want {
			t.Errorf("IsDomainLike(%q) = %v, want %v", tc.first, got, tc.want)
		}
	}
}

func TestHTTPSecretFromEnv(t *testing.T) {
	t.Setenv(DefaultHTTPSecretEnv, "super-secret")
	cfg, err := Load(writeTemp(t, minimal()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Server.HTTPSecret, "super-secret"; got != want {
		t.Errorf("http_secret = %q, want %q", got, want)
	}
}

func TestLoadReportsFilePath(t *testing.T) {
	p := writeTemp(t, "server: {listen: nope}\n")
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), p) {
		t.Errorf("error should mention the config path %q, got: %v", p, err)
	}
}

func TestMetricsPathMustBeAbsolute(t *testing.T) {
	cfg, err := Parse([]byte(minimal()))
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	cfg.Observability.Metrics.Path = "metrics"
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "metrics.path") {
		t.Fatalf("error = %v, want a metrics.path complaint", err)
	}
}
