package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestTraceMiddlewareLinksParent verifies an incoming traceparent header makes
// the handler's span a child of the caller's span (cross-process context).
func TestTraceMiddlewareLinksParent(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(t.Context())
	})

	h := traceMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, span := otel.Tracer("test").Start(r.Context(), "child")
		span.End()
		w.WriteHeader(http.StatusOK)
	}))

	const parent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("traceparent", parent)
	h.ServeHTTP(httptest.NewRecorder(), req)

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	if got := spans[0].Parent().SpanID().String(); got != "00f067aa0ba902b7" {
		t.Fatalf("parent span id = %q, want the traceparent's", got)
	}
}
