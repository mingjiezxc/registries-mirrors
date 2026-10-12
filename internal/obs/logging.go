// Package obs configures logging, metrics and tracing.
//
// Metrics and traces default to the environment's SigNoz instance
// (192.168.188.103); logging is always on.
package obs

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/watsons/rms/internal/config"
)

// NewLogger builds the process logger.
func NewLogger(cfg config.Logs, w io.Writer) (*slog.Logger, error) {
	if w == nil {
		w = os.Stderr
	}

	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	switch cfg.Format {
	case "text":
		handler = slog.NewTextHandler(w, opts)
	case "json":
		handler = slog.NewJSONHandler(w, opts)
	default:
		return nil, fmt.Errorf("unsupported log format %q", cfg.Format)
	}

	return slog.New(handler), nil
}

func parseLevel(level string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unsupported log level %q", level)
	}
}

// ConfigureLogrus aligns the registry engine's logger with rms's own.
//
// The embedded distribution registry logs through logrus, whose level and
// formatter are process globals and must be set once, before any engine is
// constructed.
func ConfigureLogrus(cfg config.Logs, w io.Writer) error {
	if w == nil {
		w = os.Stderr
	}

	level, err := logrus.ParseLevel(strings.ToLower(strings.TrimSpace(cfg.Level)))
	if err != nil {
		return fmt.Errorf("unsupported log level %q: %w", cfg.Level, err)
	}

	logrus.SetLevel(level)
	logrus.SetOutput(w)
	// The registry is a component of rms, so its entries are tagged rather than
	// left looking like a separate process.
	logrus.SetFormatter(&logrus.JSONFormatter{
		TimestampFormat:   "2006-01-02T15:04:05.000Z07:00",
		DisableHTMLEscape: true,
		FieldMap: logrus.FieldMap{
			logrus.FieldKeyMsg: "msg",
		},
	})
	return nil
}
