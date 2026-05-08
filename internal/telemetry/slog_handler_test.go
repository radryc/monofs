package telemetry

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestWrapSlogHandlerAddsTraceContextToBaseLogs(t *testing.T) {
	var buf bytes.Buffer
	handler := WrapSlogHandler(slog.NewTextHandler(&buf, &slog.HandlerOptions{}), "monofs/test")
	logger := slog.New(handler)

	traceID, err := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("TraceIDFromHex() error = %v", err)
	}
	spanID, err := trace.SpanIDFromHex("0123456789abcdef")
	if err != nil {
		t.Fatalf("SpanIDFromHex() error = %v", err)
	}
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID,
		SpanID:  spanID,
		Remote:  true,
	}))

	logger.WarnContext(ctx, "read: metadata not found", "path", "/repo/missing.txt")

	output := buf.String()
	if !strings.Contains(output, "trace_id=0123456789abcdef0123456789abcdef") {
		t.Fatalf("expected trace_id in log output, got %q", output)
	}
	if !strings.Contains(output, "span_id=0123456789abcdef") {
		t.Fatalf("expected span_id in log output, got %q", output)
	}
	if !strings.Contains(output, "path=/repo/missing.txt") {
		t.Fatalf("expected original attrs in log output, got %q", output)
	}
}
