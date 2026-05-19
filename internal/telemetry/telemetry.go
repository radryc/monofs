package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
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
	"google.golang.org/grpc/stats"
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

var excludedDoctorIngestMethods = map[string]struct{}{
	pb.MonoFS_IngestLogs_FullMethodName:          {},
	pb.MonoFS_IngestMetrics_FullMethodName:       {},
	pb.MonoFS_IngestTraces_FullMethodName:        {},
	pb.MonoFSRouter_IngestLogs_FullMethodName:    {},
	pb.MonoFSRouter_IngestMetrics_FullMethodName: {},
	pb.MonoFSRouter_IngestTraces_FullMethodName:  {},
}

// NewGRPCServerStatsHandler returns the standard server stats handler with
// MonoFS-specific exclusions for doctor ingest RPCs. Without this filter, a
// collector that exports back into MonoFS doctor can create a telemetry loop by
// instrumenting the ingest RPCs themselves.
func NewGRPCServerStatsHandler() stats.Handler {
	return otelgrpc.NewServerHandler(otelgrpc.WithFilter(ShouldInstrumentGRPCServerRPC))
}

// ShouldInstrumentGRPCServerRPC reports whether a server-side gRPC request
// should emit OpenTelemetry data.
func ShouldInstrumentGRPCServerRPC(info *stats.RPCTagInfo) bool {
	if info == nil {
		return true
	}
	_, excluded := excludedDoctorIngestMethods[info.FullMethodName]
	return !excluded
}

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

func emitSlogRecord(ctx context.Context, scope string, record slog.Record) {
	providerMu.RLock()
	current := provider
	providerMu.RUnlock()
	if current == nil {
		return
	}

	logger := current.Logger(scope)
	var otelRecord apilog.Record
	otelRecord.SetTimestamp(record.Time.UTC())
	otelRecord.SetSeverity(severityForSlogLevel(record.Level))
	otelRecord.SetSeverityText(severityText(severityForSlogLevel(record.Level)))
	otelRecord.SetBody(apilog.StringValue(record.Message))
	record.Attrs(func(attr slog.Attr) bool {
		otelRecord.AddAttributes(slogAttrToLogKV(attr))
		return true
	})
	logger.Emit(ctx, otelRecord)
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

func slogAttrToLogKV(attr slog.Attr) apilog.KeyValue {
	return apilog.KeyValue{
		Key:   attr.Key,
		Value: slogValueToLogValue(attr.Value.Resolve()),
	}
}

func slogValueToLogValue(value slog.Value) apilog.Value {
	switch value.Kind() {
	case slog.KindBool:
		return apilog.BoolValue(value.Bool())
	case slog.KindDuration:
		return apilog.StringValue(value.Duration().String())
	case slog.KindFloat64:
		return apilog.Float64Value(value.Float64())
	case slog.KindInt64:
		return apilog.Int64Value(value.Int64())
	case slog.KindString:
		return apilog.StringValue(value.String())
	case slog.KindTime:
		return apilog.StringValue(value.Time().Format(time.RFC3339Nano))
	case slog.KindUint64:
		uintValue := value.Uint64()
		if uintValue <= uint64(^uint64(0)>>1) {
			return apilog.Int64Value(int64(uintValue))
		}
		return apilog.StringValue(fmt.Sprintf("%d", uintValue))
	case slog.KindGroup:
		group := value.Group()
		kvs := make([]apilog.KeyValue, 0, len(group))
		for _, nested := range group {
			kvs = append(kvs, slogAttrToLogKV(nested))
		}
		return apilog.MapValue(kvs...)
	default:
		return apilog.StringValue(fmt.Sprint(value.Any()))
	}
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
