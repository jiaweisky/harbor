package telemetry_test

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/jiaweisky/harbor/internal/telemetry"
)

// TestMetricsRecorded verifies the recorder helpers emit the expected OTLP
// metrics through the installed MeterProvider.
func TestMetricsRecorded(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		otel.SetMeterProvider(prev)
		_ = mp.Shutdown(context.Background())
	})

	telemetry.TaskSubmitted("cb")
	telemetry.TaskActive(1)
	telemetry.TaskFinished("cb", "completed")
	telemetry.TaskDuration("cb", 1500*time.Millisecond)
	telemetry.TaskUsage("cb", 3, 4, 0.5)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	names := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names[m.Name] = true
		}
	}
	for _, want := range []string{
		"harbor.tasks.submitted", "harbor.tasks", "harbor.tasks.active",
		"harbor.task.duration", "harbor.task.tokens", "harbor.task.cost",
	} {
		if !names[want] {
			t.Errorf("metric %q not recorded; got %v", want, names)
		}
	}
}

// TestInitDisabled verifies an empty endpoint installs nothing and returns a
// usable shutdown.
func TestInitDisabled(t *testing.T) {
	shutdown, err := telemetry.Init(context.Background(), telemetry.Config{})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// TestStartRecordsSpan verifies Start routes through the global tracer provider
// and carries attributes.
func TestStartRecordsSpan(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})

	_, span := telemetry.Start(context.Background(), "harbor.task", attribute.String("harbor.agent", "cb"))
	span.End()

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	if spans[0].Name() != "harbor.task" {
		t.Fatalf("span name = %q, want harbor.task", spans[0].Name())
	}
	found := false
	for _, a := range spans[0].Attributes() {
		if a.Key == "harbor.agent" && a.Value.AsString() == "cb" {
			found = true
		}
	}
	if !found {
		t.Fatalf("harbor.agent attribute missing: %v", spans[0].Attributes())
	}
}

// TestStartNested verifies a child span is linked to its parent.
func TestStartNested(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})

	ctx, parent := telemetry.Start(context.Background(), "harbor.task")
	_, child := telemetry.Start(ctx, "harbor.cli")
	child.End()
	parent.End()

	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("recorded %d spans, want 2", len(spans))
	}
	byName := make(map[string]sdktrace.ReadOnlySpan, len(spans))
	for _, s := range spans {
		byName[s.Name()] = s
	}
	childSpan, parentSpan := byName["harbor.cli"], byName["harbor.task"]
	if childSpan == nil || parentSpan == nil {
		t.Fatalf("missing spans: %v", byName)
	}
	if childSpan.Parent().SpanID() != parentSpan.SpanContext().SpanID() {
		t.Fatal("child span is not linked to the parent span")
	}
	_ = trace.SpanFromContext(ctx)
}
