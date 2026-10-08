package bridge_test

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// TestExecuteEmitsSpans verifies a task produces a harbor.task span with the
// terminal outcome and a nested harbor.cli subprocess span.
func TestExecuteEmitsSpans(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})

	h := newHandler(t, newLauncher(t, scriptPath(t, "fake-codebuddy.sh"), "stream-json", nil, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := h.SendMessage(ctx, &a2a.SendMessageRequest{Message: message("hi")})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if task := resp.(*a2a.Task); task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s, want completed", task.Status.State)
	}

	// The subprocess span ends when its read loop finishes, which may trail the
	// completing event; give it a moment.
	names := map[string]bool{}
	outcome := ""
	for i := 0; i < 100; i++ {
		names, outcome = spanSummary(rec)
		if names["harbor.task"] && names["harbor.cli"] {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !names["harbor.task"] {
		t.Errorf("missing harbor.task span; spans = %v", names)
	}
	if !names["harbor.cli"] {
		t.Errorf("missing harbor.cli span; spans = %v", names)
	}
	if outcome != "completed" {
		t.Errorf("harbor.outcome = %q, want completed", outcome)
	}
}

// spanSummary collects recorded span names and the task outcome attribute.
func spanSummary(rec *tracetest.SpanRecorder) (map[string]bool, string) {
	names := map[string]bool{}
	outcome := ""
	for _, s := range rec.Ended() {
		names[s.Name()] = true
		for _, a := range s.Attributes() {
			if a.Key == "harbor.outcome" {
				outcome = a.Value.AsString()
			}
		}
	}
	return names, outcome
}
