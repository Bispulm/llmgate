package main

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/Bispulm/llmgate/internal/config"
)

// tracingShutdownTimeout bounds flushing the last spans at exit.
const tracingShutdownTimeout = 5 * time.Second

// tracesURL adds OTLP's standard path to a collector's base URL, as the usual
// OTEL_EXPORTER_OTLP_ENDPOINT does. A URL that already has a path is used as written.
func tracesURL(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Path != "" && u.Path != "/") {
		return endpoint
	}
	u.Path = "/v1/traces"
	return u.String()
}

// startTracing returns a tracer that exports over OTLP/HTTP, and a function that flushes
// and stops it. Without an endpoint, it returns nil, and nothing is traced.
func startTracing(ctx context.Context, cfg config.Tracing) (trace.Tracer, func(), error) {
	if cfg.OTLPEndpoint == "" {
		return nil, func() {}, nil
	}
	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(tracesURL(cfg.OTLPEndpoint)))
	if err != nil {
		return nil, nil, fmt.Errorf("starting the trace exporter: %w", err)
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", "llmgate"))),
	)
	stop := func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tracingShutdownTimeout)
		defer cancel()
		_ = provider.Shutdown(stopCtx)
	}
	return provider.Tracer("github.com/Bispulm/llmgate"), stop, nil
}
