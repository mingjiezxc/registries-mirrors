package obs

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"

	"github.com/watsons/rms/internal/config"
)

// ServiceName identifies rms in the trace backend.
const ServiceName = "rms"

// Tracing owns the OpenTelemetry trace pipeline.
type Tracing struct {
	provider *sdktrace.TracerProvider
}

// InitTracing configures an OTLP trace exporter.
//
// The default configuration targets the SigNoz instance in this environment
// (192.168.188.103:4317) over plain gRPC.
func InitTracing(ctx context.Context, cfg config.Traces, version string) (*Tracing, error) {
	if !cfg.Enabled {
		return &Tracing{}, nil
	}

	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
	}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		headers := map[string]string{}
		for _, h := range cfg.Headers {
			k, v, ok := strings.Cut(h, "=")
			if !ok {
				return nil, fmt.Errorf("traces.headers: %q is not key=value", h)
			}
			headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
		opts = append(opts, otlptracegrpc.WithHeaders(headers))
	}

	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("create OTLP trace exporter for %s: %w", cfg.Endpoint, err)
	}

	// NewSchemaless rather than NewWithAttributes: the SDK's default resource
	// already carries a semantic-convention schema URL, and merging two
	// different schema URLs is rejected as a conflict.
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName(ServiceName),
		semconv.ServiceVersion(version),
	))
	if err != nil {
		return nil, fmt.Errorf("build trace resource: %w", err)
	}

	ratio := cfg.SampleRatio
	if ratio <= 0 {
		ratio = 1
	}
	if ratio > 1 {
		ratio = 1
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(5*time.Second)),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	return &Tracing{provider: provider}, nil
}

// Enabled reports whether traces are being exported.
func (t *Tracing) Enabled() bool { return t != nil && t.provider != nil }

// Middleware instruments the handler. It returns next unchanged when tracing is
// off, so there is no cost when the feature is disabled.
func (t *Tracing) Middleware(next http.Handler) http.Handler {
	if !t.Enabled() {
		return next
	}
	return otelhttp.NewHandler(next, "rms",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + spanPath(r.URL.Path)
		}),
	)
}

// spanPath keeps span names low-cardinality by reporting the registry path shape
// rather than the digest or tag.
func spanPath(p string) string {
	segments := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(segments) < 2 || segments[0] != "v2" {
		return p
	}
	for i, s := range segments {
		switch s {
		case "manifests", "blobs", "tags":
			return "/" + strings.Join(segments[:i+1], "/")
		}
	}
	return p
}

// Shutdown flushes pending spans.
func (t *Tracing) Shutdown(ctx context.Context) error {
	if !t.Enabled() {
		return nil
	}
	return t.provider.Shutdown(ctx)
}

// NamespaceAttributes are the span attributes rms adds to registry requests.
func NamespaceAttributes(namespace, mode string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("rms.namespace", namespace),
		attribute.String("rms.mode", mode),
	}
}
