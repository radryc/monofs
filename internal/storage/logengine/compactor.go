package logengine

import (
	"bytes"
	"context"
	"encoding/json"
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
// configured minimum age are compacted, and source chunks are deleted only
// after a durable completion marker confirms every output is present.
const (
	defaultCompactionMaxRecords = 1_000_000
	defaultCompactionMaxBytes   = 256 << 20
	defaultCompactionMinAge     = time.Hour
	// maxCompactionCatchUp bounds how far back the background worker looks for
	// uncompacted chunks on each tick. Older backlog can still be compacted with
	// monofs-logengine-compact.
	maxCompactionCatchUp = 24 * time.Hour
	// compactionMarkerDirName stores per-run completion markers. A marker is
	// written only after every output chunk is durable; source chunks are
	// deleted only after the marker exists, so a crash cannot strand duplicate
	// rows without a way to finish or roll back the run.
	compactionMarkerDirName = "compaction-runs"
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

// compactionRunMarker is the durable completion record for one compaction run.
// It is written after all output manifests exist and before any source is
// deleted. Cleanup verifies every output before deleting sources.
type compactionRunMarker struct {
	Signal    Signal    `json:"signal"`
	Group     string    `json:"group"`
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	Sources   []string  `json:"sources"`
	Outputs   []string  `json:"outputs"`
	CreatedAt time.Time `json:"created_at"`
}

// Compactor merges small chunks within a time range.
type Compactor struct {
	store           *CachedStore
	ingester        *Ingester
	query           *QueryEngine
	mu              sync.Mutex
	compactionLevel int
	compactionGroup string

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

	c.compactionGroup = fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	validGroups, err := c.cleanupCompactionMarkers(ctx, signal)
	if err != nil {
		return result, err
	}

	paths, err := c.store.ListChunkPaths(ctx, signal, from, to)
	if err != nil {
		return result, err
	}
	raw := make([]string, 0, len(paths))
	compacted := make([]string, 0)
	legacyCompacted := false
	for _, path := range paths {
		if !isCompactedChunkPath(path) {
			raw = append(raw, path)
			continue
		}
		manifest, err := c.store.ReadManifest(ctx, signal, path)
		if err != nil {
			if errors.Is(err, ErrGhostChunk) {
				continue
			}
			return result, err
		}
		if manifest.CompactionGroup != "" {
			if _, ok := validGroups[manifest.CompactionGroup]; !ok {
				// Orphaned output from a run that never wrote its marker. All
				// sources still exist because deletion happens after the
				// marker, so removing the output and retrying is safe.
				if err := c.store.Delete(ctx, chunkStoragePath(signal, path)); err != nil {
					return result, err
				}
				result.DeletedChunks++
				continue
			}
		} else {
			legacyCompacted = true
		}
		compacted = append(compacted, path)
	}
	result.InputChunks = len(raw)
	if len(compacted) > 0 && legacyCompacted && len(raw) > 0 {
		// Manifests written before compaction markers/provenance existed cannot
		// prove that they cover every raw source. Leave the range alone rather
		// than risk deleting data.
		return result, nil
	}
	if len(raw) == 0 {
		return result, nil
	}
	if len(raw) < 2 {
		return result, nil
	}

	observer := beginQueryPathObservation(signal)
	defer observer.finish()

	sources := append([]string(nil), raw...)
	outputs := make([]string, 0)
	outputSeq := 0
	writeCompacted := func(records any, _ time.Time, _ time.Time) error {
		var written []string
		var err error
		switch typed := records.(type) {
		case []LogRecord:
			written, err = c.writeLogCompacted(ctx, typed, opts.MaxRecords, &outputSeq, sources)
		case []MetricRecord:
			written, err = c.writeMetricCompacted(ctx, typed, opts.MaxRecords, &outputSeq, sources)
		case []SpanRecord:
			written, err = c.writeSpanCompacted(ctx, typed, opts.MaxRecords, &outputSeq, sources)
		default:
			return fmt.Errorf("unsupported compaction record type %T", records)
		}
		if err != nil {
			return err
		}
		outputs = append(outputs, written...)
		return nil
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

	result.OutputChunks = len(outputs)
	if len(outputs) == 0 {
		// Nothing was written; keep all sources.
		return result, nil
	}
	marker := compactionRunMarker{
		Signal:    signal,
		Group:     c.compactionGroup,
		From:      from,
		To:        to,
		Sources:   sources,
		Outputs:   outputs,
		CreatedAt: time.Now().UTC(),
	}
	if err := c.writeCompactionMarker(ctx, signal, marker); err != nil {
		return result, err
	}
	for _, path := range sources {
		if err := c.store.Delete(ctx, chunkStoragePath(signal, path)); err != nil {
			return result, err
		}
		result.DeletedChunks++
	}
	return result, nil
}

func compactionMarkerDir(signal Signal, group string) string {
	return filepath.ToSlash(filepath.Join("chunks", string(signal), compactionMarkerDirName, group))
}

func compactionMarkerPath(signal Signal, group string) string {
	return filepath.Join(compactionMarkerDir(signal, group), "marker.json")
}

func (c *Compactor) writeCompactionMarker(ctx context.Context, signal Signal, marker compactionRunMarker) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(marker); err != nil {
		return err
	}
	return c.store.Write(ctx, compactionMarkerPath(signal, marker.Group), &buf)
}

// cleanupCompactionMarkers finishes or rolls back interrupted compaction runs.
// It returns the groups whose output manifests are all present and whose source
// chunks have been deleted. A marker with missing outputs is rolled back by
// deleting the outputs it did write; the sources are still intact, so the next
// compaction can retry from scratch.
func (c *Compactor) cleanupCompactionMarkers(ctx context.Context, signal Signal) (map[string]struct{}, error) {
	valid := make(map[string]struct{})
	root := filepath.ToSlash(filepath.Join("chunks", string(signal), compactionMarkerDirName))
	groups, err := c.store.ListChunks(ctx, root)
	if err != nil {
		return nil, err
	}
	for _, group := range groups {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		markerPath := compactionMarkerPath(signal, group)
		rc, readErr := c.store.Read(ctx, markerPath)
		if readErr != nil {
			if errors.Is(readErr, ErrGhostChunk) {
				continue
			}
			return nil, readErr
		}
		var marker compactionRunMarker
		decodeErr := json.NewDecoder(rc).Decode(&marker)
		rc.Close()
		if decodeErr != nil {
			// A torn marker is equivalent to a run that never completed. The
			// outputs have a group without a valid marker and are removed when
			// the range is scanned again.
			log.Printf("logengine compaction marker %s is invalid, discarding: %v", markerPath, decodeErr)
			if deleteErr := c.store.Delete(ctx, compactionMarkerDir(signal, group)); deleteErr != nil {
				return nil, deleteErr
			}
			continue
		}
		complete := true
		for _, output := range marker.Outputs {
			if _, err := c.store.ReadManifest(ctx, signal, output); err != nil {
				if errors.Is(err, ErrGhostChunk) {
					complete = false
					break
				}
				return nil, err
			}
		}
		if !complete {
			// Roll back the partial run. Sources were never deleted before the
			// marker existed, so they can be compacted again.
			for _, output := range marker.Outputs {
				if deleteErr := c.store.Delete(ctx, chunkStoragePath(signal, output)); deleteErr != nil {
					return nil, deleteErr
				}
			}
		} else {
			for _, source := range marker.Sources {
				if _, err := c.store.ReadManifest(ctx, signal, source); err != nil {
					if errors.Is(err, ErrGhostChunk) {
						continue
					}
					return nil, err
				}
				if deleteErr := c.store.Delete(ctx, chunkStoragePath(signal, source)); deleteErr != nil {
					return nil, deleteErr
				}
			}
			valid[group] = struct{}{}
		}
		if deleteErr := c.store.Delete(ctx, compactionMarkerDir(signal, group)); deleteErr != nil {
			return nil, deleteErr
		}
	}
	return valid, nil
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

func nextCompactedChunkID(minTime, maxTime time.Time, seq *int, group string) string {
	chunkID := fmt.Sprintf("compact-%d-%d-%s-%d", minTime.UTC().UnixNano(), maxTime.UTC().UnixNano(), group, *seq)
	*seq++
	return chunkID
}

func (c *Compactor) writeLogCompacted(ctx context.Context, records []LogRecord, maxRecords int, seq *int, sources []string) ([]string, error) {
	if len(records) == 0 {
		return nil, nil
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
	outputs := make([]string, 0, 1)
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
		chunkID := nextCompactedChunkID(minTime, maxTime, seq, c.compactionGroup)
		path, err := c.ingester.FlushChunkWithOptions(ctx, SignalLogs, chunkID, records[start:end], nil, nil, ChunkWriteOptions{
			CompactionLevel:   c.compactionLevel,
			CompactionGroup:   c.compactionGroup,
			CompactionSources: sources,
		})
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, path)
		start = end
	}
	return outputs, nil
}

func (c *Compactor) writeMetricCompacted(ctx context.Context, records []MetricRecord, maxRecords int, seq *int, sources []string) ([]string, error) {
	if len(records) == 0 {
		return nil, nil
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
	outputs := make([]string, 0, 1)
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
		chunkID := nextCompactedChunkID(minTime, maxTime, seq, c.compactionGroup)
		path, err := c.ingester.FlushChunkWithOptions(ctx, SignalMetrics, chunkID, nil, records[start:end], nil, ChunkWriteOptions{
			CompactionLevel:   c.compactionLevel,
			CompactionGroup:   c.compactionGroup,
			CompactionSources: sources,
		})
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, path)
		start = end
	}
	return outputs, nil
}

func (c *Compactor) writeSpanCompacted(ctx context.Context, records []SpanRecord, maxRecords int, seq *int, sources []string) ([]string, error) {
	if len(records) == 0 {
		return nil, nil
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
	outputs := make([]string, 0, 1)
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
		chunkID := nextCompactedChunkID(minTime, maxTime, seq, c.compactionGroup)
		path, err := c.ingester.FlushChunkWithOptions(ctx, SignalTraces, chunkID, nil, nil, records[start:end], ChunkWriteOptions{
			CompactionLevel:   c.compactionLevel,
			CompactionGroup:   c.compactionGroup,
			CompactionSources: sources,
		})
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, path)
		start = end
	}
	return outputs, nil
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
