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
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/storage/logengine"
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
	maxTemp := flag.String("duckdb-max-temp-size", "50GB", "DuckDB maximum spill size")
	maxConns := flag.Int("duckdb-max-conns", 4, "Maximum DuckDB connections")
	fallback := flag.Bool("fallback", true, "Fall back to the native scanner when DuckDB cannot run a query")
	selfTest := flag.Bool("self-test", false, "Open DuckDB, run a validation query, and exit")
	sqlQuery := flag.String("sql", "", "Run an ad-hoc DuckDB SQL query and exit")
	healthInterval := flag.Duration("health-interval", 30*time.Second, "DuckDB health check interval")
	logLevel := flag.String("log-level", "info", "Log level: debug, info, warn, error")
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

	engine := logengine.New(logengine.NewMockS3Store(*dir), logengine.Config{
		LocalCacheDir: *dir,
		ChunkDuration: 5 * time.Minute,
	})
	defer engine.Close()
	if err := engine.EnableDuckDB(logengine.DuckDBConfig{
		MemoryLimit:          *memoryLimit,
		Threads:              *threads,
		TempDir:              *tempDir,
		MaxTempDirectorySize: *maxTemp,
		MaxOpenConns:         *maxConns,
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
	queryServer := &logQueryServer{engine: engine, logger: logger}
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
		"duckdb_threads", *threads)
	if err := grpcServer.Serve(lis); err != nil {
		logger.Error("serve", "error", err)
		os.Exit(1)
	}
}

type logQueryServer struct {
	pb.UnimplementedMonoFSServer
	engine *logengine.LogEngine
	logger *slog.Logger
}

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

func (s *logQueryServer) QueryLogs(ctx context.Context, req *pb.QueryLogsRequest) (*pb.QueryLogsResponse, error) {
	from, to := queryRange(req.GetFromUnixNano(), req.GetToUnixNano())
	ctx = logQueryContext(ctx, req.GetVisiblePartitions(), req.GetVisiblePartitionsSet())
	records, err := s.engine.QueryLogs(ctx, req.GetQuery(), req.GetService(), from, to, int(req.GetLimit()))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "query logs failed: %v", err)
	}
	data, err := json.Marshal(records)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal logs: %v", err)
	}
	return &pb.QueryLogsResponse{ResultsJson: data}, nil
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
	records, err := s.engine.QueryMetrics(ctx, metricQuery(req), from, to)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "query metrics failed: %v", err)
	}
	data, err := json.Marshal(records)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal metrics: %v", err)
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
		return status.Errorf(codes.Internal, "query metrics failed: %v", err)
	}
	return sender.flush()
}

func (s *logQueryServer) StreamMetricSeries(req *pb.QueryMetricsRequest, stream grpc.ServerStreamingServer[pb.MetricSeriesItem]) error {
	from, to := queryRange(req.GetFromUnixNano(), req.GetToUnixNano())
	return s.engine.StreamMetricSeries(stream.Context(), metricQuery(req), from, to, func(meta logengine.MetricSeriesMeta) error {
		return stream.Send(&pb.MetricSeriesItem{
			MetricName:      meta.MetricName,
			Service:         meta.Service,
			Labels:          meta.Labels,
			MinTimeUnixNano: meta.MinTime.UnixNano(),
			MaxTimeUnixNano: meta.MaxTime.UnixNano(),
		})
	})
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
	records, err := s.engine.QueryTraces(ctx, req.GetTraceId(), req.GetService(), from, to, int(req.GetLimit()))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "query traces failed: %v", err)
	}
	data, err := json.Marshal(records)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal traces: %v", err)
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
		return nil, status.Errorf(codes.Internal, "aggregate telemetry failed: %v", err)
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal aggregate rows: %v", err)
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
