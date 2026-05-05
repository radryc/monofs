package telemetry

import (
	"context"
	"log/slog"

	apilog "go.opentelemetry.io/otel/log"
)

type slogHandler struct {
	base  slog.Handler
	scope string
}

func (h *slogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.Enabled(ctx, level)
}

func (h *slogHandler) Handle(ctx context.Context, record slog.Record) error {
	if err := h.base.Handle(ctx, record); err != nil {
		return err
	}
	emitLogRecord(ctx, h.scope, severityForSlogLevel(record.Level), record.Message)
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
