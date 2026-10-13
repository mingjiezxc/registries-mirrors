// Package app assembles the service from configuration.
//
// The order matters: the outbound transport is installed first, because the
// registry engine performs an HTTP request against the upstream while it is
// being constructed, and that request must already honour the configured proxy
// and timeouts.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/watsons/rms/internal/auth"
	"github.com/watsons/rms/internal/config"
	"github.com/watsons/rms/internal/httpx"
	"github.com/watsons/rms/internal/namespace"
	"github.com/watsons/rms/internal/obs"
	"github.com/watsons/rms/internal/registry"
	"github.com/watsons/rms/internal/registryapp"
	"github.com/watsons/rms/internal/selection"
	"github.com/watsons/rms/internal/server"
)

// Build wires every component together and starts the namespace engines.
//
// The returned Server has its listeners bound but is not serving yet; call
// Serve.
func Build(ctx context.Context, cfg *config.Config, log *slog.Logger, telemetry server.Observability) (*server.Server, error) {
	if log == nil {
		log = slog.Default()
	}

	// The embedded registry logs through logrus, whose level and formatter are
	// process globals. They must be configured wherever the app is assembled,
	// not only in the CLI, otherwise an embedding caller gets the engine's
	// default info-level chatter.
	if err := obs.ConfigureLogrus(cfg.Observability.Logs, nil); err != nil {
		return nil, err
	}

	client, err := httpx.InstallWithClient(httpx.OptionsFromConfig(cfg.Upstream.HTTP))
	if err != nil {
		return nil, fmt.Errorf("configure outbound transport: %w", err)
	}
	roots, caErr := httpx.CheckCAs()
	if caErr != nil {
		// Not fatal: a deployment that only chains to plain-HTTP internal
		// mirrors does not need a CA pool. It is always worth shouting about,
		// because every HTTPS upstream is about to fail.
		log.Warn("no usable CA certificates; HTTPS upstreams will fail", "error", caErr)
	}
	log.Info("outbound transport configured",
		"detail", httpx.Describe(httpx.OptionsFromConfig(cfg.Upstream.HTTP)),
		"ca_roots", roots,
	)

	users, err := personalNamespaces(cfg, log)
	if err != nil {
		return nil, err
	}

	table := namespace.NewTable(cfg, users)

	authenticator, err := auth.New(auth.Options{
		Realm:              cfg.Auth.Realm,
		HtpasswdFile:       htpasswdPath(cfg),
		Users:              users,
		SharedNamespaces:   append(append([]string{}, cfg.Namespaces.Local...), cfg.Auth.SharedNamespaces...),
		Admins:             toSet(cfg.Auth.AdminUsers),
		AnonymousPullProxy: cfg.Auth.AnonymousPullProxy,
		AnonymousPullLocal: cfg.Auth.AnonymousPullLocal,
	})
	if err != nil {
		return nil, fmt.Errorf("configure authentication: %w", err)
	}

	secret := cfg.Server.HTTPSecret
	if secret == "" {
		secret = registryapp.DeriveHTTPSecret(cfg.Storage.Root)
		log.Warn("no http_secret configured; a value derived from the storage root and hostname is used so uploads survive a restart. " +
			"Set server.http_secret (or " + config.DefaultHTTPSecretEnv + ") to control it explicitly.")
	}

	mgr := registry.New(cfg, table, log, client, secret, externalURLs(cfg, table), recorderOf(telemetry))
	if err := mgr.Start(ctx); err != nil {
		// Local namespaces that failed are fatal; report before serving.
		return nil, fmt.Errorf("start namespaces: %w", err)
	}

	// The selector probes every upstream of every proxy namespace with the same
	// outbound client the engines use, so a probe honours the configured proxy,
	// timeouts and TLS settings and shares its connection pool.
	chooser := selection.New(mgr, selection.Options{
		Client:           client,
		TTL:              cfg.Namespaces.Selection.ProbeTTL.Std(),
		Timeout:          cfg.Namespaces.Selection.ProbeTimeout.Std(),
		DecisionDeadline: cfg.Namespaces.Selection.DecisionDeadline.Std(),
		Balance:          cfg.BalanceFor,
		BalanceWindow:    cfg.Namespaces.Selection.BalanceWindow,
		Observer:         telemetry.Recorder,
		Logger:           log,
	})

	logAuthPosture(log, cfg)

	logStartupSummary(log, cfg, table, authenticator)

	return server.New(cfg, log, table, authenticator, mgr, chooser, telemetry), nil
}

// personalNamespaces returns the htpasswd users that can own a namespace.
//
// A username is only usable as a namespace when it is not domain-like: the
// router treats a first path element containing "." or ":" as a registry host,
// so a user named "jane.doe" would be shadowed by the default-namespace rule.
// Such users are reported and skipped rather than silently broken.
func personalNamespaces(cfg *config.Config, log *slog.Logger) ([]string, error) {
	if cfg.Auth.Type != "htpasswd" || !cfg.Auth.UserNamespace {
		return nil, nil
	}

	names, err := auth.Usernames(cfg.Auth.Htpasswd)
	if err != nil {
		return nil, fmt.Errorf("read htpasswd users: %w", err)
	}

	out := make([]string, 0, len(names))
	for _, name := range names {
		switch {
		case config.IsDomainLike(name):
			log.Warn("htpasswd user cannot own a namespace because the name looks like a registry host",
				"user", name, "hint", "rename the user or add a shared namespace")
		case strings.ContainsAny(name, "/ "):
			log.Warn("htpasswd user cannot own a namespace because the name contains an unusable character",
				"user", name)
		default:
			out = append(out, name)
		}
	}

	sort.Strings(out)
	return out, nil
}

func htpasswdPath(cfg *config.Config) string {
	if cfg.Auth.Type != "htpasswd" {
		return ""
	}
	return cfg.Auth.Htpasswd
}

// externalURLs maps every namespace to the origin clients use for it.
//
// There is a single listener, so every namespace is reached through it and
// inherits server.external_url. It may be empty, in which case the engine
// derives the origin from each request, which is correct when rms is reached
// directly.
func externalURLs(cfg *config.Config, table *namespace.Table) map[string]string {
	out := map[string]string{}

	names := make([]string, 0, len(cfg.Namespaces.Proxy)+len(table.LocalNames()))
	names = append(names, table.ProxyNames()...)
	names = append(names, table.LocalNames()...)

	for _, name := range names {
		out[name] = cfg.Server.ExternalURL
	}
	return out
}

func recorderOf(obs server.Observability) registry.Recorder {
	if obs.Recorder == nil {
		return nil
	}
	return obs.Recorder
}

func toSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		if n != "" {
			out[n] = true
		}
	}
	return out
}

// logAuthPosture states the pull/push policy at startup.
//
// The failure modes it exists for are silent. An endpoint with authentication
// switched off accepts pushes from anyone who can reach it. An endpoint that
// challenges anonymous content reads is invisible to a Docker daemon's
// registry-mirrors list: the daemon's mirror requests carry no credentials for
// the mirror host, so it abandons the mirror and falls back to the upstream
// registry — which looks like a working pull while the cache is bypassed
// entirely (measured: reports/mirror-probe-103.md). Neither shows up as an
// error, so both are said out loud once, here.
func logAuthPosture(log *slog.Logger, cfg *config.Config) {
	if cfg.Auth.Type != "htpasswd" {
		log.Warn("authentication is disabled: anyone who can reach this endpoint may push to its local namespaces",
			"auth_type", cfg.Auth.Type)
	}
	if !cfg.Auth.AnonymousPullProxy {
		log.Warn("anonymous pulls of proxy namespaces are disabled: a Docker daemon using this host as a registry mirror will abandon it and pull from the upstream registry instead")
	}
	if cfg.Auth.AnonymousPullLocal {
		log.Warn("anonymous pulls of local namespaces are enabled: every client can read every private namespace")
	}
}

// logStartupSummary reports what was configured, including the knobs that
// changed behaviour in this version: whether upstreams are raced, and how the
// winners' traffic is balanced.
func logStartupSummary(log *slog.Logger, cfg *config.Config, table *namespace.Table, a *auth.Authenticator) {
	local := table.LocalNames()
	proxy := table.ProxyNames()

	var racing, ordered []string
	for _, name := range proxy {
		if cfg.RaceEnabledFor(name) {
			racing = append(racing, name)
			continue
		}
		ordered = append(ordered, name)
	}

	log.Info("namespaces configured",
		"local", strings.Join(local, ","),
		"proxy", strings.Join(proxy, ","),
		"proxy_candidates", len(proxy),
		"selection_probe_ttl", cfg.Namespaces.Selection.ProbeTTL.Std().String(),
		"selection_probe_timeout", cfg.Namespaces.Selection.ProbeTimeout.Std().String(),
		"selection_decision_deadline", cfg.Namespaces.Selection.DecisionDeadline.Std().String(),
		"selection_balance", cfg.Namespaces.Selection.Balance,
		"selection_balance_window", cfg.Namespaces.Selection.BalanceWindow,
		"upstream_race", strings.Join(racing, ","),
		"upstream_ordered", strings.Join(ordered, ","),
		"storage_root", cfg.Storage.Root,
		"auth", authMode(a),
	)
}

func authMode(a *auth.Authenticator) string {
	if !a.Enabled() {
		return "none (open registry)"
	}
	return "htpasswd"
}

// Serve builds the service and blocks until ctx is cancelled.
func Serve(ctx context.Context, cfg *config.Config, log *slog.Logger, obs server.Observability) error {
	srv, err := Build(ctx, cfg, log, obs)
	if err != nil {
		return err
	}

	if err := srv.Listen(); err != nil {
		return err
	}
	log.Info("rms ready", "listeners", strings.Join(srv.Addrs(), ","), "version", Version)

	if err := srv.Serve(ctx); err != nil {
		return err
	}

	// Shutdown with a fresh budget: Serve already ran the graceful shutdown, so
	// this only needs to close anything still open.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// Version is injected at build time.
var Version = "dev"
