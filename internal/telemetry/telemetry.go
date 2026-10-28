// Package telemetry configures OpenTelemetry tracing, metrics, and cross-process
// context propagation, and exposes helpers to instrument Harbor.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Config configures OTLP export.
type Config struct {
	// Endpoint is the OTLP gRPC collector address (host:port). Empty disables
	// export: no provider is installed and spans/metrics are no-ops.
	Endpoint string
	// Insecure sends without TLS (plaintext), for a local collector.
	Insecure bool
	// ServiceName is the resource service.name; default "harbor".
	ServiceName string
}

// Init installs a global TracerProvider and MeterProvider exporting to the
// configured OTLP collector, sets the W3C trace-context propagator, and returns
// a shutdown function that flushes pending data. With an empty endpoint the
// providers stay no-op, so instrumented code costs nothing.
func Init(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	// Always install the W3C propagator so an incoming traceparent links the
	// task span to the caller's trace, even when export is disabled.
	otel.SetTextMapPropagator(propagation.TraceContext{})

	if cfg.Endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	name := cfg.ServiceName
	if name == "" {
		name = "harbor"
	}

	res, err := resource.New(ctx, resource.WithAttributes(attribute.String("service.name", name)))
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}

	traceOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
	metricOpts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		traceOpts = append(traceOpts, otlptracegrpc.WithInsecure())
		metricOpts = append(metricOpts, otlpmetricgrpc.WithInsecure())
	}

	texp, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		return nil, fmt.Errorf("otlp trace exporter: %w", err)
	}

	mexp, err := otlpmetricgrpc.New(ctx, metricOpts...)
	if err != nil {
		return nil, fmt.Errorf("otlp metric exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(texp), sdktrace.WithResource(res))
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(mexp)),
		sdkmetric.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)

	return func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}, nil
}

// Start begins a span named by the caller. If tracing is disabled (or the caller
// has no active provider) it returns a non-recording span, so callers always
// receive a valid span to end.
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return otel.Tracer("harbor").Start(ctx, name, trace.WithAttributes(attrs...))
}

// instruments holds the process-wide OTel metrics, created lazily on first use
// so they bind to the MeterProvider installed by Init.
type instruments struct {
	submitted metric.Int64Counter
	finished  metric.Int64Counter
	active    metric.Int64UpDownCounter
	duration  metric.Float64Histogram
	tokens    metric.Int64Counter
	cost      metric.Float64Counter
}

var (
	instOnce sync.Once
	inst     *instruments
)

func load() *instruments {
	instOnce.Do(func() {
		m := otel.Meter("harbor")
		inst = &instruments{
			submitted: instrument(m.Int64Counter("harbor.tasks.submitted")),
			finished:  instrument(m.Int64Counter("harbor.tasks")),
			active:    instrument(m.Int64UpDownCounter("harbor.tasks.active")),
			duration:  instrument(m.Float64Histogram("harbor.task.duration", metric.WithUnit("s"))),
			tokens:    instrument(m.Int64Counter("harbor.task.tokens")),
			cost:      instrument(m.Float64Counter("harbor.task.cost", metric.WithUnit("USD"))),
		}
	})

	return inst
}

// instrument returns v, reporting any instrument creation error via the global
// error handler (creation never fails fatally).
func instrument[T any](v T, err error) T {
	if err != nil {
		otel.Handle(err)
	}
	return v
}

func agent(agent string, extra ...attribute.KeyValue) metric.MeasurementOption {
	return metric.WithAttributes(append([]attribute.KeyValue{attribute.String("agent", agent)}, extra...)...)
}

// TaskSubmitted records a task submission.
func TaskSubmitted(agentID string) {
	load().submitted.Add(context.Background(), 1, agent(agentID))
}

// TaskActive adjusts the in-flight task gauge.
func TaskActive(delta int64) {
	if delta != 0 {
		load().active.Add(context.Background(), delta)
	}
}

// TaskFinished records a task's terminal outcome.
func TaskFinished(agentID, outcome string) {
	load().finished.Add(context.Background(), 1, agent(agentID, attribute.String("outcome", outcome)))
}

// TaskDuration records a task's duration.
func TaskDuration(agentID string, d time.Duration) {
	load().duration.Record(context.Background(), d.Seconds(), agent(agentID))
}

// TaskUsage records the token and cost usage a CLI reported.
func TaskUsage(agentID string, inputTokens, outputTokens int, costUSD float64) {
	if inputTokens != 0 {
		load().tokens.Add(context.Background(), int64(inputTokens), agent(agentID, attribute.String("direction", "input")))
	}

	if outputTokens != 0 {
		load().tokens.Add(context.Background(), int64(outputTokens), agent(agentID, attribute.String("direction", "output")))
	}

	if costUSD > 0 {
		load().cost.Add(context.Background(), costUSD, agent(agentID))
	}
}
