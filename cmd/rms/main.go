// Command rms runs a caching registry mirror: one HTTP endpoint that proxies and
// caches upstream registries by namespace and accepts pushes into local
// namespaces.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/watsons/rms/internal/app"
	"github.com/watsons/rms/internal/auth"
	"github.com/watsons/rms/internal/config"
	"github.com/watsons/rms/internal/obs"
	"github.com/watsons/rms/internal/server"
)

// Build metadata, injected with -ldflags.
var (
	version = "dev"
	commit  = "none"
)

const defaultConfigPath = "/etc/rms/rms.yaml"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(os.Stderr, "rms: %v\n", err)
		}
		os.Exit(exitCode(err))
	}
}

func exitCode(err error) int {
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 1
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		usage(stderr)
		return flag.ErrHelp
	}

	switch args[0] {
	case "serve":
		return cmdServe(args[1:], stdout, stderr)
	case "version":
		fmt.Fprintf(stdout, "rms %s (commit %s)\n", version, commit)
		return nil
	case "healthcheck":
		return cmdHealthcheck(args[1:], stdout)
	case "htpasswd":
		return cmdHtpasswd(args[1:], stdout, stderr)
	case "config":
		return cmdConfig(args[1:], stdout)
	case "help", "-h", "--help":
		usage(stdout)
		return nil
	default:
		usage(stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `rms - caching registry mirror

usage:
  rms serve       [--config path]         run the mirror
  rms healthcheck [--addr url]            exit non-zero unless the API answers
  rms htpasswd add --file path --user u --password p
                                         add or replace a user (bcrypt)
  rms config validate [--config path]     check the configuration and exit
  rms version                             print the version

Environment:
  `+config.DefaultHTTPSecretEnv+`   upload-state secret, used when server.http_secret is unset
  HTTP_PROXY / HTTPS_PROXY / NO_PROXY    outbound proxy for upstream registries
`)
}

// --- serve -------------------------------------------------------------------

func cmdServe(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath, "path to the configuration file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	logger, err := obs.NewLogger(cfg.Observability.Logs, stderr)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	obs, err := setupObservability(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := obs.tracing.Shutdown(shutdownCtx); err != nil {
			logger.Error("trace shutdown failed", "error", err)
		}
	}()

	// SIGHUP reloads the htpasswd file, so adding a user does not need a restart.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	srv, err := app.Build(ctx, cfg, logger, obs.server)
	if err != nil {
		return err
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				if err := srv.Auth().Reload(); err != nil {
					logger.Error("htpasswd reload failed", "error", err)
					continue
				}
				logger.Info("htpasswd reloaded", "users", len(srv.Auth().Usernames()))
			}
		}
	}()

	if err := srv.Listen(); err != nil {
		return err
	}

	logger.Info("rms ready",
		"version", version,
		"commit", commit,
		"listeners", strings.Join(srv.Addrs(), ","),
		"config", *configPath,
	)
	fmt.Fprintf(stdout, "rms listening on %s\n", strings.Join(srv.Addrs(), ","))

	if err := srv.Serve(ctx); err != nil {
		return err
	}

	logger.Info("rms stopped")
	return nil
}

// --- healthcheck -------------------------------------------------------------

func cmdHealthcheck(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addr := fs.String("addr", "http://127.0.0.1:5000/v2/", "registry API URL to probe")
	timeout := fs.Duration("timeout", 5*time.Second, "probe timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	client := &http.Client{Timeout: *timeout}
	resp, err := client.Get(*addr)
	if err != nil {
		return fmt.Errorf("healthcheck %s: %w", *addr, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	// The registry API root answers 200 when open and 401 when it wants
	// credentials; both mean the service is alive.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("healthcheck %s: unexpected status %d", *addr, resp.StatusCode)
	}

	fmt.Fprintln(stdout, "ok")
	return nil
}

// --- htpasswd ----------------------------------------------------------------

func cmdHtpasswd(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(`usage: rms htpasswd add --file path --user u --password p`)
	}

	switch args[0] {
	case "add":
		return cmdHtpasswdAdd(args[1:], stdout, stderr)
	case "list":
		return cmdHtpasswdList(args[1:], stdout)
	default:
		return fmt.Errorf("unknown htpasswd subcommand %q", args[0])
	}
}

func cmdHtpasswdAdd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("htpasswd add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("file", "", "htpasswd file to create or update")
	user := fs.String("user", "", "username")
	password := fs.String("password", "", "password (read from stdin when empty)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *file == "" || *user == "" {
		return errors.New("both --file and --user are required")
	}
	if strings.ContainsAny(*user, ":/ \t") {
		return fmt.Errorf("invalid username %q", *user)
	}

	pass := *password
	if pass == "" {
		fmt.Fprint(stderr, "password: ")
		line, err := readLine(os.Stdin)
		if err != nil {
			return err
		}
		pass = line
		if pass == "" {
			return errors.New("password must not be empty")
		}
	}

	hash, err := auth.HashPassword(pass)
	if err != nil {
		return err
	}

	if err := upsertHtpasswd(*file, *user, hash); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "%s: updated %s\n", *file, *user)
	return nil
}

func cmdHtpasswdList(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("htpasswd list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("file", "", "htpasswd file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return errors.New("--file is required")
	}

	users, err := auth.Usernames(*file)
	if err != nil {
		return err
	}
	sort.Strings(users)
	for _, u := range users {
		fmt.Fprintln(stdout, u)
	}
	return nil
}

// upsertHtpasswd replaces the user's entry, preserving other entries and
// comments, and writes the file atomically with owner-only permissions.
func upsertHtpasswd(path, user, hash string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	prefix := user + ":"
	lines := make([]string, 0, 8)
	for _, line := range strings.Split(strings.TrimRight(string(existing), "\n"), "\n") {
		if line == "" && len(lines) == 0 {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			continue
		}
		lines = append(lines, line)
	}
	lines = append(lines, prefix+hash)

	body := strings.Join(lines, "\n") + "\n"

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".htpasswd-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.WriteString(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

func readLine(r io.Reader) (string, error) {
	var sb strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				break
			}
			sb.WriteByte(buf[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", err
		}
	}
	return strings.TrimRight(sb.String(), "\r"), nil
}

// --- config ------------------------------------------------------------------

func cmdConfig(args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "validate" {
		return errors.New("usage: rms config validate [--config path]")
	}

	fs := flag.NewFlagSet("config validate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", defaultConfigPath, "path to the configuration file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "%s: ok\n", *configPath)
	fmt.Fprintf(stdout, "  primary listener: %s\n", cfg.Server.Listen)
	for _, l := range cfg.EffectiveListeners() {
		ns := l.Namespace
		if ns == "" {
			ns = "<multi-namespace>"
		}
		fmt.Fprintf(stdout, "  listener %-20s namespace=%s external_url=%s\n", l.Listen, ns, orDash(l.ExternalURL))
	}
	fmt.Fprintf(stdout, "  storage root: %s\n", cfg.Storage.Root)
	fmt.Fprintf(stdout, "  default namespace: %s\n", cfg.Namespaces.Default)
	for _, name := range sortedKeys(cfg.Namespaces.Proxy) {
		fmt.Fprintf(stdout, "  proxy %-16s upstreams=%s\n",
			name, strings.Join(cfg.Namespaces.Proxy[name].Upstreams, ","))
	}
	return nil
}

func sortedKeys(m map[string]config.ProxyNamespace) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// observability bundles the optional telemetry components so shutdown can flush
// them in one place.
type observability struct {
	server  server.Observability
	tracing *obs.Tracing
}

// setupObservability builds metrics and traces and assembles the server hook.
//
// Both default to the SigNoz instance in this environment: metrics are exposed
// for scraping on the configured port, traces are exported over OTLP to
// 192.168.188.103:4317.
func setupObservability(ctx context.Context, cfg *config.Config, logger *slog.Logger) (observability, error) {
	out := observability{}

	metrics, err := obs.NewMetrics(cfg.Observability.Metrics.Path)
	if err != nil {
		return out, err
	}
	metrics.SetBuildInfo(version, commit)
	out.server.Metrics = metrics.Handler()
	out.server.Recorder = metrics

	if cfg.Observability.Pprof {
		mux := http.NewServeMux()
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		out.server.Pprof = mux
	}

	tracing, err := obs.InitTracing(ctx, cfg.Observability.Traces, version)
	if err != nil {
		return out, err
	}
	out.tracing = tracing
	if tracing.Enabled() {
		logger.Info("trace export enabled",
			"endpoint", cfg.Observability.Traces.Endpoint,
			"insecure", cfg.Observability.Traces.Insecure,
			"sample_ratio", cfg.Observability.Traces.SampleRatio,
			"service_name", obs.ServiceName,
		)
		out.server.Middleware = tracing.Middleware
	}

	logger.Info("metrics enabled",
		"listen", cfg.Observability.Metrics.Listen,
		"path", cfg.Observability.Metrics.Path,
	)
	return out, nil
}
