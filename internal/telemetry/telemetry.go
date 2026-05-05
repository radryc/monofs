package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	apilog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
)

type Handle struct {
	enabled bool
	traces  *sdktrace.TracerProvider
	metrics *sdkmetric.MeterProvider
	logs    *sdklog.LoggerProvider
}

var (
	providerMu sync.RWMutex
	provider   *sdklog.LoggerProvider
)

func Setup(ctx context.Context, cfg Config) (*Handle, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return &Handle{}, nil
	}
	res := resource.NewWithAttributes("", buildResourceAttributes(cfg)...)

	traceOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
	metricOpts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.Endpoint)}
	logOpts := []otlploggrpc.Option{otlploggrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		traceOpts = append(traceOpts, otlptracegrpc.WithInsecure())
		metricOpts = append(metricOpts, otlpmetricgrpc.WithInsecure())
		logOpts = append(logOpts, otlploggrpc.WithInsecure())
	}

	traceExporter, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		return nil, fmt.Errorf("create trace exporter: %w", err)
	}
	metricExporter, err := otlpmetricgrpc.New(ctx, metricOpts...)
	if err != nil {
		return nil, fmt.Errorf("create metric exporter: %w", err)
	}
	logExporter, err := otlploggrpc.New(ctx, logOpts...)
	if err != nil {
		return nil, fmt.Errorf("create log exporter: %w", err)
	}

	traceProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
	)
	metricProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter, sdkmetric.WithInterval(cfg.MetricInterval))),
	)
	logProvider := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
	)

	otel.SetTracerProvider(traceProvider)
	otel.SetMeterProvider(metricProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	providerMu.Lock()
	provider = logProvider
	providerMu.Unlock()

	return &Handle{
		enabled: true,
		traces:  traceProvider,
		metrics: metricProvider,
		logs:    logProvider,
	}, nil
}

func (h *Handle) Enabled() bool {
	return h != nil && h.enabled
}

func (h *Handle) Shutdown(ctx context.Context) error {
	if h == nil || !h.enabled {
		return nil
	}
	providerMu.Lock()
	if provider == h.logs {
		provider = nil
	}
	providerMu.Unlock()

	var errs []error
	if err := h.logs.Shutdown(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := h.metrics.Shutdown(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := h.traces.Shutdown(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func WrapSlogHandler(base slog.Handler, scope string) slog.Handler {
	return &slogHandler{base: base, scope: scope}
}

func EmitInfo(ctx context.Context, scope, message string) {
	emitLogRecord(ctx, scope, apilog.SeverityInfo, message)
}

func emitLogRecord(ctx context.Context, scope string, severity apilog.Severity, message string) {
	providerMu.RLock()
	current := provider
	providerMu.RUnlock()
	if current == nil {
		return
	}

	logger := current.Logger(scope)
	var record apilog.Record
	record.SetTimestamp(time.Now().UTC())
	record.SetSeverity(severity)
	record.SetSeverityText(severityText(severity))
	record.SetBody(apilog.StringValue(message))
	logger.Emit(ctx, record)
}

func buildResourceAttributes(cfg Config) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("service.name", cfg.ServiceName),
		attribute.String("monofs.component", cfg.Component),
	}
	if cfg.Component != "" {
		attrs = append(attrs, attribute.String("service.version", cfg.Component))
	}
	if cfg.InstanceID != "" {
		attrs = append(attrs, attribute.String("service.instance.id", cfg.InstanceID))
	}
	return attrs
}

func severityText(severity apilog.Severity) string {
	switch severity {
	case apilog.SeverityFatal:
		return "FATAL"
	case apilog.SeverityError:
		return "ERROR"
	case apilog.SeverityWarn:
		return "WARN"
	default:
		return "INFO"
	}
}
