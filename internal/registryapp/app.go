// Package registryapp builds one distribution registry engine per namespace.
//
// Each namespace gets its own storage root (<storage.root>/<namespace>), its own
// upload purger and, for proxy namespaces, its own pull-through cache. That
// isolation is what makes "cache per repository point" work: content is stored
// under the namespace it came from, and a namespace can be measured or removed
// on its own.
package registryapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/distribution/distribution/v3/configuration"
	"github.com/distribution/distribution/v3/registry/handlers"

	// Storage drivers self-register with the driver factory from package
	// init(); without this import handlers.NewApp panics with
	// "StorageDriver not registered: filesystem".
	_ "github.com/distribution/distribution/v3/registry/storage/driver/filesystem"

	"github.com/watsons/rms/internal/config"
)

// Spec describes the engine to build.
type Spec struct {
	// Namespace this engine serves.
	Namespace string
	// Mode is local (writable) or proxy (read-only pull-through cache).
	Mode config.Mode
	// Root is the storage root, normally <storage.root>/<namespace>.
	Root string
	// ExternalURL is the client-visible origin for this namespace, used to
	// build absolute upload URLs. Empty means "derive from the request", which
	// is correct when rms is reached directly.
	ExternalURL string
	// RemoteURL is the upstream base URL; required for proxy namespaces.
	RemoteURL string
	// UpstreamUsername and UpstreamPassword authenticate to the upstream.
	UpstreamUsername string
	UpstreamPassword string
	// TTL expires cached content. Zero disables expiry entirely, which also
	// avoids installing distribution's TTL scheduler.
	TTL time.Duration
	// DeleteEnabled allows DELETE of manifests and blobs (local namespaces).
	DeleteEnabled bool
	// ReadOnly rejects writes. It is used for the cache-only engine that serves
	// an already-populated cache while the upstream is unreachable.
	ReadOnly bool
	// HTTPSecret seeds upload state tokens. Must be stable across restarts.
	HTTPSecret string
	// UploadPurge configures abandoned upload cleanup.
	UploadPurge config.UploadPurge
	// Logger receives construction events.
	Logger *slog.Logger
}

// App is one namespace's registry engine.
type App struct {
	Namespace   string
	Mode        config.Mode
	RemoteURL   string
	ExternalURL string

	engine *handlers.App
}

// ServeHTTP implements http.Handler.
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.engine.ServeHTTP(w, r) }

// Shutdown stops the engine's background workers (upload purger, and the TTL
// scheduler for proxy namespaces).
func (a *App) Shutdown() error { return a.engine.Shutdown() }

// Build constructs a namespace engine.
//
// handlers.NewApp panics rather than returning an error — for example when a
// proxy namespace's upstream does not answer. The panic is converted into an
// error so a startup failure is reported instead of crashing the process.
func Build(spec Spec) (app *App, err error) {
	defer func() {
		if r := recover(); r != nil {
			app = nil
			err = fmt.Errorf("namespace %s: registry engine init failed: %v", spec.Namespace, r)
		}
	}()

	if spec.Namespace == "" {
		return nil, fmt.Errorf("namespace must not be empty")
	}
	if spec.Root == "" {
		return nil, fmt.Errorf("namespace %s: storage root must not be empty", spec.Namespace)
	}
	if spec.Mode == config.ModeProxy && spec.RemoteURL == "" {
		return nil, fmt.Errorf("namespace %s: proxy namespace requires a remote URL", spec.Namespace)
	}

	engineCfg := engineConfiguration(spec)
	return &App{
		Namespace:   spec.Namespace,
		Mode:        spec.Mode,
		RemoteURL:   spec.RemoteURL,
		ExternalURL: spec.ExternalURL,
		engine:      handlers.NewApp(context.Background(), engineCfg),
	}, nil
}

// engineConfiguration translates an rms Spec into distribution's configuration.
func engineConfiguration(spec Spec) *configuration.Configuration {
	params := configuration.Parameters{
		"rootdirectory": spec.Root,
	}

	// The upload purger is always configured explicitly. Left unset,
	// distribution enables it with its own defaults, which would start a purger
	// even for read-only proxy namespaces.
	//
	// The value must be a map[any]any, not a configuration.Parameters: NewApp
	// type-asserts it, and distribution's YAML parser produces map[any]any while
	// a Go-built configuration.Parameters would panic with
	// "uploadpurging config key must contain additional keys".
	purge := map[any]any{
		"enabled":  spec.UploadPurge.Enabled,
		"age":      spec.UploadPurge.Age.Std().String(),
		"interval": spec.UploadPurge.Interval.Std().String(),
		"dryrun":   false,
	}

	storage := configuration.Storage{
		"filesystem": params,
		// Blob reads stream through rms instead of redirecting the client to
		// the storage backend, which would leak internal addresses.
		"redirect":    configuration.Parameters{"disable": true},
		"maintenance": configuration.Parameters{"uploadpurging": purge},
	}
	if spec.ReadOnly {
		// Same map[any]any requirement as uploadpurging above.
		storage["maintenance"] = configuration.Parameters{
			"uploadpurging": purge,
			"readonly":      map[any]any{"enabled": true},
		}
	}
	if spec.DeleteEnabled {
		storage["delete"] = configuration.Parameters{"enabled": true}
	}

	cfg := &configuration.Configuration{
		Log:     configuration.Log{Level: "error"},
		Storage: storage,
		HTTP: configuration.HTTP{
			Host:   spec.ExternalURL,
			Secret: spec.HTTPSecret,
		},
	}

	// Explicit validation: manifests referenced by foreign URLs are rejected,
	// and every child of an image index must already be present. This is the
	// same policy registry:2 applies by default, and the pull-through cache
	// waives the index check when it writes fetched content.
	cfg.Validation.Enabled = true

	if spec.Mode == config.ModeProxy {
		ttl := spec.TTL
		cfg.Proxy = configuration.Proxy{
			RemoteURL: spec.RemoteURL,
			Username:  spec.UpstreamUsername,
			Password:  spec.UpstreamPassword,
			TTL:       &ttl,
		}
	}

	return cfg
}

// DeriveHTTPSecret produces a stable upload-state secret when the operator did
// not configure one.
//
// It is derived rather than random on purpose: a per-start random secret
// silently invalidates every in-progress blob upload across a restart. The
// value is not a security boundary — upload state tokens additionally require a
// valid, authorised request against the same repository.
func DeriveHTTPSecret(storageRoot string) string {
	host, _ := os.Hostname()
	sum := sha256.Sum256([]byte("rms/http-secret\x00" + storageRoot + "\x00" + host))
	return hex.EncodeToString(sum[:16])
}
