// Package config loads and validates the rms configuration file.
//
// The file is YAML with strict decoding: unknown keys are an error, so a typo
// in a namespace or listener never silently disables a feature.
package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Mode describes what a namespace is allowed to do.
type Mode string

const (
	// ModeLocal is a writable namespace: pushes are accepted, content lives
	// only under /data/<namespace>.
	ModeLocal Mode = "local"
	// ModeProxy is a read-only pull-through cache for one or more upstreams.
	ModeProxy Mode = "proxy"
)

// Defaults applied when a field is omitted.
const (
	DefaultListen        = "0.0.0.0:5000"
	DefaultStorageRoot   = "/data"
	DefaultHTTPSecretEnv = "RMS_HTTP_SECRET"
)

// Duration accepts both Go duration strings ("30s", "168h") and bare integers,
// which are interpreted as seconds.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
//
// It accepts a Go duration ("30s", "168h"), a bare number of seconds (0, 90) and
// a numeric string ("0"), because a quoted value that a ${VAR:-0} reference
// expanded into is still a string as far as YAML is concerned.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a scalar, got %s", node.Tag)
	}

	if dur, err := time.ParseDuration(node.Value); err == nil {
		*d = Duration(dur)
		return nil
	}

	if secs, err := strconv.ParseInt(strings.TrimSpace(node.Value), 10, 64); err == nil {
		*d = Duration(time.Duration(secs) * time.Second)
		return nil
	}

	return fmt.Errorf("invalid duration %q: want a Go duration (30s, 168h) or a number of seconds", node.Value)
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) { return d.Std().String(), nil }

// Std converts to a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String implements fmt.Stringer.
func (d Duration) String() string { return d.Std().String() }

// Config is the root of the configuration file.
type Config struct {
	Server        Server        `yaml:"server"`
	Storage       Storage       `yaml:"storage"`
	Auth          Auth          `yaml:"auth"`
	Namespaces    Namespaces    `yaml:"namespaces"`
	Upstream      Upstream      `yaml:"upstream"`
	Observability Observability `yaml:"observability"`

	// path is where the file was loaded from, for error messages.
	path string `yaml:"-"`
}

// Server holds the primary listener and process-level HTTP settings.
type Server struct {
	// Listen is the primary (multi-namespace) listener address.
	Listen string `yaml:"listen"`
	// ExternalURL is the origin clients use to reach the primary listener.
	// It must not contain a path; the registry engine drops it and rms relies
	// on the repository name to carry the namespace.
	ExternalURL string `yaml:"external_url"`
	// HTTPSecret seeds upload state tokens. It must be stable across restarts,
	// otherwise in-flight uploads become invalid. When empty it is read from
	// RMS_HTTP_SECRET, else a random secret is generated (and uploads will not
	// survive a restart).
	HTTPSecret string `yaml:"http_secret"`
	// BehindProxy trusts X-Forwarded-* when deriving the client address.
	BehindProxy bool `yaml:"behind_proxy"`
	// GracefulShutdown bounds how long shutdown waits for in-flight requests.
	GracefulShutdown Duration `yaml:"graceful_shutdown"`
	// ReadTimeout and WriteTimeout are applied to the HTTP servers. The
	// defaults are 0 (no timeout) because layer pushes can be very long.
	ReadTimeout  Duration `yaml:"read_timeout"`
	WriteTimeout Duration `yaml:"write_timeout"`
}

// Storage configures the on-disk cache.
type Storage struct {
	// Root is the parent of the per-namespace roots: <Root>/<namespace>.
	Root string `yaml:"root"`
	// DeleteEnabled allows DELETE of manifests/blobs in local namespaces.
	DeleteEnabled bool `yaml:"delete_enabled"`
	// UploadPurge removes abandoned blob uploads.
	UploadPurge UploadPurge `yaml:"upload_purge"`
}

// UploadPurge maps onto distribution's maintenance.uploadpurging section.
type UploadPurge struct {
	Enabled  bool     `yaml:"enabled"`
	Age      Duration `yaml:"age"`
	Interval Duration `yaml:"interval"`
}

// Auth configures client authentication and push authorisation.
type Auth struct {
	// Type is "htpasswd" or "none".
	Type string `yaml:"type"`
	// Htpasswd is the bcrypt htpasswd file path (Type == "htpasswd").
	Htpasswd string `yaml:"htpasswd"`
	// Realm is the HTTP Basic realm advertised in WWW-Authenticate.
	Realm string `yaml:"realm"`
	// UserNamespace makes every htpasswd user the owner of a namespace named
	// after them: <user>/<repo> -> /data/<user>.
	UserNamespace bool `yaml:"user_namespace"`
	// SharedNamespaces are extra writable namespaces not tied to a single user.
	SharedNamespaces []string `yaml:"shared_namespaces"`
	// AnonymousPullProxy allows unauthenticated pulls from proxy namespaces.
	AnonymousPullProxy bool `yaml:"anonymous_pull_proxy"`
	// AnonymousPullLocal allows unauthenticated pulls from local namespaces.
	AnonymousPullLocal bool `yaml:"anonymous_pull_local"`
	// AdminUsers may list the merged catalog and delete content anywhere.
	AdminUsers []string `yaml:"admin_users"`
}

// Namespaces is the namespace table.
type Namespaces struct {
	// Local lists additional writable namespaces. It is merged with
	// auth.shared_namespaces and, when auth.user_namespace is set, the
	// htpasswd users.
	Local []string `yaml:"local"`
	// Proxy maps an upstream registry domain to its cache settings.
	Proxy map[string]ProxyNamespace `yaml:"proxy"`
	// Selection bounds how an unqualified request picks the proxy namespace
	// that serves it. There is no default namespace: every unqualified request
	// is decided by probing the proxy namespaces.
	Selection Selection `yaml:"selection"`
}

// ProxyNamespace configures a read-only pull-through cache namespace.
type ProxyNamespace struct {
	// Upstreams are the namespace's upstream endpoints, in **authority order**.
	//
	// With racing on (the default) all of them are probed concurrently for the
	// repository being served and the fastest one wins; the order is what
	// decides which upstream defines the expected content digest of a tag, so
	// the first upstream that answers is the one whose bytes a mismatching
	// mirror is measured against. With racing off the historical semantics
	// apply: the first healthy upstream in this order is elected and used until
	// it fails.
	Upstreams []string `yaml:"upstreams"`
	// OfficialLibrary mirrors Docker Hub's rule that a single-segment
	// repository means library/<name>.
	OfficialLibrary bool `yaml:"official_library"`
	// Username and Password authenticate to the upstream (optional).
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// TTL expires cached content. 0 (the default) never expires anything,
	// which also disables distribution's TTL scheduler. See README.
	TTL Duration `yaml:"ttl"`
	// HTTP overrides upstream transport settings for this namespace.
	HTTP *HTTPClient `yaml:"http"`
	// UpstreamRace turns racing on or off for this namespace. Nil means on:
	// every upstream is probed concurrently and the fastest one serves, with
	// equal-digest upstreams sharing the load. Setting it to false restores the
	// ordered election (first healthy upstream wins, failover rebuilds the
	// engine), which is kept for deployments that want a strict preference
	// order.
	UpstreamRace *bool `yaml:"upstream_race"`
	// Balance overrides namespaces.selection.balance for this namespace.
	// Empty inherits the global default. It is only consulted when racing is
	// on.
	Balance string `yaml:"balance"`
}

// RaceEnabled reports whether this namespace probes its upstreams concurrently
// and serves from the fastest one. Racing is on unless it was switched off
// explicitly.
func (ns ProxyNamespace) RaceEnabled() bool { return ns.UpstreamRace == nil || *ns.UpstreamRace }

// Upstream holds defaults for talking to upstream registries.
type Upstream struct {
	Health UpstreamHealth `yaml:"health"`
	HTTP   HTTPClient     `yaml:"http"`
}

// UpstreamHealth configures endpoint election.
type UpstreamHealth struct {
	Enabled bool `yaml:"enabled"`
	// Interval is the probe period.
	Interval Duration `yaml:"interval"`
	// Timeout bounds a single probe.
	Timeout Duration `yaml:"timeout"`
	// FailThreshold consecutive failures before an endpoint is evicted.
	FailThreshold int `yaml:"fail_threshold"`
	// SuccessThreshold consecutive successes before an endpoint is eligible.
	SuccessThreshold int `yaml:"success_threshold"`
	// Path is the probe path; /v2/ is the registry API version endpoint.
	Path string `yaml:"path"`
}

// HTTPClient configures outbound HTTP. An empty Proxy falls back to the
// standard HTTP_PROXY / HTTPS_PROXY / NO_PROXY environment variables.
type HTTPClient struct {
	Proxy string `yaml:"proxy"`
	// NoProxy is a comma-separated list of hosts/CIDRs/IPs. Go's httpproxy
	// understands CIDR notation.
	NoProxy             string   `yaml:"no_proxy"`
	DialTimeout         Duration `yaml:"dial_timeout"`
	ResponseHeaderTime  Duration `yaml:"response_header_timeout"`
	TLSHandshakeTimeout Duration `yaml:"tls_handshake_timeout"`
	IdleConnTimeout     Duration `yaml:"idle_conn_timeout"`
	InsecureSkipVerify  bool     `yaml:"insecure_skip_verify"`
	CAFile              string   `yaml:"ca_file"`
}

// Observability configures logs, traces and metrics.
type Observability struct {
	Logs    Logs    `yaml:"logs"`
	Traces  Traces  `yaml:"traces"`
	Metrics Metrics `yaml:"metrics"`
	// Pprof exposes /debug/pprof on the metrics listener.
	Pprof bool `yaml:"pprof"`
}

// Logs configures logging.
type Logs struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
	// OTLP also ships logs to the configured OTLP endpoint.
	OTLP bool `yaml:"otlp"`
}

// Traces configures OTLP trace export (SigNoz by default).
type Traces struct {
	Enabled     bool     `yaml:"enabled"`
	Endpoint    string   `yaml:"endpoint"`
	Insecure    bool     `yaml:"insecure"`
	SampleRatio float64  `yaml:"sample_ratio"`
	Headers     []string `yaml:"headers"`
}

// Metrics configures the Prometheus endpoint.
type Metrics struct {
	Listen string `yaml:"listen"`
	Path   string `yaml:"path"`
}

// Load reads, decodes and validates the configuration at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.path = path
	return cfg, nil
}

// Parse decodes and validates configuration bytes.
//
// Environment references are expanded in scalar *values* only. Expanding the
// raw text would also rewrite comments, so a file that merely documents
// "${VAR}" in a comment would fail to start.
func Parse(data []byte) (*Config, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if err := expandEnvNode(&doc); err != nil {
		return nil, err
	}

	// Re-emit the expanded document and decode it strictly: yaml.Node.Decode
	// does not honour KnownFields, and rejecting unknown keys is how a typo in a
	// namespace or listener is caught instead of silently disabling it.
	expanded, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	dec := yaml.NewDecoder(strings.NewReader(string(expanded)))
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	var extra any
	switch err := dec.Decode(&extra); {
	case err == nil:
		return nil, errors.New("decode config: more than one YAML document")
	case !errors.Is(err, io.EOF):
		return nil, fmt.Errorf("decode config: %w", err)
	}

	cfg.setDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// expandEnvNode substitutes environment references in string values. Keys are
// never expanded, so an environment variable cannot rename a configuration key.
func expandEnvNode(n *yaml.Node) error {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			if err := expandEnvNode(c); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i, c := range n.Content {
			// Content alternates key, value: only values are expanded.
			if i%2 == 1 {
				if err := expandEnvNode(c); err != nil {
					return err
				}
			}
		}
	case yaml.ScalarNode:
		if n.Tag != "!!str" {
			return nil
		}
		out, err := ExpandEnv(n.Value)
		if err != nil {
			return err
		}
		n.Value = out
	}
	return nil
}

func (c *Config) setDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = DefaultListen
	}
	if c.Storage.Root == "" {
		c.Storage.Root = DefaultStorageRoot
	}
	if !c.Storage.UploadPurge.Enabled && c.Storage.UploadPurge.Age == 0 && c.Storage.UploadPurge.Interval == 0 {
		c.Storage.UploadPurge.Enabled = true
	}
	if c.Storage.UploadPurge.Age == 0 {
		c.Storage.UploadPurge.Age = Duration(168 * time.Hour)
	}
	if c.Storage.UploadPurge.Interval == 0 {
		c.Storage.UploadPurge.Interval = Duration(24 * time.Hour)
	}

	if c.Auth.Type == "" {
		c.Auth.Type = "htpasswd"
	}
	if c.Auth.Realm == "" {
		c.Auth.Realm = "rms"
	}

	c.Namespaces.Selection = c.Namespaces.Selection.withDefaults()

	if c.Upstream.Health.Interval == 0 {
		c.Upstream.Health.Interval = Duration(30 * time.Second)
	}
	if c.Upstream.Health.Timeout == 0 {
		// Generous by default: through a shared HTTP proxy, public registries
		// have been measured at 4-5s to answer /v2/.
		c.Upstream.Health.Timeout = Duration(10 * time.Second)
	}
	if c.Upstream.Health.Path == "" {
		c.Upstream.Health.Path = "/v2/"
	}
	if c.Upstream.Health.FailThreshold == 0 {
		c.Upstream.Health.FailThreshold = 2
	}
	if c.Upstream.Health.SuccessThreshold == 0 {
		c.Upstream.Health.SuccessThreshold = 1
	}
	c.Upstream.HTTP = c.Upstream.HTTP.withDefaults()

	for name, ns := range c.Namespaces.Proxy {
		ns.HTTP = ns.HTTP.withDefaultsPtr(c.Upstream.HTTP)
		if ns.Balance == "" {
			ns.Balance = c.Namespaces.Selection.Balance
		}
		c.Namespaces.Proxy[name] = ns
	}

	if c.Observability.Logs.Level == "" {
		c.Observability.Logs.Level = "info"
	}
	if c.Observability.Logs.Format == "" {
		c.Observability.Logs.Format = "json"
	}
	if c.Observability.Traces.Endpoint == "" {
		c.Observability.Traces.Endpoint = "192.168.188.103:4317"
	}
	if c.Observability.Traces.SampleRatio == 0 {
		c.Observability.Traces.SampleRatio = 0.2
	}
	if c.Observability.Metrics.Listen == "" {
		c.Observability.Metrics.Listen = "0.0.0.0:9100"
	}
	if c.Observability.Metrics.Path == "" {
		c.Observability.Metrics.Path = "/metrics"
	}
	if c.Server.GracefulShutdown == 0 {
		c.Server.GracefulShutdown = Duration(20 * time.Second)
	}
	if c.Server.HTTPSecret == "" {
		c.Server.HTTPSecret = os.Getenv(DefaultHTTPSecretEnv)
	}
}

func (h HTTPClient) withDefaults() HTTPClient {
	if h.DialTimeout == 0 {
		h.DialTimeout = Duration(10 * time.Second)
	}
	if h.ResponseHeaderTime == 0 {
		h.ResponseHeaderTime = Duration(60 * time.Second)
	}
	if h.TLSHandshakeTimeout == 0 {
		h.TLSHandshakeTimeout = Duration(10 * time.Second)
	}
	if h.IdleConnTimeout == 0 {
		h.IdleConnTimeout = Duration(90 * time.Second)
	}
	return h
}

func (h *HTTPClient) withDefaultsPtr(base HTTPClient) *HTTPClient {
	merged := base
	if h == nil {
		return &merged
	}
	if h.Proxy != "" {
		merged.Proxy = h.Proxy
	}
	if h.NoProxy != "" {
		merged.NoProxy = h.NoProxy
	}
	if h.DialTimeout != 0 {
		merged.DialTimeout = h.DialTimeout
	}
	if h.ResponseHeaderTime != 0 {
		merged.ResponseHeaderTime = h.ResponseHeaderTime
	}
	if h.TLSHandshakeTimeout != 0 {
		merged.TLSHandshakeTimeout = h.TLSHandshakeTimeout
	}
	if h.IdleConnTimeout != 0 {
		merged.IdleConnTimeout = h.IdleConnTimeout
	}
	if h.InsecureSkipVerify {
		merged.InsecureSkipVerify = true
	}
	if h.CAFile != "" {
		merged.CAFile = h.CAFile
	}
	return &merged
}

var (
	nsNameRe   = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	envVarExpr = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)
)

// IsDomainLike reports whether first looks like a registry host rather than a
// path component. It mirrors Docker's own reference normalisation rule: the
// first component is a registry when it contains "." or ":", or is "localhost".
func IsDomainLike(first string) bool {
	return first == "localhost" || strings.ContainsAny(first, ".:")
}

// Validate checks the configuration for internal consistency.
func (c *Config) Validate() error {
	var errs []error

	if err := validateListen(c.Server.Listen); err != nil {
		errs = append(errs, fmt.Errorf("server.listen: %w", err))
	}
	if c.Server.ExternalURL != "" {
		u, err := url.Parse(c.Server.ExternalURL)
		if err != nil {
			errs = append(errs, fmt.Errorf("server.external_url: %w", err))
		} else {
			if u.Scheme != "http" && u.Scheme != "https" {
				errs = append(errs, fmt.Errorf("server.external_url: scheme must be http or https, got %q", u.Scheme))
			}
			if u.Host == "" {
				errs = append(errs, errors.New("server.external_url: missing host"))
			}
			if p := strings.Trim(u.Path, "/"); p != "" {
				errs = append(errs, fmt.Errorf("server.external_url: must not contain a path (%q); the registry engine drops it and namespaces are carried by the repository name", u.Path))
			}
		}
	}

	if c.Storage.Root == "" || !strings.HasPrefix(c.Storage.Root, "/") {
		errs = append(errs, fmt.Errorf("storage.root: must be an absolute path, got %q", c.Storage.Root))
	}

	switch c.Auth.Type {
	case "none", "htpasswd":
	default:
		errs = append(errs, fmt.Errorf("auth.type: must be \"none\" or \"htpasswd\", got %q", c.Auth.Type))
	}
	if c.Auth.Type == "htpasswd" {
		if c.Auth.Htpasswd == "" {
			errs = append(errs, errors.New("auth.htpasswd: required when auth.type is \"htpasswd\""))
		} else if _, err := os.Stat(c.Auth.Htpasswd); err != nil {
			errs = append(errs, fmt.Errorf("auth.htpasswd: %w", err))
		}
	}
	if c.Auth.UserNamespace && c.Auth.Type != "htpasswd" {
		errs = append(errs, errors.New("auth.user_namespace: requires auth.type \"htpasswd\""))
	}

	if len(c.Namespaces.Proxy) == 0 {
		errs = append(errs, errors.New("namespaces.proxy: at least one proxy namespace is required"))
	}

	if !ValidBalance(c.Namespaces.Selection.Balance) {
		errs = append(errs, fmt.Errorf("namespaces.selection.balance: must be %q, %q or %q, got %q",
			BalanceWeighted, BalanceRoundRobin, BalanceOff, c.Namespaces.Selection.Balance))
	}
	if c.Namespaces.Selection.BalanceWindow < 1 {
		errs = append(errs, fmt.Errorf("namespaces.selection.balance_window: must be >= 1, got %v",
			c.Namespaces.Selection.BalanceWindow))
	}
	if c.Namespaces.Selection.DecisionDeadline < 0 {
		errs = append(errs, fmt.Errorf("namespaces.selection.decision_deadline: must not be negative, got %s",
			c.Namespaces.Selection.DecisionDeadline))
	}
	if d, p := c.Namespaces.Selection.DecisionDeadline, c.Namespaces.Selection.ProbeTimeout; d > 0 && p > 0 && d > p {
		errs = append(errs, fmt.Errorf(
			"namespaces.selection.decision_deadline (%s) must not exceed probe_timeout (%s): a probe cannot be given more time than the decision that bounds it", d, p))
	}

	for _, name := range sortedKeys(c.Namespaces.Proxy) {
		ns := c.Namespaces.Proxy[name]
		if !IsDomainLike(name) {
			errs = append(errs, fmt.Errorf("namespaces.proxy[%s]: key must look like a registry host (contain \".\" or \":\", or be \"localhost\")", name))
		}
		if len(ns.Upstreams) == 0 {
			errs = append(errs, fmt.Errorf("namespaces.proxy[%s].upstreams: at least one upstream is required", name))
		}
		for i, up := range ns.Upstreams {
			if err := validateUpstreamURL(up); err != nil {
				errs = append(errs, fmt.Errorf("namespaces.proxy[%s].upstreams[%d]: %w", name, i, err))
			}
		}
		if ns.TTL < 0 {
			errs = append(errs, fmt.Errorf("namespaces.proxy[%s].ttl: must not be negative", name))
		}
		if ns.Balance != "" && !ValidBalance(ns.Balance) {
			errs = append(errs, fmt.Errorf("namespaces.proxy[%s].balance: must be %q, %q or %q, got %q",
				name, BalanceWeighted, BalanceRoundRobin, BalanceOff, ns.Balance))
		}
	}

	for _, name := range append(append([]string{}, c.Namespaces.Local...), c.Auth.SharedNamespaces...) {
		if err := validateNamespaceName(name); err != nil {
			errs = append(errs, fmt.Errorf("namespaces.local: %w", err))
		}
		// No local/proxy collision check is needed: proxy keys must be
		// domain-like and local names must not be, so the two sets are
		// disjoint by construction.
	}

	if c.Observability.Metrics.Path != "" && !strings.HasPrefix(c.Observability.Metrics.Path, "/") {
		errs = append(errs, fmt.Errorf("observability.metrics.path: must start with %q, got %q", "/", c.Observability.Metrics.Path))
	}
	if c.Observability.Metrics.Listen != "" {
		if err := validateListen(c.Observability.Metrics.Listen); err != nil {
			errs = append(errs, fmt.Errorf("observability.metrics.listen: %w", err))
		}
		if c.Observability.Metrics.Listen == c.Server.Listen {
			errs = append(errs, errors.New("observability.metrics.listen: must differ from server.listen"))
		}
	}
	if c.Observability.Traces.Enabled {
		if _, _, err := net.SplitHostPort(c.Observability.Traces.Endpoint); err != nil {
			errs = append(errs, fmt.Errorf("observability.traces.endpoint: %w", err))
		}
	}
	switch c.Observability.Logs.Format {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("observability.logs.format: must be \"json\" or \"text\", got %q", c.Observability.Logs.Format))
	}

	return errors.Join(errs...)
}

func validateListen(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid address %q: %w", addr, err)
	}
	if port == "" {
		return fmt.Errorf("invalid address %q: missing port", addr)
	}
	if host != "" {
		if ip := net.ParseIP(host); ip == nil {
			return fmt.Errorf("invalid address %q: host must be an IP or empty", addr)
		}
	}
	return nil
}

func validateUpstreamURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid URL %q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid URL %q: missing host", raw)
	}
	if p := strings.Trim(u.Path, "/"); p != "" {
		return fmt.Errorf("invalid URL %q: must not contain a path; the namespace already supplies it", raw)
	}
	return nil
}

func validateNamespaceName(name string) error {
	if name == "" {
		return errors.New("namespace name must not be empty")
	}
	if IsDomainLike(name) {
		return fmt.Errorf("namespace %q must not look like a registry host (no \".\" or \":\")", name)
	}
	if !nsNameRe.MatchString(name) {
		return fmt.Errorf("namespace %q is invalid: use lowercase letters, digits and . _ - separators", name)
	}
	return nil
}

// IsLocal reports whether name is a writable namespace declared in the file.
// htpasswd-derived namespaces are added by the caller.
func (c *Config) IsLocal(name string) bool {
	for _, n := range c.Namespaces.Local {
		if n == name {
			return true
		}
	}
	for _, n := range c.Auth.SharedNamespaces {
		if n == name {
			return true
		}
	}
	return false
}

// NamespaceRoot returns the storage root for a namespace.
func (c *Config) NamespaceRoot(namespace string) string {
	return path.Join(c.Storage.Root, namespace)
}

// HTTPDefaults returns the resolved upstream HTTP settings.
func (c *Config) HTTPDefaults() HTTPClient { return c.Upstream.HTTP }

// HTTPFor returns the effective HTTP settings for a proxy namespace.
func (c *Config) HTTPFor(namespace string) HTTPClient {
	ns, ok := c.Namespaces.Proxy[namespace]
	if !ok || ns.HTTP == nil {
		return c.Upstream.HTTP
	}
	return *ns.HTTP
}

// BalanceFor returns the effective load-balancing mode for a proxy namespace:
// the namespace's own `balance` when set, otherwise the global default.
func (c *Config) BalanceFor(namespace string) string {
	ns, ok := c.Namespaces.Proxy[namespace]
	if !ok || ns.Balance == "" {
		return c.Namespaces.Selection.Balance
	}
	return ns.Balance
}

// RaceEnabledFor reports whether a proxy namespace probes its upstreams
// concurrently. It is false for a namespace that is not configured as a proxy
// namespace at all.
func (c *Config) RaceEnabledFor(namespace string) bool {
	ns, ok := c.Namespaces.Proxy[namespace]
	if !ok {
		return false
	}
	return ns.RaceEnabled()
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ExpandEnv substitutes ${VAR} and ${VAR:-default} in raw. A reference to an
// unset variable without a default is an error, so a missing secret fails at
// startup instead of silently becoming an empty string.
func ExpandEnv(raw string) (string, error) {
	var missing []string

	out := envVarExpr.ReplaceAllStringFunc(raw, func(match string) string {
		groups := envVarExpr.FindStringSubmatch(match)
		name, def, hasDef := groups[1], groups[3], groups[2] != ""
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		if hasDef {
			return def
		}
		missing = append(missing, name)
		return match
	})

	if len(missing) > 0 {
		sort.Strings(missing)
		return "", fmt.Errorf("undefined environment variable(s): %s", strings.Join(dedupe(missing), ", "))
	}
	return out, nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
