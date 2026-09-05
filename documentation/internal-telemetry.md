# Internal Telemetry Package

Package path: `internal/telemetry`

This package provides OpenTelemetry integration for traces, metrics, and logs (OTLP over gRPC), alongside a custom `slog.Handler` that bridges Go's structured logging into the OTel logging pipeline.

---

## File: `config.go`

Package-level constants and configuration loading.

### Constants

```go
defaultServiceName    = "monofs"
defaultMetricInterval = 15 * time.Second
```

- `defaultServiceName` — Fallback value for `ServiceName` when `MONOFS_OTEL_SERVICE_NAME` env var is empty.
- `defaultMetricInterval` — Fallback interval for the metric periodic reader when `MONOFS_OTEL_METRIC_INTERVAL` env var is empty.

---

### `type Config struct`

```go
type Config struct {
    Endpoint       string
    ServiceName    string
    Component      string
    InstanceID     string
    Insecure       bool
    MetricInterval time.Duration
}
```

Holds all telemetry configuration values derived from environment variables.

| Field | Env Var | Default |
|---|---|---|
| `Endpoint` | `MONOFS_OTEL_ENDPOINT` or `OTEL_EXPORTER_OTLP_ENDPOINT` | `""` (telemetry disabled) |
| `ServiceName` | `MONOFS_OTEL_SERVICE_NAME` | `"monofs"` |
| `Component` | (explicit argument) | `filepath.Base(os.Args[0])` |
| `InstanceID` | `HOSTNAME` | value of `Component` |
| `Insecure` | `MONOFS_OTEL_INSECURE` | `true` |
| `MetricInterval` | `MONOFS_OTEL_METRIC_INTERVAL` | `15s` |

---

### `func LoadConfig(component string) (Config, error)`

```go
func LoadConfig(component string) (Config, error)
```

**What it does:** Reads telemetry configuration from environment variables and returns a populated `Config`. If `Endpoint` is empty, returns early with the partially-populated config (telemetry will be disabled).

**Called from:** Every `main.go` entrypoint:
- `cmd/monofs-server/main.go:120`
- `cmd/monofs-router/main.go:106`
- `cmd/monofs-fetcher/main.go:218`
- `cmd/monofs-search/main.go:50`
- `cmd/monofs-registry/main.go:61`
- `cmd/monofs-pipeline-worker/main.go:53`

**Parameters:**
- `component` — A human-readable identifier for this component (e.g. `"monofs-server"`). If empty, defaults to the binary's basename.

**Returns:**
- `Config` — The populated configuration.
- `error` — Non-nil if `MONOFS_OTEL_INSECURE` or `MONOFS_OTEL_METRIC_INTERVAL` parse fails.

**Implementation details:**
- Uses `envFirst` to check `MONOFS_OTEL_ENDPOINT` first, falling back to the standard `OTEL_EXPORTER_OTLP_ENDPOINT`.
- Returns early (with `Endpoint == ""`) if no OTLP endpoint is configured — all callers treat empty endpoint as "telemetry disabled."
- `Insecure` defaults to `true` when the env var is absent/unset, matching typical local collector setups.
- `MetricInterval` is parsed via `time.ParseDuration`.

---

### `func envFirst(keys ...string) string`

```go
func envFirst(keys ...string) string
```

**What it does:** Iterates over environment variable names in order and returns the first non-empty (after trimming) value.

**Called from:** `LoadConfig` only (`config.go:28`).

**Parameters:**
- `keys` — Ordered list of environment variable names to check.

**Returns:** The first non-empty value found, or `""` if none are set.

**Implementation details:** Uses `os.Getenv` + `strings.TrimSpace`. Empty string return means no value was found for any key.

---

## File: `slog_handler.go`

A custom `slog.Handler` implementation that wraps a base handler and bridges log records into the OpenTelemetry log pipeline while enriching records with trace/span context.

---

### `type slogHandler struct`

```go
type slogHandler struct {
    base  slog.Handler
    scope string
}
```

Private struct implementing `slog.Handler`. Wraps a base handler (e.g. JSON or text handler) and adds OTel log emission and trace-context enrichment.

| Field | Purpose |
|---|---|
| `base` | The underlying slog handler to which records are also forwarded. |
| `scope` | OTel logger scope name (e.g. `"monofs/server"`). |

---

### `func (h *slogHandler) Enabled(ctx context.Context, level slog.Level) bool`

```go
func (h *slogHandler) Enabled(ctx context.Context, level slog.Level) bool
```

**What it does:** Delegates to the base handler's `Enabled` method. Satisfies the `slog.Handler` interface.

**Called from:** The `slog` package internally when deciding whether to call `Handle` for a given log level. Not called directly by application code.

**Parameters:**
- `ctx` — Context (forwarded to base handler).
- `level` — The slog log level being queried.

**Returns:** `true` if the base handler is configured to emit this level.

---

### `func (h *slogHandler) Handle(ctx context.Context, record slog.Record) error`

```go
func (h *slogHandler) Handle(ctx context.Context, record slog.Record) error
```

**What it does:** The core of the bridge. On each log call:
1. Clones the record and enriches it with `trace_id` and `span_id` from the context (if a valid trace span is present and the attributes are not already set).
2. Forwards the enriched record to the base handler.
3. Emits the enriched record into the OTel log pipeline via `emitSlogRecord`.

**Called from:** The `slog` package whenever a log call passes the `Enabled` check. Not called directly by application code.

**Parameters:**
- `ctx` — Context carrying the current OpenTelemetry span.
- `record` — The original slog record (message, level, time, attrs).

**Returns:** An error from the base handler, or nil.

**Implementation details:**
- Calls `record.Clone()` before enrichment to avoid mutating the original.
- Both the base handler write and the OTel emission happen; errors from the base handler are returned, but OTel emission errors are silently swallowed (consistent with the "fire and forget" nature of log emission).

---

### `func (h *slogHandler) WithAttrs(attrs []slog.Attr) slog.Handler`

```go
func (h *slogHandler) WithAttrs(attrs []slog.Attr) slog.Handler
```

**What it does:** Returns a new `slogHandler` with the given attributes pre-attached to the base handler. Satisfies the `slog.Handler` interface.

**Called from:** The `slog` package when `slog.With(...)` is used. Not called directly by application code.

**Parameters:**
- `attrs` — Attributes to pre-attach.

**Returns:** A new `slogHandler` wrapping the base handler with the additional attributes, preserving the same scope.

---

### `func (h *slogHandler) WithGroup(name string) slog.Handler`

```go
func (h *slogHandler) WithGroup(name string) slog.Handler
```

**What it does:** Returns a new `slogHandler` with the given group name applied to the base handler. Satisfies the `slog.Handler` interface.

**Called from:** The `slog` package when `slog.WithGroup(...)` is used. Not called directly by application code.

**Parameters:**
- `name` — Group name to apply.

**Returns:** A new `slogHandler` wrapping the grouped base handler, preserving the same scope.

---

### `func severityForSlogLevel(level slog.Level) apilog.Severity`

```go
func severityForSlogLevel(level slog.Level) apilog.Severity
```

**What it does:** Maps Go `slog.Level` to OpenTelemetry `log.Severity`.

**Called from:**
- `emitSlogRecord` (`telemetry.go:174,175`) — to set severity on emitted OTel log records.

**Parameters:**
- `level` — The slog log level.

**Returns:** OTel severity enum.

| slog.Level | OTel Severity |
|---|---|
| `>= LevelError` | `SeverityError` |
| `>= LevelWarn` | `SeverityWarn` |
| `< LevelWarn` | `SeverityInfo` |

**Implementation details:** Note that `SeverityFatal*` is never returned because slog has no fatal level — but `severityText` (called with the same value) does handle it for completeness.

---

### `func enrichRecordWithTraceContext(ctx context.Context, record slog.Record) slog.Record`

```go
func enrichRecordWithTraceContext(ctx context.Context, record slog.Record) slog.Record
```

**What it does:** Adds `trace_id` and `span_id` string attributes to a slog record if a valid OpenTelemetry span is present in the context and those keys are not already set on the record.

**Called from:** `slogHandler.Handle` (`slog_handler.go:21`).

**Parameters:**
- `ctx` — Context from which to extract the span context.
- `record` — A clone of the original slog record (already cloned by the caller).

**Returns:** The record (mutated in-place via `AddAttrs`).

**Implementation details:**
- Uses `trace.SpanContextFromContext(ctx)` to extract span context.
- Returns the record unchanged if `spanCtx.IsValid()` is false.
- Checks `recordHasAttr` before adding each key to avoid duplication when the caller already sets these fields explicitly.

---

### `func recordHasAttr(record slog.Record, key string) bool`

```go
func recordHasAttr(record slog.Record, key string) bool
```

**What it does:** Iterates through a slog record's attributes to determine if a key already exists.

**Called from:** `enrichRecordWithTraceContext` (`slog_handler.go:53,56`).

**Parameters:**
- `record` — The slog record to search.
- `key` — The attribute key to look for.

**Returns:** `true` if an attribute with the given key exists, `false` otherwise.

**Implementation details:** Uses `record.Attrs(func(attr slog.Attr) bool { ... })` with early termination (returns false from the callback) once a match is found.

---

## File: `telemetry.go`

Core setup, shutdown, and the bridge from slog to OpenTelemetry log records.

---

### `type Handle struct`

```go
type Handle struct {
    enabled bool
    traces  *sdktrace.TracerProvider
    metrics *sdkmetric.MeterProvider
    logs    *sdklog.LoggerProvider
}
```

Owns the three OTel SDK providers. If `enabled` is `false`, all methods no-op gracefully.

| Field | Type | Purpose |
|---|---|---|
| `enabled` | `bool` | Whether telemetry was initialized. `Setup` sets this to `true` only if an endpoint was configured. |
| `traces` | `*sdktrace.TracerProvider` | SDK trace provider with batch exporter. |
| `metrics` | `*sdkmetric.MeterProvider` | SDK meter provider with periodic reader. |
| `logs` | `*sdklog.LoggerProvider` | SDK log provider with batch processor. |

---

### Package-level variables

```go
var (
    providerMu sync.RWMutex
    provider   *sdklog.LoggerProvider
)
```

- `providerMu` — Protects concurrent access to the global `provider` variable.
- `provider` — The currently active `*sdklog.LoggerProvider`. Set on `Setup` and cleared on `Shutdown`. Used by `emitSlogRecord` and `emitLogRecord` to create per-scope loggers.

```go
var excludedDoctorIngestMethods = map[string]struct{}{
    pb.MonoFS_IngestLogs_FullMethodName:          {},
    pb.MonoFS_IngestMetrics_FullMethodName:       {},
    pb.MonoFS_IngestTraces_FullMethodName:        {},
    pb.MonoFSRouter_IngestLogs_FullMethodName:    {},
    pb.MonoFSRouter_IngestMetrics_FullMethodName: {},
    pb.MonoFSRouter_IngestTraces_FullMethodName:  {},
}
```

A set of gRPC full method names for the doctor ingest RPCs. These are excluded from instrumentation to prevent telemetry loops when a collector exports back into MonoFS doctor.

---

### `func NewGRPCServerStatsHandler() stats.Handler`

```go
func NewGRPCServerStatsHandler() stats.Handler
```

**What it does:** Creates a gRPC server stats handler that instruments server-side RPCs with OpenTelemetry telemetry, but filters out MonoFS doctor ingest RPCs (to avoid telemetry feedback loops).

**Called from:**
- `cmd/monofs-server/main.go:181` — used as a `grpc.StatsHandler` server option.
- `cmd/monofs-router/main.go:333` — used as a `grpc.StatsHandler` server option.

**Returns:** An `otelgrpc.ServerHandler` wrapped with the `ShouldInstrumentGRPCServerRPC` filter.

**Why the exclusion exists:** If an OTel collector exports telemetry into MonoFS doctor's ingest endpoints, instrumenting those ingest RPCs would create additional telemetry traffic, which would then be ingested again, creating an infinite loop.

---

### `func ShouldInstrumentGRPCServerRPC(info *stats.RPCTagInfo) bool`

```go
func ShouldInstrumentGRPCServerRPC(info *stats.RPCTagInfo) bool
```

**What it does:** Decides whether a given gRPC RPC should be instrumented. Returns `false` for doctor ingest methods to prevent telemetry loops.

**Called from:** `NewGRPCServerStatsHandler` (as a filter callback passed to `otelgrpc.WithFilter`). Tested in `slog_handler_test.go:48`.

**Parameters:**
- `info` — gRPC RPC tag info. If nil, returns `true` (instrument everything).

**Returns:** `false` if `info.FullMethodName` is one of the six doctor ingest RPCs; `true` otherwise.

---

### `func Setup(ctx context.Context, cfg Config) (*Handle, error)`

```go
func Setup(ctx context.Context, cfg Config) (*Handle, error)
```

**What it does:** Initializes the OpenTelemetry SDK. If `cfg.Endpoint` is empty, returns a disabled `Handle` (no-op). Otherwise:
1. Builds an OTel `resource` with service name, component, version, and instance ID.
2. Creates trace, metric, and log exporters connected to the configured OTLP endpoint.
3. Creates SDK providers (trace with batcher, metrics with periodic reader, logs with batch processor).
4. Registers them as global providers via `otel.SetTracerProvider`, `otel.SetMeterProvider`, and `otel.SetTextMapPropagator`.
5. Stores the log provider in the package-level `provider` variable for use by `emitSlogRecord`/`emitLogRecord`.

**Called from:** Every `main.go` entrypoint (same callers as `LoadConfig`):
- `cmd/monofs-server/main.go:125`
- `cmd/monofs-router/main.go:111`
- `cmd/monofs-fetcher/main.go:223`
- `cmd/monofs-search/main.go:55`
- `cmd/monofs-registry/main.go:62`
- `cmd/monofs-pipeline-worker/main.go:54`

**Parameters:**
- `ctx` — Context for exporter connection setup.
- `cfg` — Configuration from `LoadConfig`.

**Returns:**
- `*Handle` — A handle used for later `Shutdown`. If endpoint is empty, `enabled` is `false` and all providers are nil.
- `error` — Non-nil if any exporter fails to connect.

**Implementation details:**
- All three exporters use the same `cfg.Endpoint` address.
- `Insecure` mode (default `true`) enables plaintext gRPC to the collector.
- Metric reader interval is set from `cfg.MetricInterval` (default 15s).
- Writing to `provider` is protected by `providerMu.Lock()`.
- The trace propagator is configured with both `TraceContext` and `Baggage` propagation formats.

---

### `func (h *Handle) Enabled() bool`

```go
func (h *Handle) Enabled() bool
```

**What it does:** Reports whether telemetry was successfully initialized (non-nil handle with `enabled == true`).

**Called from:** Every `main.go` entrypoint, typically in `if` guards before wrapping slog handlers or emitting info messages. Examples:
- `cmd/monofs-server/main.go:130,156,161`
- `cmd/monofs-router/main.go:116,142,147,443`
- etc.

**Returns:** `true` if the handle is non-nil and telemetry is active.

---

### `func (h *Handle) Shutdown(ctx context.Context) error`

```go
func (h *Handle) Shutdown(ctx context.Context) error
```

**What it does:** Gracefully shuts down all three OTel providers (logs, metrics, traces) and clears the package-level `provider` reference if it matches this handle.

**Called from:** Every `main.go` entrypoint during graceful shutdown (deferred or in signal handler):
- `cmd/monofs-server/main.go:134`
- `cmd/monofs-router/main.go:120`
- `cmd/monofs-fetcher/main.go:232`
- `cmd/monofs-search/main.go:64`
- `cmd/monofs-registry/main.go:70`
- `cmd/monofs-pipeline-worker/main.go:62`

**Parameters:**
- `ctx` — Context with deadline for the shutdown operation.

**Returns:** Combined errors from all three provider shutdowns (via `errors.Join`), or `nil`.

**Implementation details:**
- No-ops if `h` is nil or telemetry is not enabled.
- Clears the package-level `provider` only if `h.logs` matches the current value (to prevent one handle's shutdown from clearing another's provider).
- Shuts down logs first, then metrics, then traces.
- Uses `errors.Join` to aggregate all errors — callers should check for nil, not a specific error type.
- Providers are **not** nilled on the handle struct after shutdown (enabled remains true), so repeated calls will attempt re-shutdown on nil providers (which is safe for SDK providers).

---

### `func WrapSlogHandler(base slog.Handler, scope string) slog.Handler`

```go
func WrapSlogHandler(base slog.Handler, scope string) slog.Handler
```

**What it does:** Creates an `slogHandler` that wraps the given base handler, bridging all log records into both the base handler and the OTel log pipeline.

**Called from:** Every `main.go` entrypoint, conditionally when telemetry is enabled:
- `cmd/monofs-server/main.go:157`
- `cmd/monofs-router/main.go:143`
- `cmd/monofs-fetcher/main.go:257`
- `cmd/monofs-search/main.go:79`
- `cmd/monofs-registry/main.go:74`
- `cmd/monofs-pipeline-worker/main.go:66`

**Parameters:**
- `base` — The underlying slog handler (e.g. a JSON handler writing to stderr).
- `scope` — OTel logger scope name (e.g. `"monofs/server"`).

**Returns:** A new `slog.Handler` that forwards to `base` and emits OTel logs under the given scope.

---

### `func EmitInfo(ctx context.Context, scope, message string)`

```go
func EmitInfo(ctx context.Context, scope, message string)
```

**What it does:** Emits an info-level log record directly to the OTel log pipeline, bypassing slog. Used for telemetry lifecycle messages (e.g. "telemetry enabled").

**Called from:**
- `cmd/monofs-server/main.go:162`
- `cmd/monofs-router/main.go:148`
- `cmd/monofs-fetcher/main.go:262`
- `cmd/monofs-search/main.go:83`

**Parameters:**
- `ctx` — Context (carries no span for lifecycle messages, typically `context.Background()`).
- `scope` — OTel logger scope.
- `message` — The log message.

**Implementation details:** Delegates to `emitLogRecord` with `apilog.SeverityInfo`. No-ops if no log provider is active (global `provider` is nil). Does not go through slog at all.

---

### `func emitSlogRecord(ctx context.Context, scope string, record slog.Record)`

```go
func emitSlogRecord(ctx context.Context, scope string, record slog.Record)
```

**What it does:** Converts a `slog.Record` into an OTel `log.Record` and emits it through the active log provider. This is the bridge from slog to OTel for all slog-originated messages.

**Called from:** `slogHandler.Handle` (`slog_handler.go:25`).

**Parameters:**
- `ctx` — Context.
- `scope` — OTel logger scope.
- `record` — The (already enriched) slog record.

**Implementation details:**
- Reads the package-level `provider` under `RLock`. No-ops silently if nil.
- Maps slog attributes to OTel key-value pairs via `slogAttrToLogKV`.
- Timestamp is set from `record.Time.UTC()`.
- Severity is mapped via `severityForSlogLevel`.
- Severity text is produced via `severityText`.
- Body is set to the slog record's message string.

---

### `func emitLogRecord(ctx context.Context, scope string, severity apilog.Severity, message string)`

```go
func emitLogRecord(ctx context.Context, scope string, severity apilog.Severity, message string)
```

**What it does:** Emits a log record directly to the OTel log pipeline with the given severity and message, bypassing any slog processing. Used for direct OTel log emission (e.g. `EmitInfo`).

**Called from:** `EmitInfo` (`telemetry.go:160`).

**Parameters:**
- `ctx` — Context.
- `scope` — OTel logger scope.
- `severity` — OTel log severity (e.g. `apilog.SeverityInfo`).
- `message` — The log body text.

**Implementation details:**
- Reads the package-level `provider` under `RLock`. No-ops if nil.
- Timestamps the record with `time.Now().UTC()` (unlike `emitSlogRecord` which uses the record's own timestamp).
- No attributes are attached — the record has only a timestamp, severity, and body.

---

### `func slogAttrToLogKV(attr slog.Attr) apilog.KeyValue`

```go
func slogAttrToLogKV(attr slog.Attr) apilog.KeyValue
```

**What it does:** Converts a single `slog.Attr` into an OTel `log.KeyValue`. Resolves the attribute's lazy value and delegates conversion to `slogValueToLogValue`.

**Called from:**
- `emitSlogRecord` (`telemetry.go:178`) — iterates over all slog record attrs.
- `slogValueToLogValue` (`telemetry.go:232`) — recursively for group/nested attrs.

**Parameters:**
- `attr` — The slog attribute to convert.

**Returns:** An OTel key-value pair with the attribute's key and its resolved value converted to an OTel value.

---

### `func slogValueToLogValue(value slog.Value) apilog.Value`

```go
func slogValueToLogValue(value slog.Value) apilog.Value
```

**What it does:** Converts a resolved `slog.Value` to an OTel `log.Value`, handling all slog value kinds.

**Called from:** `slogAttrToLogKV` (`telemetry.go:204`).

**Parameters:**
- `value` — A resolved slog value.

**Returns:** The corresponding OTel value.

**Implementation details (per kind):**

| slog Kind | OTel Value |
|---|---|
| `KindBool` | `BoolValue` |
| `KindDuration` | `StringValue` (formatted via `Duration.String()`) |
| `KindFloat64` | `Float64Value` |
| `KindInt64` | `Int64Value` |
| `KindString` | `StringValue` |
| `KindTime` | `StringValue` (formatted via `RFC3339Nano`) |
| `KindUint64` | `Int64Value` if value fits in `int64`; otherwise `StringValue` of decimal representation. `^uint64(0)>>1` is used as the max safe int64 bound. |
| `KindGroup` | `MapValue` — recursively converts nested attributes via `slogAttrToLogKV`. |
| `KindAny` / other | `StringValue` via `fmt.Sprint(value.Any())`. |

---

### `func buildResourceAttributes(cfg Config) []attribute.KeyValue`

```go
func buildResourceAttributes(cfg Config) []attribute.KeyValue
```

**What it does:** Constructs the OTel resource attributes slice from the configuration.

**Called from:** `Setup` (`telemetry.go:72`).

**Parameters:**
- `cfg` — The telemetry configuration.

**Returns:** A slice of OTel `attribute.KeyValue` containing:
- `service.name` — always set (to `cfg.ServiceName`).
- `monofs.component` — always set (to `cfg.Component`).
- `service.version` — set to `cfg.Component` if non-empty.
- `service.instance.id` — set to `cfg.InstanceID` if non-empty.

---

### `func severityText(severity apilog.Severity) string`

```go
func severityText(severity apilog.Severity) string
```

**What it does:** Maps an OTel log severity enum to its canonical uppercase string representation.

**Called from:**
- `emitSlogRecord` (`telemetry.go:175`) — to set `SeverityText` on emitted records.
- `emitLogRecord` (`telemetry.go:196`) — same purpose.

**Parameters:**
- `severity` — The OTel log severity.

**Returns:** A severity string:

| Severity | Text |
|---|---|
| `SeverityFatal` | `"FATAL"` |
| `SeverityError` | `"ERROR"` |
| `SeverityWarn` | `"WARN"` |
| default (all others) | `"INFO"` |

**Implementation details:** Notably includes `SeverityFatal` for completeness even though `severityForSlogLevel` never maps to it (slog lacks a fatal level).
