// Command monofs-logquery is the DuckDB query sidecar for a MonoFS storage
// node. It reads the node's logengine Parquet chunks directly and exposes the
// same telemetry query RPCs as monofs-server, so the router, UI and Doctor see
// one consistent API whether the query runs natively or in DuckDB.
//
// Build it with CGO and the duckdb tag:
//
//	CGO_ENABLED=1 go build -tags duckdb -o bin/monofs-logquery ./cmd/monofs-logquery
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/storage/logengine"
	"github.com/radryc/monofs/internal/telemetrycrypto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

func main() {
	addr := flag.String("addr", ":9101", "Sidecar listen address")
	dir := flag.String("logengine-dir", "", "Logengine data directory (required)")
	memoryLimit := flag.String("duckdb-memory-limit", "2GB", "DuckDB memory limit")
	threads := flag.Int("duckdb-threads", 8, "DuckDB worker threads")
	tempDir := flag.String("duckdb-temp-dir", "", "DuckDB spill directory (defaults to <dir>/duckdb-tmp)")
	maxTemp := flag.String("duckdb-max-temp-size", "20GB", "DuckDB maximum spill size")
	maxConns := flag.Int("duckdb-max-conns", 2, "Maximum DuckDB connections")
	maxConcurrent := flag.Int("duckdb-max-concurrent-queries", 1, "Maximum concurrent DuckDB queries")
	maxMetricSeries := flag.Int("max-metric-series", 200000, "Maximum distinct metric series merged by a series scan")
	queryMode := flag.String("query-mode", "auto", "Telemetry query engine: auto, native, or duckdb")
	nativeMaxLimit := flag.Int("native-max-limit", 500, "Auto mode: largest result limit routed to the native scanner")
	nativeMaxRange := flag.Duration("native-max-range", 6*time.Hour, "Auto mode: largest time range routed to the native scanner")
	duckQueueThreshold := flag.Int("duckdb-queue-threshold", 2, "Auto mode: DuckDB queue depth above which small queries prefer native")
	fallback := flag.Bool("fallback", true, "Fall back to the native scanner when DuckDB cannot run a query")
	isolateSQL := flag.Bool("isolate-sql-queries", false, "Run DuckDB SQL console queries in a short-lived child process")
	isolateTimeout := flag.Duration("isolate-sql-timeout", 2*time.Minute, "Timeout for an isolated SQL query")
	execSQLRequest := flag.Bool("exec-sql-request", false, "internal: execute one SQL request from stdin and exit")
	selfTest := flag.Bool("self-test", false, "Open DuckDB, run a validation query, and exit")
	sqlQuery := flag.String("sql", "", "Run an ad-hoc DuckDB SQL query and exit")
	healthInterval := flag.Duration("health-interval", 30*time.Second, "DuckDB health check interval")
	logLevel := flag.String("log-level", "info", "Log level: debug, info, warn, error")
	telemetryEncryption := flag.String("telemetry-encryption", strings.TrimSpace(os.Getenv("MONOFS_TELEMETRY_ENCRYPTION")), "Telemetry encryption: off, read, or envelope (default off)")
	telemetryEncryptionMode := flag.String("telemetry-encryption-mode", strings.TrimSpace(os.Getenv("MONOFS_TELEMETRY_ENCRYPTION_MODE")), "Telemetry encryption read policy: observe or enforce (default observe)")
	telemetryEncryptionKeysFile := flag.String("telemetry-encryption-keys-file", strings.TrimSpace(os.Getenv("MONOFS_TELEMETRY_ENCRYPTION_KEYS_FILE")), "Path to the telemetry encryption key file")
	telemetryEncryptionKeysJSON := flag.String("telemetry-encryption-keys-json", strings.TrimSpace(os.Getenv("MONOFS_TELEMETRY_ENCRYPTION_KEYS_JSON")), "Inline telemetry encryption key JSON")
	telemetryEncryptionTenant := flag.String("telemetry-encryption-tenant", strings.TrimSpace(os.Getenv("MONOFS_TELEMETRY_ENCRYPTION_TENANT")), "Tenant used for telemetry encryption keys (default default)")
	flag.Parse()

	level := slog.LevelInfo
	switch strings.ToLower(*logLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})).With("component", "monofs-logquery")

	if strings.TrimSpace(*dir) == "" {
		fmt.Fprintln(os.Stderr, "--logengine-dir is required")
		flag.Usage()
		os.Exit(2)
	}
	if err := os.MkdirAll(*dir, 0755); err != nil {
		logger.Error("create logengine directory", "error", err)
		os.Exit(1)
	}

	encryptionSetting, err := telemetrycrypto.ParseSetting(*telemetryEncryption)
	if err != nil {
		logger.Error("invalid telemetry encryption setting", "value", *telemetryEncryption, "error", err)
		os.Exit(1)
	}
	var encryption *telemetrycrypto.Provider
	if encryptionSetting.Write || encryptionSetting.Read {
		encryptionMode := telemetrycrypto.Mode(strings.ToLower(strings.TrimSpace(*telemetryEncryptionMode)))
		if encryptionMode == "" {
			encryptionMode = telemetrycrypto.ModeObserve
		}
		encryption, err = telemetrycrypto.LoadProvider(telemetrycrypto.Config{
			Enabled:     encryptionSetting.Write,
			DecryptOnly: encryptionSetting.Read && !encryptionSetting.Write,
			Mode:        encryptionMode,
			Tenant:      strings.TrimSpace(*telemetryEncryptionTenant),
			KeysFile:    *telemetryEncryptionKeysFile,
			KeysJSON:    *telemetryEncryptionKeysJSON,
		})
		if err != nil {
			logger.Error("invalid telemetry encryption configuration", "error", err)
			os.Exit(1)
		}
		logger.Info("telemetry encryption configured",
			"write", encryptionSetting.Write,
			"read", encryptionSetting.Read,
			"mode", encryption.Mode(),
			"tenant", encryption.Tenant(),
			"root_key_id", encryption.RootKeyID())
	}

	mode := logengine.ParseQueryMode(*queryMode)
	logger.Info("telemetry query routing configured",
		"mode", mode,
		"native_max_limit", *nativeMaxLimit,
		"native_max_range", nativeMaxRange.String(),
		"duckdb_queue_threshold", *duckQueueThreshold)

	engine := logengine.New(logengine.NewMockS3Store(*dir), logengine.Config{
		LocalCacheDir:      *dir,
		ChunkDuration:      5 * time.Minute,
		MaxMetricSeries:    *maxMetricSeries,
		QueryMode:          mode,
		NativeMaxLimit:     *nativeMaxLimit,
		NativeMaxRange:     *nativeMaxRange,
		DuckQueueThreshold: *duckQueueThreshold,
		Encryption:         encryption,
	})
	defer engine.Close()
	if err := engine.EnableDuckDB(logengine.DuckDBConfig{
		MemoryLimit:          *memoryLimit,
		Threads:              *threads,
		TempDir:              *tempDir,
		MaxTempDirectorySize: *maxTemp,
		MaxOpenConns:         *maxConns,
		MaxConcurrentQueries: *maxConcurrent,
		FallbackOnError:      *fallback,
	}); err != nil {
		logger.Error("DuckDB is not available in this build; rebuild with CGO_ENABLED=1 -tags duckdb", "error", err)
		os.Exit(1)
	}
	checkCtx, checkCancel := context.WithTimeout(context.Background(), 30*time.Second)
	checkErr := engine.CheckDuckDB(checkCtx)
	checkCancel()
	if checkErr != nil {
		logger.Error("DuckDB self-test failed", "error", checkErr)
		os.Exit(1)
	}
	if *selfTest {
		logger.Info("DuckDB self-test passed")
		return
	}
	if *execSQLRequest {
		if err := runIsolatedSQLRequest(context.Background(), engine); err != nil {
			logger.Error("isolated SQL request failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if strings.TrimSpace(*sqlQuery) != "" {
		queryCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := runSQL(queryCtx, engine, *sqlQuery); err != nil {
			logger.Error("SQL query failed", "error", err)
			os.Exit(1)
		}
		return
	}

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		logger.Error("listen", "addr", *addr, "error", err)
		os.Exit(1)
	}
	grpcServer := grpc.NewServer(
		grpc.MaxRecvMsgSize(64<<20),
		grpc.MaxSendMsgSize(256<<20),
	)
	executable, _ := os.Executable()
	encryptionEnv := []string{
		"MONOFS_TELEMETRY_ENCRYPTION=" + strings.TrimSpace(*telemetryEncryption),
		"MONOFS_TELEMETRY_ENCRYPTION_MODE=" + strings.TrimSpace(*telemetryEncryptionMode),
		"MONOFS_TELEMETRY_ENCRYPTION_TENANT=" + strings.TrimSpace(*telemetryEncryptionTenant),
	}
	if strings.TrimSpace(*telemetryEncryptionKeysFile) != "" {
		encryptionEnv = append(encryptionEnv, "MONOFS_TELEMETRY_ENCRYPTION_KEYS_FILE="+strings.TrimSpace(*telemetryEncryptionKeysFile))
	}
	if strings.TrimSpace(*telemetryEncryptionKeysJSON) != "" {
		encryptionEnv = append(encryptionEnv, "MONOFS_TELEMETRY_ENCRYPTION_KEYS_JSON="+strings.TrimSpace(*telemetryEncryptionKeysJSON))
	}
	queryServer := &logQueryServer{
		engine:         engine,
		logger:         logger,
		isolateSQL:     *isolateSQL,
		isolateTimeout: *isolateTimeout,
		executable:     executable,
		isolateArgs:    isolatedSQLChildArgs(*dir, *memoryLimit, *tempDir, *maxTemp, *threads, *maxConns, *fallback, *logLevel),
		encryptionEnv:  encryptionEnv,
	}
	pb.RegisterMonoFSServer(grpcServer, queryServer)
	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
		grpcServer.GracefulStop()
	}()
	if *healthInterval > 0 {
		go func() {
			ticker := time.NewTicker(*healthInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
					err := engine.CheckDuckDB(checkCtx)
					cancel()
					if err != nil {
						logger.Warn("DuckDB health check failed", "error", err)
						healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
						continue
					}
					healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
				}
			}
		}()
	}

	logger.Info("logquery sidecar listening",
		"addr", *addr,
		"dir", *dir,
		"duckdb_memory_limit", *memoryLimit,
		"duckdb_threads", *threads,
		"duckdb_max_conns", *maxConns,
		"duckdb_max_concurrent_queries", *maxConcurrent,
		"max_metric_series", *maxMetricSeries,
		"isolate_sql_queries", *isolateSQL)
	if err := grpcServer.Serve(lis); err != nil {
		logger.Error("serve", "error", err)
		os.Exit(1)
	}
}

type logQueryServer struct {
	pb.UnimplementedMonoFSServer
	engine *logengine.LogEngine
	logger *slog.Logger

	isolateSQL     bool
	isolateTimeout time.Duration
	executable     string
	isolateArgs    []string
	encryptionEnv  []string
}

const (
	// Unary RPCs are retained for compatibility, but their responses are
	// materialized and JSON-encoded in one shot. Streaming RPCs have no such
	// limit. These budgets keep a legacy caller from turning a large scan into
	// a second, unbounded copy of the result set.
	maxUnaryResultRecords = 20_000
	maxUnaryResultBytes   = 32 << 20
)

var errUnaryResultTooLarge = errors.New("unary result exceeds the response budget")

func queryRange(fromNS, toNS int64) (time.Time, time.Time) {
	from := time.Unix(0, fromNS).UTC()
	to := time.Unix(0, toNS).UTC()
	if fromNS == 0 {
		from = time.Time{}
	}
	if toNS == 0 {
		to = time.Time{}
	}
	return from, to
}

// queryStatusError preserves bounded-cardinality failures as ResourceExhausted
// so callers can narrow the query instead of retrying an internal error.
func queryStatusError(message string, err error) error {
	if errors.Is(err, logengine.ErrMetricSeriesLimit) || errors.Is(err, errUnaryResultTooLarge) {
		return status.Errorf(codes.ResourceExhausted, "%s: %v", message, err)
	}
	return status.Errorf(codes.Internal, "%s: %v", message, err)
}

// collectUnary buffers at most maxUnaryResultRecords. Callers that need more
// must use the corresponding streaming RPC.
func collectUnary[T any](stream func(func(T) error) error) ([]T, error) {
	records := make([]T, 0)
	if err := stream(func(record T) error {
		if len(records) >= maxUnaryResultRecords {
			return errUnaryResultTooLarge
		}
		records = append(records, record)
		return nil
	}); err != nil {
		return nil, err
	}
	return records, nil
}

func marshalUnary(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(data) > maxUnaryResultBytes {
		return nil, errUnaryResultTooLarge
	}
	return data, nil
}

func (s *logQueryServer) QueryLogs(ctx context.Context, req *pb.QueryLogsRequest) (*pb.QueryLogsResponse, error) {
	from, to := queryRange(req.GetFromUnixNano(), req.GetToUnixNano())
	ctx = logQueryContext(ctx, req.GetVisiblePartitions(), req.GetVisiblePartitionsSet())
	records, err := collectUnary(func(yield func(logengine.LogRecord) error) error {
		return s.engine.StreamLogs(ctx, req.GetQuery(), req.GetService(), from, to, int(req.GetLimit()), yield)
	})
	if err != nil {
		return nil, queryStatusError("query logs failed", err)
	}
	data, err := marshalUnary(records)
	if err != nil {
		return nil, queryStatusError("marshal logs failed", err)
	}
	return &pb.QueryLogsResponse{ResultsJson: data}, nil
}

func (s *logQueryServer) QueryDuckDBSQL(ctx context.Context, req *pb.QueryDuckDBSQLRequest) (*pb.QueryDuckDBSQLResponse, error) {
	if s != nil && s.isolateSQL {
		return s.queryDuckDBSQLIsolated(ctx, req)
	}
	from, to := queryRange(req.GetFromUnixNano(), req.GetToUnixNano())
	scope := logengine.QueryScope{}
	if req.GetVisiblePartitionsSet() {
		scope = logengine.QueryScope{
			Partitions: append([]string(nil), req.GetVisiblePartitions()...),
			Restricted: true,
		}
	}
	result, err := s.engine.QueryDuckDBSQL(ctx, req.GetDataset(), req.GetQuery(), from, to, int(req.GetLimit()), scope)
	if err != nil {
		if errors.Is(err, logengine.ErrDuckDBUnavailable) {
			return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "query duckdb sql failed: %v", err)
	}
	data, err := marshalUnary(result)
	if err != nil {
		return nil, queryStatusError("marshal duckdb sql failed", err)
	}
	return &pb.QueryDuckDBSQLResponse{ResultsJson: string(data)}, nil
}

// isolatedSQLRequest is the wire format between the sidecar and its
// short-lived SQL worker. Queries arrive on stdin so they never appear in the
// process table.
type isolatedSQLRequest struct {
	Dataset              string   `json:"dataset"`
	Query                string   `json:"query"`
	FromUnixNano         int64    `json:"from_unix_nano,omitempty"`
	ToUnixNano           int64    `json:"to_unix_nano,omitempty"`
	Limit                int32    `json:"limit,omitempty"`
	VisiblePartitions    []string `json:"visible_partitions,omitempty"`
	VisiblePartitionsSet bool     `json:"visible_partitions_set,omitempty"`
}

type isolatedSQLResponse struct {
	ResultsJSON string `json:"results_json,omitempty"`
	Error       string `json:"error,omitempty"`
}

func (s *logQueryServer) queryDuckDBSQLIsolated(ctx context.Context, req *pb.QueryDuckDBSQLRequest) (*pb.QueryDuckDBSQLResponse, error) {
	if s == nil || s.executable == "" {
		return nil, status.Error(codes.FailedPrecondition, "isolated SQL execution is not configured")
	}
	payload, err := json.Marshal(isolatedSQLRequest{
		Dataset:              req.GetDataset(),
		Query:                req.GetQuery(),
		FromUnixNano:         req.GetFromUnixNano(),
		ToUnixNano:           req.GetToUnixNano(),
		Limit:                req.GetLimit(),
		VisiblePartitions:    append([]string(nil), req.GetVisiblePartitions()...),
		VisiblePartitionsSet: req.GetVisiblePartitionsSet(),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal isolated sql request: %v", err)
	}

	timeout := s.isolateTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	queryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := append(append([]string(nil), s.isolateArgs...), "--exec-sql-request")
	cmd := exec.CommandContext(queryCtx, s.executable, args...)
	cmd.Env = mergeEnv(os.Environ(), s.encryptionEnv)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	var response isolatedSQLResponse
	decodeErr := json.Unmarshal(stdout.Bytes(), &response)
	if decodeErr != nil {
		if errors.Is(queryCtx.Err(), context.DeadlineExceeded) {
			return nil, status.Errorf(codes.DeadlineExceeded, "isolated sql query exceeded %s", timeout)
		}
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 2048 {
			detail = detail[:2048]
		}
		return nil, status.Errorf(codes.Internal, "isolated sql query failed: %v: %s", runErr, detail)
	}
	if response.Error != "" {
		return nil, status.Errorf(codes.Internal, "isolated sql query failed: %s", response.Error)
	}
	if runErr != nil {
		return nil, status.Errorf(codes.Internal, "isolated sql query failed: %v", runErr)
	}
	return &pb.QueryDuckDBSQLResponse{ResultsJson: response.ResultsJSON}, nil
}

// runIsolatedSQLRequest is the child side of queryDuckDBSQLIsolated. It reads a
// single request from stdin, writes one JSON response to stdout, and exits.
func runIsolatedSQLRequest(ctx context.Context, engine *logengine.LogEngine) error {
	var request isolatedSQLRequest
	if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil {
		return writeIsolatedSQLResponse(isolatedSQLResponse{Error: "decode request: " + err.Error()})
	}
	from, to := queryRange(request.FromUnixNano, request.ToUnixNano)
	scope := logengine.QueryScope{}
	if request.VisiblePartitionsSet {
		scope = logengine.QueryScope{
			Partitions: append([]string(nil), request.VisiblePartitions...),
			Restricted: true,
		}
	}
	result, err := engine.QueryDuckDBSQL(ctx, request.Dataset, request.Query, from, to, int(request.Limit), scope)
	if err != nil {
		return writeIsolatedSQLResponse(isolatedSQLResponse{Error: err.Error()})
	}
	data, err := json.Marshal(result)
	if err != nil {
		return writeIsolatedSQLResponse(isolatedSQLResponse{Error: "marshal result: " + err.Error()})
	}
	return writeIsolatedSQLResponse(isolatedSQLResponse{ResultsJSON: string(data)})
}

func writeIsolatedSQLResponse(response isolatedSQLResponse) error {
	encoder := json.NewEncoder(os.Stdout)
	if err := encoder.Encode(response); err != nil {
		return err
	}
	if response.Error != "" {
		return errors.New(response.Error)
	}
	return nil
}

func isolatedSQLChildArgs(dir, memoryLimit, tempDir, maxTemp string, threads, maxConns int, fallback bool, logLevel string) []string {
	args := []string{
		"--logengine-dir=" + dir,
		"--duckdb-memory-limit=" + memoryLimit,
		"--duckdb-threads=" + strconv.Itoa(threads),
		"--duckdb-max-conns=" + strconv.Itoa(maxConns),
		"--duckdb-max-concurrent-queries=1",
		"--duckdb-max-temp-size=" + maxTemp,
		"--fallback=" + strconv.FormatBool(fallback),
		"--log-level=" + logLevel,
		"--health-interval=0",
	}
	if strings.TrimSpace(tempDir) != "" {
		args = append(args, "--duckdb-temp-dir="+tempDir)
	}
	return args
}

// mergeEnv overlays overrides on top of base while avoiding duplicate keys.
// It lets the isolated SQL child inherit the exact telemetry encryption config
// even when it was supplied via flags rather than the parent environment.
func mergeEnv(base, overrides []string) []string {
	values := make(map[string]string, len(base)+len(overrides))
	order := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, seen := values[key]; !seen {
			order = append(order, key)
		}
		values[key] = value
	}
	for _, entry := range overrides {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, seen := values[key]; !seen {
			order = append(order, key)
		}
		values[key] = value
	}
	out := make([]string, 0, len(order))
	for _, key := range order {
		out = append(out, key+"="+values[key])
	}
	return out
}

func (s *logQueryServer) StreamQueryLogs(req *pb.QueryLogsRequest, stream grpc.ServerStreamingServer[pb.QueryResultItem]) error {
	from, to := queryRange(req.GetFromUnixNano(), req.GetToUnixNano())
	ctx := logQueryContext(stream.Context(), req.GetVisiblePartitions(), req.GetVisiblePartitionsSet())
	sender := newResultSender(stream.Send, int(req.GetBatchSize()))
	err := s.engine.StreamLogs(ctx, req.GetQuery(), req.GetService(), from, to, int(req.GetLimit()), func(record logengine.LogRecord) error {
		return sender.add(record)
	})
	if err != nil {
		return status.Errorf(codes.Internal, "query logs failed: %v", err)
	}
	return sender.flush()
}

func (s *logQueryServer) QueryMetrics(ctx context.Context, req *pb.QueryMetricsRequest) (*pb.QueryMetricsResponse, error) {
	from, to := queryRange(req.GetFromUnixNano(), req.GetToUnixNano())
	records, err := collectUnary(func(yield func(logengine.MetricRecord) error) error {
		return s.engine.StreamMetrics(ctx, metricQuery(req), from, to, yield)
	})
	if err != nil {
		return nil, queryStatusError("query metrics failed", err)
	}
	data, err := marshalUnary(records)
	if err != nil {
		return nil, queryStatusError("marshal metrics failed", err)
	}
	return &pb.QueryMetricsResponse{ResultsJson: data}, nil
}

func (s *logQueryServer) StreamQueryMetrics(req *pb.QueryMetricsRequest, stream grpc.ServerStreamingServer[pb.QueryResultItem]) error {
	from, to := queryRange(req.GetFromUnixNano(), req.GetToUnixNano())
	sender := newResultSender(stream.Send, int(req.GetBatchSize()))
	err := s.engine.StreamMetrics(stream.Context(), metricQuery(req), from, to, func(record logengine.MetricRecord) error {
		return sender.add(record)
	})
	if err != nil {
		return queryStatusError("query metrics failed", err)
	}
	return sender.flush()
}

func (s *logQueryServer) StreamMetricSeries(req *pb.QueryMetricsRequest, stream grpc.ServerStreamingServer[pb.MetricSeriesItem]) error {
	from, to := queryRange(req.GetFromUnixNano(), req.GetToUnixNano())
	if err := s.engine.StreamMetricSeries(stream.Context(), metricQuery(req), from, to, func(meta logengine.MetricSeriesMeta) error {
		return stream.Send(&pb.MetricSeriesItem{
			MetricName:      meta.MetricName,
			Service:         meta.Service,
			Labels:          meta.Labels,
			MinTimeUnixNano: meta.MinTime.UnixNano(),
			MaxTimeUnixNano: meta.MaxTime.UnixNano(),
		})
	}); err != nil {
		return queryStatusError("query metric series failed", err)
	}
	return nil
}

func (s *logQueryServer) QueryTraces(ctx context.Context, req *pb.QueryTracesRequest) (*pb.QueryTracesResponse, error) {
	from, to := queryRange(req.GetFromUnixNano(), req.GetToUnixNano())
	ctx = logQueryContext(ctx, req.GetVisiblePartitions(), req.GetVisiblePartitionsSet())
	discovery := strings.TrimSpace(req.GetDiscovery())
	if discovery != "" {
		values, err := s.traceDiscovery(ctx, discovery, req.GetDiscoveryTag(), from, to)
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(values)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "marshal trace discovery: %v", err)
		}
		return &pb.QueryTracesResponse{ResultsJson: data}, nil
	}
	records, err := collectUnary(func(yield func(logengine.SpanRecord) error) error {
		return s.engine.StreamTraces(ctx, req.GetTraceId(), req.GetService(), from, to, int(req.GetLimit()), yield)
	})
	if err != nil {
		return nil, queryStatusError("query traces failed", err)
	}
	data, err := marshalUnary(records)
	if err != nil {
		return nil, queryStatusError("marshal traces failed", err)
	}
	return &pb.QueryTracesResponse{ResultsJson: data}, nil
}

func (s *logQueryServer) StreamQueryTraces(req *pb.QueryTracesRequest, stream grpc.ServerStreamingServer[pb.QueryResultItem]) error {
	from, to := queryRange(req.GetFromUnixNano(), req.GetToUnixNano())
	ctx := logQueryContext(stream.Context(), req.GetVisiblePartitions(), req.GetVisiblePartitionsSet())
	discovery := strings.TrimSpace(req.GetDiscovery())
	if discovery != "" {
		values, err := s.traceDiscovery(ctx, discovery, req.GetDiscoveryTag(), from, to)
		if err != nil {
			return err
		}
		for _, value := range values {
			data, _ := json.Marshal(value)
			if err := stream.Send(&pb.QueryResultItem{ItemJson: data}); err != nil {
				return err
			}
		}
		return nil
	}
	sender := newResultSender(stream.Send, int(req.GetBatchSize()))
	err := s.engine.StreamTraces(ctx, req.GetTraceId(), req.GetService(), from, to, int(req.GetLimit()), func(record logengine.SpanRecord) error {
		return sender.add(record)
	})
	if err != nil {
		return status.Errorf(codes.Internal, "query traces failed: %v", err)
	}
	return sender.flush()
}

func (s *logQueryServer) traceDiscovery(ctx context.Context, discovery, tag string, from, to time.Time) ([]string, error) {
	switch discovery {
	case "services":
		return s.engine.DiscoverTraceServices(ctx, from, to)
	case "tags":
		return s.engine.DiscoverTraceTags(ctx, from, to)
	case "tag_values":
		return s.engine.DiscoverTraceTagValues(ctx, from, to, tag)
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unknown trace discovery mode %q", discovery)
	}
}

func metricQuery(req *pb.QueryMetricsRequest) logengine.MetricQuery {
	matchers := make([]logengine.MetricLabelMatcher, 0, len(req.GetLabelMatchers()))
	for _, matcher := range req.GetLabelMatchers() {
		matchers = append(matchers, logengine.MetricLabelMatcher{
			Name:  matcher.GetName(),
			Value: matcher.GetValue(),
			Type:  metricMatcherType(matcher.GetType()),
		})
	}
	return logengine.MetricQuery{
		MetricName:    req.GetMetricName(),
		Service:       req.GetService(),
		LabelMatchers: matchers,
	}
}

func metricMatcherType(value pb.MetricLabelMatcherType) logengine.MetricMatchType {
	switch value {
	case pb.MetricLabelMatcherType_METRIC_LABEL_MATCHER_TYPE_NOT_EQUAL:
		return logengine.MetricMatchNotEqual
	case pb.MetricLabelMatcherType_METRIC_LABEL_MATCHER_TYPE_REGEXP:
		return logengine.MetricMatchRegexp
	case pb.MetricLabelMatcherType_METRIC_LABEL_MATCHER_TYPE_NOT_REGEXP:
		return logengine.MetricMatchNotRegexp
	default:
		return logengine.MetricMatchEqual
	}
}

type resultSender struct {
	send      func(*pb.QueryResultItem) error
	batchSize int
	pending   [][]byte
	mu        sync.Mutex
}

func newResultSender(send func(*pb.QueryResultItem) error, batchSize int) *resultSender {
	if batchSize <= 0 {
		batchSize = 1
	}
	if batchSize > 2048 {
		batchSize = 2048
	}
	return &resultSender{send: send, batchSize: batchSize}
}

func (s *resultSender) add(record any) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, data)
	if len(s.pending) >= s.batchSize {
		return s.flushLocked()
	}
	return nil
}

func (s *resultSender) flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

func (s *resultSender) flushLocked() error {
	if len(s.pending) == 0 {
		return nil
	}
	batch := s.pending
	s.pending = nil
	item := &pb.QueryResultItem{ItemsJson: batch}
	if len(batch) == 1 {
		item.ItemJson = batch[0]
	}
	return s.send(item)
}

func logQueryContext(ctx context.Context, partitions []string, restricted bool) context.Context {
	if !restricted {
		return ctx
	}
	return logengine.WithQueryScope(ctx, logengine.QueryScope{
		Partitions: append([]string(nil), partitions...),
		Restricted: true,
	})
}

func (s *logQueryServer) AggregateTelemetry(ctx context.Context, req *pb.AggregateTelemetryRequest) (*pb.AggregateTelemetryResponse, error) {
	signal, err := aggregateSignal(req.GetSignal())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	from, to := queryRange(req.GetFromUnixNano(), req.GetToUnixNano())
	ctx = logQueryContext(ctx, req.GetVisiblePartitions(), req.GetVisiblePartitionsSet())
	query := logengine.AggregateQuery{
		Signal:      signal,
		Query:       req.GetQuery(),
		Service:     req.GetService(),
		MetricQuery: metricQuery(&pb.QueryMetricsRequest{MetricName: req.GetMetricName(), Service: req.GetService(), LabelMatchers: req.GetLabelMatchers()}),
		TraceID:     req.GetTraceId(),
		From:        from,
		To:          to,
		GroupBy:     append([]string(nil), req.GetGroupBy()...),
		Aggregates:  aggregateSpecs(req.GetAggregates()),
		Partial:     true,
	}
	rows, err := s.engine.Aggregate(ctx, query)
	if err != nil {
		return nil, queryStatusError("aggregate telemetry failed", err)
	}
	data, err := marshalUnary(rows)
	if err != nil {
		return nil, queryStatusError("marshal aggregate rows failed", err)
	}
	return &pb.AggregateTelemetryResponse{RowsJson: data}, nil
}

func aggregateSignal(name string) (logengine.Signal, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "logs", "log":
		return logengine.SignalLogs, nil
	case "metrics", "metric":
		return logengine.SignalMetrics, nil
	case "traces", "trace":
		return logengine.SignalTraces, nil
	default:
		return "", fmt.Errorf("unknown telemetry signal %q", name)
	}
}

func aggregateSpecs(specs []*pb.AggregateSpec) []logengine.AggregateSpec {
	out := make([]logengine.AggregateSpec, 0, len(specs))
	for _, spec := range specs {
		out = append(out, logengine.AggregateSpec{
			Op:    aggregateOp(spec.GetOp()),
			Field: spec.GetField(),
			Alias: spec.GetAlias(),
		})
	}
	return out
}

func aggregateOp(op pb.AggregateOp) logengine.AggregateOp {
	switch op {
	case pb.AggregateOp_AGGREGATE_OP_COUNT:
		return logengine.AggregateCount
	case pb.AggregateOp_AGGREGATE_OP_SUM:
		return logengine.AggregateSum
	case pb.AggregateOp_AGGREGATE_OP_MIN:
		return logengine.AggregateMin
	case pb.AggregateOp_AGGREGATE_OP_MAX:
		return logengine.AggregateMax
	default:
		return logengine.AggregateOp(strings.ToLower(strings.TrimPrefix(op.String(), "AGGREGATE_OP_")))
	}
}

func runSQL(ctx context.Context, engine *logengine.LogEngine, query string) error {
	rows, err := engine.QueryDuckDB(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	fmt.Println(strings.Join(columns, "\t"))
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for idx := range values {
			pointers[idx] = &values[idx]
		}
		if err := rows.Scan(pointers...); err != nil {
			return err
		}
		fields := make([]string, len(values))
		for idx, value := range values {
			if value == nil {
				fields[idx] = ""
				continue
			}
			fields[idx] = fmt.Sprint(value)
		}
		fmt.Println(strings.Join(fields, "\t"))
	}
	return rows.Err()
}
