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

	logStartupSummary(log, cfg, table, authenticator)

	return server.New(cfg, log, table, authenticator, mgr, telemetry), nil
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
// A namespace bound to its own listener inherits that listener's origin so that
// generated upload URLs carry the right scheme, host and port. Everything else
// falls back to the primary listener's origin, which may be empty — in that case
// the engine derives the origin from each request, which is correct when rms is
// reached directly.
func externalURLs(cfg *config.Config, table *namespace.Table) map[string]string {
	out := map[string]string{}

	for _, l := range cfg.EffectiveListeners() {
		if l.Namespace == "" {
			continue
		}
		if _, dup := out[l.Namespace]; dup {
			continue
		}
		out[l.Namespace] = l.ExternalURL
	}

	names := make([]string, 0, len(cfg.Namespaces.Proxy)+len(table.LocalNames()))
	for name := range cfg.Namespaces.Proxy {
		names = append(names, name)
	}
	names = append(names, table.LocalNames()...)

	for _, name := range names {
		if _, ok := out[name]; !ok {
			out[name] = cfg.Server.ExternalURL
		}
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

func logStartupSummary(log *slog.Logger, cfg *config.Config, table *namespace.Table, a *auth.Authenticator) {
	local := table.LocalNames()
	proxy := make([]string, 0, len(cfg.Namespaces.Proxy))
	for name := range cfg.Namespaces.Proxy {
		proxy = append(proxy, name)
	}
	sort.Strings(proxy)

	log.Info("namespaces configured",
		"local", strings.Join(local, ","),
		"proxy", strings.Join(proxy, ","),
		"default", table.Default(),
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
