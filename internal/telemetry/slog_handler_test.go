package telemetry

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/stats"
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

func TestShouldInstrumentGRPCServerRPCExcludesDoctorIngestMethods(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		fullMethod string
		want       bool
	}{
		{name: "server ingest logs", fullMethod: pb.MonoFS_IngestLogs_FullMethodName, want: false},
		{name: "server ingest metrics", fullMethod: pb.MonoFS_IngestMetrics_FullMethodName, want: false},
		{name: "server ingest traces", fullMethod: pb.MonoFS_IngestTraces_FullMethodName, want: false},
		{name: "router ingest logs", fullMethod: pb.MonoFSRouter_IngestLogs_FullMethodName, want: false},
		{name: "router ingest metrics", fullMethod: pb.MonoFSRouter_IngestMetrics_FullMethodName, want: false},
		{name: "router ingest traces", fullMethod: pb.MonoFSRouter_IngestTraces_FullMethodName, want: false},
		{name: "normal read rpc", fullMethod: pb.MonoFS_Read_FullMethodName, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			info := &stats.RPCTagInfo{FullMethodName: tc.fullMethod}
			if got := ShouldInstrumentGRPCServerRPC(info); got != tc.want {
				t.Fatalf("ShouldInstrumentGRPCServerRPC(%q) = %v, want %v", tc.fullMethod, got, tc.want)
			}
		})
	}

	if got := ShouldInstrumentGRPCServerRPC(nil); !got {
		t.Fatal("ShouldInstrumentGRPCServerRPC(nil) = false, want true")
	}
}
