package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/watsons/rms/internal/auth"
	"github.com/watsons/rms/internal/config"
	"github.com/watsons/rms/internal/namespace"
	"github.com/watsons/rms/internal/registry"
)

// ErrNoListeners is returned when no listener could be bound.
var ErrNoListeners = errors.New("no listener could be bound")

// bound pairs a listener with the HTTP server serving it.
type bound struct {
	ln  net.Listener
	srv *http.Server
	cfg config.Listener
}

// Listen binds every configured listener without serving, so a port conflict or
// an unusable bind address is reported before the process starts serving.
//
// The metrics listener is separate from the registry listeners on purpose:
// scraping endpoints and pprof must not be exposed on the port that serves
// images.
func (s *Server) Listen() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var errs []error
	for _, l := range s.cfg.EffectiveListeners() {
		ln, err := net.Listen("tcp", l.Listen)
		if err != nil {
			errs = append(errs, fmt.Errorf("listen %s: %w", l.Listen, err))
			continue
		}

		s.bound = append(s.bound, bound{
			ln:  ln,
			cfg: l,
			srv: &http.Server{
				Handler:           s.Handler(l.Namespace),
				ReadHeaderTimeout: 30 * time.Second,
				// ReadTimeout and WriteTimeout default to zero because a layer
				// push can outlive any sane fixed timeout.
				ReadTimeout:  s.cfg.Server.ReadTimeout.Std(),
				WriteTimeout: s.cfg.Server.WriteTimeout.Std(),
				IdleTimeout:  2 * time.Minute,
			},
		})

		s.log.Info("listening",
			"addr", ln.Addr().String(),
			"namespace", describeListenerNamespace(l.Namespace),
			"external_url", l.ExternalURL,
		)
	}

	if s.metricsHandler() != nil && s.cfg.Observability.Metrics.Listen != "" {
		addr := s.cfg.Observability.Metrics.Listen
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			errs = append(errs, fmt.Errorf("listen %s (metrics): %w", addr, err))
		} else {
			s.bound = append(s.bound, bound{
				ln:  ln,
				cfg: config.Listener{Listen: addr},
				srv: &http.Server{
					Handler:           s.metricsHandler(),
					ReadHeaderTimeout: 10 * time.Second,
					IdleTimeout:       time.Minute,
				},
			})
			s.log.Info("listening", "addr", ln.Addr().String(), "purpose", "metrics")
		}
	}

	if len(s.bound) == 0 {
		if len(errs) > 0 {
			return errors.Join(errs...)
		}
		return ErrNoListeners
	}
	return errors.Join(errs...)
}

// metricsHandler builds the dedicated metrics/pprof listener handler.
//
// The observability handler owns its own routing (it also serves the embedded
// registry's series), so it is installed as the catch-all rather than being
// wrapped in a second mux that would only know about the metrics path.
func (s *Server) metricsHandler() http.Handler {
	metrics := s.obs.Metrics
	pprofHandler := s.obs.Pprof

	if metrics == nil && pprofHandler == nil {
		return nil
	}
	if pprofHandler == nil {
		return metrics
	}

	mux := http.NewServeMux()
	mux.Handle("/debug/pprof/", pprofHandler)
	if metrics != nil {
		mux.Handle("/", metrics)
	}
	return mux
}

// Addrs returns the bound listener addresses, for tests and startup logs.
func (s *Server) Addrs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.bound))
	for _, b := range s.bound {
		out = append(out, b.ln.Addr().String())
	}
	return out
}

// Serve blocks until ctx is cancelled or a listener fails.
func (s *Server) Serve(ctx context.Context) error {
	s.mu.Lock()
	bs := make([]bound, len(s.bound))
	copy(bs, s.bound)
	s.mu.Unlock()

	if len(bs) == 0 {
		return ErrNoListeners
	}

	errCh := make(chan error, len(bs))
	for _, b := range bs {
		go func(b bound) {
			err := b.srv.Serve(b.ln)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("serve %s: %w", b.cfg.Listen, err)
				return
			}
			errCh <- nil
		}(b)
	}

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.Server.GracefulShutdown.Std())
		defer cancel()
		return s.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.Server.GracefulShutdown.Std())
			defer cancel()
			_ = s.Shutdown(shutdownCtx)
		}
		return err
	}
}

// Shutdown stops the HTTP servers and the namespace engines.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	bs := make([]bound, len(s.bound))
	copy(bs, s.bound)
	s.mu.Unlock()

	var errs []error
	for _, b := range bs {
		if err := b.srv.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown %s: %w", b.cfg.Listen, err))
		}
	}
	if err := s.mgr.Shutdown(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func describeListenerNamespace(ns string) string {
	if ns == "" {
		return "<multi-namespace>"
	}
	return ns
}

// Auth returns the authenticator so callers can report or reload it.
func (s *Server) Auth() *auth.Authenticator { return s.auth }

// Manager returns the namespace engine manager.
func (s *Server) Manager() *registry.Manager { return s.mgr }

// Table returns the routing table.
func (s *Server) Table() *namespace.Table { return s.table }
