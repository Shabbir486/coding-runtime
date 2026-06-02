package tracing

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/mdshabbir-ali/code-runtime/internal/config"
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

	httpOpts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(cfg.Tracing.Endpoint),
	}
	if cfg.Tracing.Insecure {
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
