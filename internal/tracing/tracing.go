package tracing

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/revature/corems-code-executor/internal/config"
)

// Provider wraps the OpenTelemetry TracerProvider.
type Provider struct {
	tp  *sdktrace.TracerProvider
	log *zap.Logger
}

// Init sets up the OpenTelemetry tracing pipeline.
// If tracing is disabled, it configures a no-op provider.
func Init(cfg *config.Config, log *zap.Logger) (*Provider, error) {
	if !cfg.Tracing.Enabled {
		otel.SetTracerProvider(trace.NewNoopTracerProvider())
		return &Provider{log: log}, nil
	}

	ctx := context.Background()

	// otlptracehttp.WithEndpoint expects "host:port" WITHOUT a scheme. A full
	// URL like "http://jaeger:4318" otherwise gets mangled into
	// "http://http:%2F%2Fjaeger:4318/v1/traces". Strip the scheme and let an
	// http:// endpoint imply insecure (plaintext) transport.
	endpoint, insecure := parseOTLPEndpoint(cfg.Tracing.Endpoint, cfg.Tracing.Insecure)
	httpOpts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(endpoint),
	}
	if insecure {
		httpOpts = append(httpOpts, otlptracehttp.WithInsecure())
	}

	exporter, err := otlptracehttp.New(ctx, httpOpts...)
	if err != nil {
		return nil, fmt.Errorf("create OTLP exporter: %w", err)
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.Tracing.ServiceName),
		),
	)
	if err != nil {
		// Schema URL version mismatch between SDK default and semconv package is
		// non-fatal; Merge still returns a usable resource.
		log.Warn("OTel resource merge warning", zap.Error(err))
		if res == nil {
			res = resource.NewWithAttributes(
				semconv.SchemaURL,
				semconv.ServiceName(cfg.Tracing.ServiceName),
			)
		}
	}

	sampler := sdktrace.ParentBased(
		sdktrace.TraceIDRatioBased(cfg.Tracing.SampleRate),
	)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)

	otel.SetTracerProvider(tp)

	log.Info("OpenTelemetry tracing initialised",
		zap.String("endpoint", cfg.Tracing.Endpoint),
		zap.Float64("sampling_rate", cfg.Tracing.SampleRate),
	)

	return &Provider{tp: tp, log: log}, nil
}

// Shutdown flushes and shuts down the tracer provider.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p.tp == nil {
		return nil
	}
	if err := p.tp.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown tracer: %w", err)
	}
	p.log.Info("OpenTelemetry tracer shut down")
	return nil
}

// parseOTLPEndpoint normalises a tracing endpoint into the "host:port" form
// otlptracehttp.WithEndpoint requires (no scheme, no trailing slash). An
// http:// scheme forces insecure transport; https:// keeps the configured
// setting; a bare host:port is returned unchanged.
func parseOTLPEndpoint(raw string, insecure bool) (string, bool) {
	raw = strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(raw, "http://"):
		return strings.TrimRight(strings.TrimPrefix(raw, "http://"), "/"), true
	case strings.HasPrefix(raw, "https://"):
		return strings.TrimRight(strings.TrimPrefix(raw, "https://"), "/"), insecure
	default:
		return strings.TrimRight(raw, "/"), insecure
	}
}
