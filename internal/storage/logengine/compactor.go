package logengine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Compaction turns many small ingest chunks into fewer, larger chunks. This
// reduces directory listings, manifest reads, parquet opens, and per-chunk text
// indexes. It is intentionally conservative: only ranges older than the
// configured minimum age are compacted, and a range that already contains a
// compacted chunk is treated as complete.
const (
	defaultCompactionMaxRecords = 250_000
	defaultCompactionMaxBytes   = 64 << 20
	defaultCompactionMinAge     = time.Hour
	maxCompactionCatchUp        = 2 * time.Hour
)

// CompactionOptions bounds the memory used by one output chunk.
type CompactionOptions struct {
	MaxRecords int
	MaxBytes   int
}

// CompactionResult summarizes one CompactRange call.
type CompactionResult struct {
	Signal        Signal
	From          time.Time
	To            time.Time
	InputChunks   int
	OutputChunks  int
	Records       int
	DeletedChunks int
}

// Compactor merges small chunks within a time range.
type Compactor struct {
	store    *CachedStore
	ingester *Ingester
	query    *QueryEngine
	mu       sync.Mutex
}

func newCompactor(store *CachedStore, ingester *Ingester, query *QueryEngine) *Compactor {
	return &Compactor{store: store, ingester: ingester, query: query}
}

// CompactRange compacts the raw chunks overlapping [from, to]. Chunks that were
// already compacted are left alone. The whole contents of each input chunk are
// read, so a chunk that straddles the range boundary is not truncated before
// the source chunk is deleted.
func (c *Compactor) CompactRange(ctx context.Context, signal Signal, from, to time.Time, opts CompactionOptions) (result CompactionResult, err error) {
	started := time.Now()
	result = CompactionResult{Signal: signal, From: from, To: to}
	defer func() {
		label := "ok"
		if err != nil {
			label = "error"
		}
		signalLabel := string(signal)
		logengineCompactionRunsTotal.WithLabelValues(signalLabel, label).Inc()
		logengineCompactionDurationSeconds.WithLabelValues(signalLabel).Observe(time.Since(started).Seconds())
		if err == nil {
			logengineCompactionInputChunksTotal.WithLabelValues(signalLabel).Add(float64(result.InputChunks))
			logengineCompactionOutputChunksTotal.WithLabelValues(signalLabel).Add(float64(result.OutputChunks))
			logengineCompactionRecordsTotal.WithLabelValues(signalLabel).Add(float64(result.Records))
			logengineCompactionDeletedChunksTotal.WithLabelValues(signalLabel).Add(float64(result.DeletedChunks))
		}
	}()
	if from.IsZero() || to.IsZero() || !to.After(from) {
		return result, fmt.Errorf("compaction range must be a non-empty [from, to]")
	}
	if opts.MaxRecords <= 0 {
		opts.MaxRecords = defaultCompactionMaxRecords
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = defaultCompactionMaxBytes
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	paths, err := c.store.ListChunkPaths(ctx, signal, from, to)
	if err != nil {
		return result, err
	}
	raw := make([]string, 0, len(paths))
	compacted := make([]string, 0)
	for _, path := range paths {
		if isCompactedChunkPath(path) {
			compacted = append(compacted, path)
			continue
		}
		raw = append(raw, path)
	}
	result.InputChunks = len(raw)
	if len(raw) == 0 {
		return result, nil
	}
	if len(compacted) > 0 {
		// A previous run wrote the compacted output but may have failed before
		// deleting every source chunk. Remove only sources fully covered by the
		// compacted outputs so a partially compacted range cannot lose data.
		for _, path := range raw {
			if !compactedCovers(compacted, path) {
				continue
			}
			if err := c.store.Delete(ctx, chunkStoragePath(signal, path)); err != nil {
				return result, err
			}
			result.DeletedChunks++
		}
		return result, nil
	}
	if len(raw) < 2 {
		return result, nil
	}

	observer := beginQueryPathObservation(signal)
	defer observer.finish()

	outputSeq := 0
	flushSnapshot := func(records any, minTime, maxTime time.Time) error {
		if minTime.IsZero() || maxTime.IsZero() {
			return nil
		}
		chunkID := fmt.Sprintf("compact-%d-%d-%d", minTime.UTC().UnixNano(), maxTime.UTC().UnixNano(), outputSeq)
		outputSeq++
		switch typed := records.(type) {
		case []LogRecord:
			return c.ingester.FlushChunk(ctx, SignalLogs, chunkID, typed, nil, nil)
		case []MetricRecord:
			return c.ingester.FlushChunk(ctx, SignalMetrics, chunkID, nil, typed, nil)
		case []SpanRecord:
			return c.ingester.FlushChunk(ctx, SignalTraces, chunkID, nil, nil, typed)
		default:
			return fmt.Errorf("unsupported compaction record type %T", records)
		}
	}

	switch signal {
	case SignalLogs:
		acc := newCompactAccumulator[LogRecord](
			opts,
			func(record LogRecord) time.Time { return record.Timestamp },
			estimateLogRecordSize,
			flushSnapshot,
		)
		for _, path := range raw {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			if err := c.query.scanLogParquet(ctx, chunkDataPath(signal, path), nil, compiledLogQuery{}, time.Time{}, time.Time{}, observer, func(record LogRecord) error {
				result.Records++
				return acc.Add(record)
			}); err != nil {
				if errors.Is(err, ErrGhostChunk) {
					continue
				}
				return result, err
			}
		}
		if err := acc.Flush(); err != nil {
			return result, err
		}
	case SignalMetrics:
		acc := newCompactAccumulator[MetricRecord](
			opts,
			func(record MetricRecord) time.Time { return record.Timestamp },
			estimateMetricRecordSize,
			flushSnapshot,
		)
		for _, path := range raw {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			if err := c.query.scanMetricParquet(ctx, chunkDataPath(signal, path), MetricQuery{}, nil, time.Time{}, time.Time{}, observer, func(record MetricRecord) error {
				result.Records++
				return acc.Add(record)
			}); err != nil {
				if errors.Is(err, ErrGhostChunk) {
					continue
				}
				return result, err
			}
		}
		if err := acc.Flush(); err != nil {
			return result, err
		}
	case SignalTraces:
		acc := newCompactAccumulator[SpanRecord](
			opts,
			func(record SpanRecord) time.Time { return record.Timestamp },
			estimateSpanRecordSize,
			flushSnapshot,
		)
		for _, path := range raw {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			if err := c.query.scanTraceParquet(ctx, chunkDataPath(signal, path), "", "", time.Time{}, time.Time{}, observer, func(record SpanRecord) error {
				result.Records++
				return acc.Add(record)
			}); err != nil {
				if errors.Is(err, ErrGhostChunk) {
					continue
				}
				return result, err
			}
		}
		if err := acc.Flush(); err != nil {
			return result, err
		}
	default:
		return result, fmt.Errorf("unknown signal: %s", signal)
	}

	result.OutputChunks = outputSeq
	for _, path := range raw {
		if err := c.store.Delete(ctx, chunkStoragePath(signal, path)); err != nil {
			return result, err
		}
		result.DeletedChunks++
	}
	return result, nil
}

type compactAccumulator[T any] struct {
	records   []T
	bytes     int
	minTime   time.Time
	maxTime   time.Time
	bucket    time.Time
	maxRecord int
	maxBytes  int
	timestamp func(T) time.Time
	size      func(T) int
	flush     func(any, time.Time, time.Time) error
}

func newCompactAccumulator[T any](opts CompactionOptions, timestamp func(T) time.Time, size func(T) int, flush func(any, time.Time, time.Time) error) *compactAccumulator[T] {
	return &compactAccumulator[T]{
		maxRecord: opts.MaxRecords,
		maxBytes:  opts.MaxBytes,
		timestamp: timestamp,
		size:      size,
		flush:     flush,
	}
}

func (a *compactAccumulator[T]) Add(record T) error {
	ts := a.timestamp(record)
	bucket := chunkBucketStart(ts)
	if !a.minTime.IsZero() && !bucket.Equal(a.bucket) {
		if err := a.Flush(); err != nil {
			return err
		}
	}
	if a.minTime.IsZero() {
		a.minTime = ts
		a.maxTime = ts
		a.bucket = bucket
	}
	if ts.Before(a.minTime) {
		a.minTime = ts
	}
	if ts.After(a.maxTime) {
		a.maxTime = ts
	}
	a.records = append(a.records, record)
	a.bytes += a.size(record)
	if len(a.records) >= a.maxRecord || a.bytes >= a.maxBytes {
		return a.Flush()
	}
	return nil
}

func (a *compactAccumulator[T]) Flush() error {
	if len(a.records) == 0 {
		return nil
	}
	records := a.records
	minTime, maxTime := a.minTime, a.maxTime
	a.records = nil
	a.bytes = 0
	a.minTime = time.Time{}
	a.maxTime = time.Time{}
	a.bucket = time.Time{}
	return a.flush(records, minTime, maxTime)
}

func chunkDataPath(signal Signal, chunkPath string) string {
	return filepath.Join("chunks", string(signal), chunkPath, "data.parquet")
}

func chunkStoragePath(signal Signal, chunkPath string) string {
	return filepath.Join("chunks", string(signal), chunkPath)
}

func isCompactedChunkPath(path string) bool {
	return strings.HasPrefix(filepath.Base(path), "compact-")
}

func compactedCovers(compacted []string, raw string) bool {
	rawMin, rawMax, rawOK := ParseChunkTimeRangeFromID(raw)
	if !rawOK {
		return false
	}
	type interval struct{ min, max time.Time }
	intervals := make([]interval, 0, len(compacted))
	for _, path := range compacted {
		minTime, maxTime, ok := ParseChunkTimeRangeFromID(path)
		if !ok {
			continue
		}
		intervals = append(intervals, interval{min: minTime, max: maxTime})
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].min.Before(intervals[j].min) })
	cursor := rawMin
	for _, item := range intervals {
		if item.max.Before(cursor) {
			continue
		}
		if item.min.After(cursor) {
			return false
		}
		if item.max.After(cursor) {
			cursor = item.max
		}
		if !cursor.Before(rawMax) {
			return true
		}
	}
	return false
}

func estimateLogRecordSize(record LogRecord) int {
	size := len(record.Level) + len(record.Service) + len(record.TraceID) + len(record.SpanID) + len(record.RawMessage) + 96
	for key, value := range record.Labels {
		size += len(key) + len(value)
	}
	return size
}

func estimateMetricRecordSize(record MetricRecord) int {
	size := len(record.Service) + len(record.MetricName) + 80
	for key, value := range record.Labels {
		size += len(key) + len(value)
	}
	return size
}

func estimateSpanRecordSize(record SpanRecord) int {
	size := len(record.TraceID) + len(record.SpanID) + len(record.ParentSpanID) +
		len(record.Service) + len(record.Name) + len(record.StatusCode) + 96
	for key, value := range record.Attributes {
		size += len(key) + len(value)
	}
	return size
}

// CompactRange compacts chunks for one signal.
func (e *LogEngine) CompactRange(ctx context.Context, signal Signal, from, to time.Time) (CompactionResult, error) {
	return e.compactor.CompactRange(ctx, signal, from, to, compactionOptions(signal))
}

// RunCompactor starts a background loop that compacts complete hourly windows.
// Only windows whose end is at least minAge in the past are touched. A small
// backlog is processed on every tick so a restart does not strand the most
// recent chunks forever.
func (e *LogEngine) RunCompactor(ctx context.Context, interval, minAge time.Duration) {
	if interval <= 0 {
		return
	}
	if minAge <= 0 {
		minAge = defaultCompactionMinAge
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		e.compactRecent(ctx, minAge)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				e.compactRecent(ctx, minAge)
			}
		}
	}()
}

func (e *LogEngine) compactRecent(ctx context.Context, minAge time.Duration) {
	signals := []Signal{SignalLogs, SignalMetrics, SignalTraces}
	for _, signal := range signals {
		window := compactionWindow(signal)
		end := time.Now().UTC().Add(-minAge).Truncate(window)
		opts := compactionOptions(signal)
		for offset := time.Duration(0); offset < maxCompactionCatchUp; offset += window {
			to := end.Add(-offset)
			from := to.Add(-window)
			if ctx.Err() != nil {
				return
			}
			result, err := e.compactor.CompactRange(ctx, signal, from, to, opts)
			if err != nil {
				log.Printf("logengine compaction failed signal=%s range=%s..%s: %v", signal, from.Format(time.RFC3339), to.Format(time.RFC3339), err)
				continue
			}
			if result.OutputChunks > 0 {
				log.Printf("logengine compacted signal=%s range=%s..%s inputs=%d outputs=%d records=%d",
					signal, from.Format(time.RFC3339), to.Format(time.RFC3339),
					result.InputChunks, result.OutputChunks, result.Records)
			}
		}
	}
}

func compactionWindow(signal Signal) time.Duration {
	switch signal {
	case SignalMetrics:
		return 15 * time.Minute
	case SignalTraces:
		return 30 * time.Minute
	default:
		return time.Hour
	}
}

func compactionOptions(signal Signal) CompactionOptions {
	if signal == SignalMetrics {
		return CompactionOptions{
			MaxRecords: 2_000_000,
			MaxBytes:   256 << 20,
		}
	}
	return CompactionOptions{
		MaxRecords: defaultCompactionMaxRecords,
		MaxBytes:   defaultCompactionMaxBytes,
	}
}
