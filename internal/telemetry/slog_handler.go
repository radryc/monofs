package telemetry

import (
	"context"
	"log/slog"

	apilog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

type slogHandler struct {
	base  slog.Handler
	scope string
}

func (h *slogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.Enabled(ctx, level)
}

func (h *slogHandler) Handle(ctx context.Context, record slog.Record) error {
	record = enrichRecordWithTraceContext(ctx, record.Clone())
	if err := h.base.Handle(ctx, record); err != nil {
		return err
	}
	emitSlogRecord(ctx, h.scope, record)
	return nil
}

func (h *slogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &slogHandler{base: h.base.WithAttrs(attrs), scope: h.scope}
}

func (h *slogHandler) WithGroup(name string) slog.Handler {
	return &slogHandler{base: h.base.WithGroup(name), scope: h.scope}
}

func severityForSlogLevel(level slog.Level) apilog.Severity {
	switch {
	case level >= slog.LevelError:
		return apilog.SeverityError
	case level >= slog.LevelWarn:
		return apilog.SeverityWarn
	default:
		return apilog.SeverityInfo
	}
}

func enrichRecordWithTraceContext(ctx context.Context, record slog.Record) slog.Record {
	spanCtx := trace.SpanContextFromContext(ctx)
	if !spanCtx.IsValid() {
		return record
	}
	if !recordHasAttr(record, "trace_id") {
		record.AddAttrs(slog.String("trace_id", spanCtx.TraceID().String()))
	}
	if !recordHasAttr(record, "span_id") {
		record.AddAttrs(slog.String("span_id", spanCtx.SpanID().String()))
	}
	return record
}

func recordHasAttr(record slog.Record, key string) bool {
	found := false
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == key {
			found = true
			return false
		}
		return true
	})
	return found
}
