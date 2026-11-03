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
	defaultCompactionMaxRecords = 1_000_000
	defaultCompactionMaxBytes   = 256 << 20
	defaultCompactionMinAge     = time.Hour
	// maxCompactionCatchUp bounds how far back the background worker looks for
	// uncompacted chunks on each tick. Older backlog can still be compacted with
	// monofs-logengine-compact.
	maxCompactionCatchUp = 24 * time.Hour
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
	store           *CachedStore
	ingester        *Ingester
	query           *QueryEngine
	mu              sync.Mutex
	compactionLevel int

	running       bool
	lastRun       time.Time
	lastResult    CompactionResult
	runs          uint64
	inputChunks   uint64
	outputChunks  uint64
	deletedChunks uint64
	records       uint64
}

// CompactionStats is a snapshot for the node info API and UI.
type CompactionStats struct {
	Running       bool
	LastRun       time.Time
	LastResult    CompactionResult
	Runs          uint64
	InputChunks   uint64
	OutputChunks  uint64
	DeletedChunks uint64
	Records       uint64
	TargetBytes   int64
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
	c.compactionLevel = 1
	c.running = true
	defer func() {
		c.running = false
		c.compactionLevel = 0
		c.lastRun = time.Now().UTC()
		c.lastResult = result
		if err == nil {
			c.runs++
			c.inputChunks += uint64(result.InputChunks)
			c.outputChunks += uint64(result.OutputChunks)
			c.deletedChunks += uint64(result.DeletedChunks)
			c.records += uint64(result.Records)
		}
	}()

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
		// A range that already contains a compacted chunk is considered
		// complete. Do not delete leftover sources here: without a durable
		// compaction manifest we cannot prove every source record was written
		// into the compacted outputs. Leaving duplicates is safer than losing
		// data after a partial run.
		return result, nil
	}
	if len(raw) < 2 {
		return result, nil
	}

	observer := beginQueryPathObservation(signal)
	defer observer.finish()

	outputSeq := 0
	writeCompacted := func(records any, _ time.Time, _ time.Time) error {
		switch typed := records.(type) {
		case []LogRecord:
			return c.writeLogCompacted(ctx, typed, opts.MaxRecords, &outputSeq)
		case []MetricRecord:
			return c.writeMetricCompacted(ctx, typed, opts.MaxRecords, &outputSeq)
		case []SpanRecord:
			return c.writeSpanCompacted(ctx, typed, opts.MaxRecords, &outputSeq)
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
			writeCompacted,
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
			writeCompacted,
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
			writeCompacted,
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

const maxCompactionGroupKeys = 128

func nextCompactedChunkID(minTime, maxTime time.Time, seq *int) string {
	chunkID := fmt.Sprintf("compact-%d-%d-%d", minTime.UTC().UnixNano(), maxTime.UTC().UnixNano(), *seq)
	*seq++
	return chunkID
}

func (c *Compactor) writeLogCompacted(ctx context.Context, records []LogRecord, maxRecords int, seq *int) error {
	if len(records) == 0 {
		return nil
	}
	if maxRecords <= 0 {
		maxRecords = defaultCompactionMaxRecords
	}
	sort.SliceStable(records, func(i, j int) bool {
		leftPartition := recordPartition(records[i].Labels)
		rightPartition := recordPartition(records[j].Labels)
		if leftPartition != rightPartition {
			return leftPartition < rightPartition
		}
		if records[i].Service != records[j].Service {
			return records[i].Service < records[j].Service
		}
		return records[i].Timestamp.Before(records[j].Timestamp)
	})
	groupByService := countLogGroupKeys(records) <= maxCompactionGroupKeys
	for start := 0; start < len(records); {
		end := start + 1
		if groupByService {
			partition := recordPartition(records[start].Labels)
			service := records[start].Service
			for end < len(records) && records[end].Service == service && recordPartition(records[end].Labels) == partition && end-start < maxRecords {
				end++
			}
		} else {
			for end < len(records) && end-start < maxRecords {
				end++
			}
		}
		minTime, maxTime := records[start].Timestamp, records[start].Timestamp
		for _, record := range records[start:end] {
			if record.Timestamp.Before(minTime) {
				minTime = record.Timestamp
			}
			if record.Timestamp.After(maxTime) {
				maxTime = record.Timestamp
			}
		}
		chunkID := nextCompactedChunkID(minTime, maxTime, seq)
		if err := c.ingester.FlushChunkWithOptions(ctx, SignalLogs, chunkID, records[start:end], nil, nil, ChunkWriteOptions{CompactionLevel: c.compactionLevel}); err != nil {
			return err
		}
		start = end
	}
	return nil
}

func (c *Compactor) writeMetricCompacted(ctx context.Context, records []MetricRecord, maxRecords int, seq *int) error {
	if len(records) == 0 {
		return nil
	}
	if maxRecords <= 0 {
		maxRecords = defaultCompactionMaxRecords
	}
	sort.SliceStable(records, func(i, j int) bool {
		leftPartition := recordPartition(records[i].Labels)
		rightPartition := recordPartition(records[j].Labels)
		if leftPartition != rightPartition {
			return leftPartition < rightPartition
		}
		if records[i].MetricName != records[j].MetricName {
			return records[i].MetricName < records[j].MetricName
		}
		return records[i].Timestamp.Before(records[j].Timestamp)
	})
	groupByMetric := countMetricGroupKeys(records) <= maxCompactionGroupKeys
	for start := 0; start < len(records); {
		end := start + 1
		if groupByMetric {
			partition := recordPartition(records[start].Labels)
			name := records[start].MetricName
			for end < len(records) && records[end].MetricName == name && recordPartition(records[end].Labels) == partition && end-start < maxRecords {
				end++
			}
		} else {
			for end < len(records) && end-start < maxRecords {
				end++
			}
		}
		minTime, maxTime := records[start].Timestamp, records[start].Timestamp
		for _, record := range records[start:end] {
			if record.Timestamp.Before(minTime) {
				minTime = record.Timestamp
			}
			if record.Timestamp.After(maxTime) {
				maxTime = record.Timestamp
			}
		}
		chunkID := nextCompactedChunkID(minTime, maxTime, seq)
		if err := c.ingester.FlushChunkWithOptions(ctx, SignalMetrics, chunkID, nil, records[start:end], nil, ChunkWriteOptions{CompactionLevel: c.compactionLevel}); err != nil {
			return err
		}
		start = end
	}
	return nil
}

func (c *Compactor) writeSpanCompacted(ctx context.Context, records []SpanRecord, maxRecords int, seq *int) error {
	if len(records) == 0 {
		return nil
	}
	if maxRecords <= 0 {
		maxRecords = defaultCompactionMaxRecords
	}
	sort.SliceStable(records, func(i, j int) bool {
		leftPartition := recordPartition(records[i].Attributes)
		rightPartition := recordPartition(records[j].Attributes)
		if leftPartition != rightPartition {
			return leftPartition < rightPartition
		}
		if records[i].Service != records[j].Service {
			return records[i].Service < records[j].Service
		}
		return records[i].Timestamp.Before(records[j].Timestamp)
	})
	groupByService := countSpanGroupKeys(records) <= maxCompactionGroupKeys
	for start := 0; start < len(records); {
		end := start + 1
		if groupByService {
			partition := recordPartition(records[start].Attributes)
			service := records[start].Service
			for end < len(records) && records[end].Service == service && recordPartition(records[end].Attributes) == partition && end-start < maxRecords {
				end++
			}
		} else {
			for end < len(records) && end-start < maxRecords {
				end++
			}
		}
		minTime, maxTime := records[start].Timestamp, records[start].Timestamp
		for _, record := range records[start:end] {
			if record.Timestamp.Before(minTime) {
				minTime = record.Timestamp
			}
			if record.Timestamp.After(maxTime) {
				maxTime = record.Timestamp
			}
		}
		chunkID := nextCompactedChunkID(minTime, maxTime, seq)
		if err := c.ingester.FlushChunkWithOptions(ctx, SignalTraces, chunkID, nil, nil, records[start:end], ChunkWriteOptions{CompactionLevel: c.compactionLevel}); err != nil {
			return err
		}
		start = end
	}
	return nil
}

func countLogGroupKeys(records []LogRecord) int {
	count := 0
	previous := "\x00"
	for _, record := range records {
		key := recordPartition(record.Labels) + "\x00" + record.Service
		if key != previous {
			count++
			previous = key
		}
	}
	return count
}

func countMetricGroupKeys(records []MetricRecord) int {
	count := 0
	previous := "\x00"
	for _, record := range records {
		key := recordPartition(record.Labels) + "\x00" + record.MetricName
		if key != previous {
			count++
			previous = key
		}
	}
	return count
}

func countSpanGroupKeys(records []SpanRecord) int {
	count := 0
	previous := "\x00"
	for _, record := range records {
		key := recordPartition(record.Attributes) + "\x00" + record.Service
		if key != previous {
			count++
			previous = key
		}
	}
	return count
}

func chunkDataPath(signal Signal, chunkPath string) string {
	return filepath.Join("chunks", string(signal), chunkPath, chunkDataFileName)
}

func chunkStoragePath(signal Signal, chunkPath string) string {
	return filepath.Join("chunks", string(signal), chunkPath)
}

func isCompactedChunkPath(path string) bool {
	return strings.HasPrefix(filepath.Base(path), "compact-")
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

// RunCompactor starts a background loop that compacts complete windows. Only
// windows whose end is at least minAge in the past are touched. It walks the
// configured lookback on every tick so a restart or a burst of small files
// does not strand older chunks forever.
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

// Stats returns a snapshot of the compactor state.
func (c *Compactor) Stats() CompactionStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CompactionStats{
		Running:       c.running,
		LastRun:       c.lastRun,
		LastResult:    c.lastResult,
		Runs:          c.runs,
		InputChunks:   c.inputChunks,
		OutputChunks:  c.outputChunks,
		DeletedChunks: c.deletedChunks,
		Records:       c.records,
		TargetBytes:   defaultCompactionMaxBytes,
	}
}

// CompactionStats returns the local compactor status.
func (e *LogEngine) CompactionStats() CompactionStats {
	if e.compactor == nil {
		return CompactionStats{}
	}
	return e.compactor.Stats()
}
