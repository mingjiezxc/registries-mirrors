// Package httpx builds the outbound HTTP client used to reach upstream
// registries.
//
// Installing the transport into http.DefaultTransport is not optional: the
// embedded distribution proxy issues its upstream requests through
// http.DefaultTransport (registry/proxy/proxyregistry.go), and its
// authentication discovery does a bare http.Get against <remoteurl>/v2/ with no
// timeout. Both behaviours are only controllable through the default transport,
// so rms always installs one.
package httpx

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"golang.org/x/net/http/httpproxy"

	"github.com/watsons/rms/internal/config"
)

// TransportOptions describes the transport rms needs.
type TransportOptions struct {
	// Proxy is an explicit proxy URL. When empty the standard
	// HTTP_PROXY/HTTPS_PROXY environment variables are used.
	Proxy string
	// NoProxy is a comma-separated list of hosts, IPs or CIDRs.
	NoProxy string

	DialTimeout         time.Duration
	TLSHandshakeTimeout time.Duration
	ResponseHeaderTime  time.Duration
	IdleConnTimeout     time.Duration

	InsecureSkipVerify bool
	CAFile             string
}

// OptionsFromConfig adapts the configuration to TransportOptions.
func OptionsFromConfig(c config.HTTPClient) TransportOptions {
	return TransportOptions{
		Proxy:               c.Proxy,
		NoProxy:             c.NoProxy,
		DialTimeout:         c.DialTimeout.Std(),
		TLSHandshakeTimeout: c.TLSHandshakeTimeout.Std(),
		ResponseHeaderTime:  c.ResponseHeaderTime.Std(),
		IdleConnTimeout:     c.IdleConnTimeout.Std(),
		InsecureSkipVerify:  c.InsecureSkipVerify,
		CAFile:              c.CAFile,
	}
}

// NewTransport builds an *http.Transport.
func NewTransport(o TransportOptions) (*http.Transport, error) {
	proxyFunc, err := proxyFunc(o.Proxy, o.NoProxy)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.InsecureSkipVerify {
		tlsConfig.InsecureSkipVerify = true
	}
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA file %s contains no usable certificates", o.CAFile)
		}
		tlsConfig.RootCAs = pool
	}

	dialer := &net.Dialer{Timeout: o.DialTimeout, KeepAlive: 30 * time.Second}

	return &http.Transport{
		Proxy:                 proxyFunc,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       o.IdleConnTimeout,
		TLSHandshakeTimeout:   o.TLSHandshakeTimeout,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: o.ResponseHeaderTime,
		TLSClientConfig:       tlsConfig,
	}, nil
}

// proxyFunc resolves the proxy configuration. An explicit proxy string wins;
// otherwise the standard environment variables apply, matching what operators
// expect from a container.
func proxyFunc(proxy, noProxy string) (func(*http.Request) (*url.URL, error), error) {
	if proxy == "" {
		cfg := httpproxy.FromEnvironment()
		if noProxy != "" {
			cfg.NoProxy = noProxy
		}
		return requestProxyFunc(cfg.ProxyFunc()), nil
	}

	u, err := url.Parse(proxy)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL %q: %w", proxy, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" {
		return nil, fmt.Errorf("invalid proxy URL %q: unsupported scheme %q", proxy, u.Scheme)
	}

	cfg := &httpproxy.Config{
		HTTPProxy:  proxy,
		HTTPSProxy: proxy,
		NoProxy:    noProxy,
	}
	return requestProxyFunc(cfg.ProxyFunc()), nil
}

// requestProxyFunc adapts httpproxy's *url.URL-based function to the
// *http.Request-based signature http.Transport expects.
func requestProxyFunc(fn func(*url.URL) (*url.URL, error)) func(*http.Request) (*url.URL, error) {
	return func(r *http.Request) (*url.URL, error) { return fn(r.URL) }
}

// NewClient builds an *http.Client on a fresh transport.
func NewClient(o TransportOptions) (*http.Client, error) {
	tr, err := NewTransport(o)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: tr}, nil
}

// ClientFromConfig is a convenience wrapper for a config.HTTPClient.
func ClientFromConfig(c config.HTTPClient) (*http.Client, error) {
	return NewClient(OptionsFromConfig(c))
}

// Install replaces http.DefaultTransport, so that the embedded distribution
// proxy (and its unbounded auth discovery request) inherits rms's proxy,
// timeout and TLS settings. It returns the previous transport so callers can
// restore it in tests.
func Install(o TransportOptions) (*http.Transport, error) {
	tr, err := NewTransport(o)
	if err != nil {
		return nil, err
	}
	prev, _ := http.DefaultTransport.(*http.Transport)
	http.DefaultTransport = tr
	return prev, nil
}

// InstallWithClient installs one transport as the process default and returns a
// client using that same transport, so health probes and the registry engine
// share connections, proxy settings and timeouts.
func InstallWithClient(o TransportOptions) (*http.Client, error) {
	tr, err := NewTransport(o)
	if err != nil {
		return nil, err
	}
	http.DefaultTransport = tr
	return &http.Client{Transport: tr}, nil
}

// CheckCAs reports whether the system certificate pool holds any roots.
//
// Every upstream that uses HTTPS needs them, and a slim or distroless base
// image without ca-certificates silently breaks all of them with "x509:
// certificate signed by unknown authority". Failing loudly at startup beats
// discovering it as a wave of degraded namespaces.
func CheckCAs() (int, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		return 0, fmt.Errorf("load system CA pool: %w", err)
	}
	if pool == nil {
		return 0, errors.New("no system CA pool is available")
	}
	// Subjects is deprecated in favour of a pool comparison, but it remains the
	// only way to ask how many roots a pool holds.
	n := len(pool.Subjects()) //nolint:staticcheck
	if n == 0 {
		return 0, errors.New("the system CA pool is empty; install ca-certificates in the image " +
			"(or set upstream.http.ca_file), otherwise every HTTPS upstream will fail with " +
			`"certificate signed by unknown authority"`)
	}
	return n, nil
}

// Describe renders a transport's effective proxy settings for startup logging.
func Describe(o TransportOptions) string {
	pf, err := proxyFunc(o.Proxy, o.NoProxy)
	if err != nil {
		return "proxy: <invalid: " + err.Error() + ">"
	}

	req, _ := http.NewRequest(http.MethodGet, "https://registry-1.docker.io/v2/", nil)
	u, err := pf(req)
	if err != nil {
		return "proxy: <error: " + err.Error() + ">"
	}
	if u == nil {
		return fmt.Sprintf("proxy: none (no_proxy=%q)", o.NoProxy)
	}

	internal, _ := http.NewRequest(http.MethodGet, "http://10.82.64.172:5000/v2/", nil)
	iu, _ := pf(internal)
	if iu == nil {
		return fmt.Sprintf("proxy: %s for public registries, bypassed for internal ones", u.Redacted())
	}
	return fmt.Sprintf("proxy: %s for all registries including internal ones (check no_proxy)", u.Redacted())
}
