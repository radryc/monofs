package logengine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/blugelabs/bluge"
)

// Ingester handles the chunking and dual-write architecture.
type Ingester struct {
	store        ObjectStoreBackend
	chunkDur     time.Duration
	rowGroupRows int
}

// NewIngester creates a new Ingester.
func NewIngester(store ObjectStoreBackend, chunkDur time.Duration) *Ingester {
	return &Ingester{
		store:        store,
		chunkDur:     chunkDur,
		rowGroupRows: defaultParquetRowGroupRows,
	}
}

// ChunkWriteOptions carries compaction provenance into the chunk manifest.
type ChunkWriteOptions struct {
	CompactionSources []string
	CompactionLevel   int
}

func validChunkID(chunkID string) bool {
	return chunkID != "" && chunkID != "." && chunkID != ".." &&
		filepath.Base(chunkID) == chunkID &&
		!strings.ContainsAny(chunkID, `/\`)
}

// FlushChunk writes a buffer of telemetry records for the given signal type.
// Exactly one of logs/metrics/spans should be non-nil; the signal parameter selects
// the storage path and Parquet schema.
func (i *Ingester) FlushChunk(ctx context.Context, signal Signal, chunkID string, logs []LogRecord, metrics []MetricRecord, spans []SpanRecord) error {
	return i.FlushChunkWithOptions(ctx, signal, chunkID, logs, metrics, spans, ChunkWriteOptions{})
}

// FlushChunkWithOptions writes a chunk and records compaction provenance in
// its manifest.
func (i *Ingester) FlushChunkWithOptions(ctx context.Context, signal Signal, chunkID string, logs []LogRecord, metrics []MetricRecord, spans []SpanRecord, opts ChunkWriteOptions) error {
	if !validChunkID(chunkID) {
		return fmt.Errorf("invalid chunk id %q", chunkID)
	}
	switch signal {
	case SignalLogs:
		return i.flushLogs(ctx, chunkID, logs, opts)
	case SignalMetrics:
		return i.flushMetrics(ctx, chunkID, metrics, opts)
	case SignalTraces:
		return i.flushTraces(ctx, chunkID, spans, opts)
	default:
		return fmt.Errorf("unknown signal: %s", signal)
	}
}

func (i *Ingester) flushLogs(ctx context.Context, chunkID string, logs []LogRecord, opts ChunkWriteOptions) error {
	if len(logs) == 0 {
		return nil
	}
	logs = sortLogRecords(logs)

	minTime, maxTime := logs[0].Timestamp, logs[0].Timestamp
	for _, l := range logs {
		if l.Timestamp.Before(minTime) {
			minTime = l.Timestamp
		}
		if l.Timestamp.After(maxTime) {
			maxTime = l.Timestamp
		}
	}

	manifest := ChunkManifest{
		ChunkID:           chunkID,
		Signal:            SignalLogs,
		MinTime:           minTime,
		MaxTime:           maxTime,
		Services:          collectLogServices(logs),
		Levels:            collectLogLevels(logs),
		Partitions:        collectLogPartitions(logs),
		LabelValues:       collectLogLabelValues(logs),
		TraceBloom:        buildLogTraceBloom(logs),
		RecordCount:       int64(len(logs)),
		SchemaVersion:     2,
		CompactionSources: opts.CompactionSources,
		CompactionLevel:   opts.CompactionLevel,
	}
	chunkPrefix := filepath.Join("chunks", string(SignalLogs), relativeChunkPath(chunkID, minTime, chunkPartition(manifest.Partitions)))

	parquetBytes, rowGroups, err := i.writeLogParquetBatched(ctx, filepath.Join(chunkPrefix, chunkDataFileName), logs)
	if err != nil {
		return err
	}
	manifest.ParquetBytes = parquetBytes
	manifest.RowGroups = rowGroups
	if err := i.writeBlugeIndex(ctx, filepath.Join(chunkPrefix, "text.index.tar.gz"), logs); err != nil {
		return err
	}
	// The manifest is the commit marker: write it only after data and indexes
	// are durable so readers never observe a half-written chunk.
	return i.writeMetadata(ctx, filepath.Join(chunkPrefix, "metadata.json"), manifest)
}

func (i *Ingester) flushMetrics(ctx context.Context, chunkID string, metrics []MetricRecord, opts ChunkWriteOptions) error {
	if len(metrics) == 0 {
		return nil
	}
	metrics = sortMetricRecords(metrics)

	minTime, maxTime := metrics[0].Timestamp, metrics[0].Timestamp
	for _, m := range metrics {
		if m.Timestamp.Before(minTime) {
			minTime = m.Timestamp
		}
		if m.Timestamp.After(maxTime) {
			maxTime = m.Timestamp
		}
	}

	manifest := ChunkManifest{
		ChunkID:           chunkID,
		Signal:            SignalMetrics,
		MinTime:           minTime,
		MaxTime:           maxTime,
		Services:          collectMetricServices(metrics),
		MetricNames:       collectMetricNames(metrics),
		Partitions:        collectMetricPartitions(metrics),
		MetricLabelValues: collectMetricLabelValues(metrics),
		RecordCount:       int64(len(metrics)),
		SchemaVersion:     2,
		CompactionSources: opts.CompactionSources,
		CompactionLevel:   opts.CompactionLevel,
	}
	chunkPrefix := filepath.Join("chunks", string(SignalMetrics), relativeChunkPath(chunkID, minTime, chunkPartition(manifest.Partitions)))

	parquetBytes, rowGroups, err := i.writeMetricParquetBatched(ctx, filepath.Join(chunkPrefix, chunkDataFileName), metrics)
	if err != nil {
		return err
	}
	manifest.ParquetBytes = parquetBytes
	manifest.RowGroups = rowGroups
	if err := i.writeMetricSeriesIndex(ctx, filepath.Join(chunkPrefix, "series.index.json"), collectMetricSeries(metrics)); err != nil {
		return err
	}
	return i.writeMetadata(ctx, filepath.Join(chunkPrefix, "metadata.json"), manifest)
}

func (i *Ingester) flushTraces(ctx context.Context, chunkID string, spans []SpanRecord, opts ChunkWriteOptions) error {
	if len(spans) == 0 {
		return nil
	}
	spans = sortSpanRecords(spans)

	minTime, maxTime := spans[0].Timestamp, spans[0].Timestamp
	for _, s := range spans {
		if s.Timestamp.Before(minTime) {
			minTime = s.Timestamp
		}
		if s.EndTime.After(maxTime) {
			maxTime = s.EndTime
		}
	}

	traceTagKeys, traceTagValues := collectTraceDiscovery(spans)
	manifest := ChunkManifest{
		ChunkID:           chunkID,
		Signal:            SignalTraces,
		MinTime:           minTime,
		MaxTime:           maxTime,
		Services:          collectTraceServices(spans),
		Partitions:        collectSpanPartitions(spans),
		TraceBloom:        buildTraceBloom(spans),
		TraceTagKeys:      traceTagKeys,
		TraceTagValues:    traceTagValues,
		RecordCount:       int64(len(spans)),
		SchemaVersion:     2,
		CompactionSources: opts.CompactionSources,
		CompactionLevel:   opts.CompactionLevel,
	}
	chunkPrefix := filepath.Join("chunks", string(SignalTraces), relativeChunkPath(chunkID, minTime, chunkPartition(manifest.Partitions)))

	parquetBytes, rowGroups, err := i.writeTraceParquetBatched(ctx, filepath.Join(chunkPrefix, chunkDataFileName), spans)
	if err != nil {
		return err
	}
	manifest.ParquetBytes = parquetBytes
	manifest.RowGroups = rowGroups
	return i.writeMetadata(ctx, filepath.Join(chunkPrefix, "metadata.json"), manifest)
}

func (i *Ingester) writeBlugeIndex(ctx context.Context, path string, logs []LogRecord) error {
	// Create a temporary directory for the Bluge index
	tmpDir, err := os.MkdirTemp("", "bluge-index-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	config := bluge.DefaultConfig(tmpDir)
	writer, err := bluge.OpenWriter(config)
	if err != nil {
		return err
	}

	for idx, log := range logs {
		doc := bluge.NewDocument(fmt.Sprintf("%d", idx)) // Use row ID as document ID
		// Index the raw message text
		doc.AddField(bluge.NewTextField("raw_message", log.RawMessage).StoreValue())

		if err := writer.Update(doc.ID(), doc); err != nil {
			writer.Close()
			return err
		}
	}

	if err := writer.Close(); err != nil {
		return err
	}

	// Tar and gzip the directory
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	err = filepath.Walk(tmpDir, func(file string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		header, err := tar.FileInfoHeader(fi, fi.Name())
		if err != nil {
			return err
		}

		relPath, _ := filepath.Rel(tmpDir, file)
		if relPath == "." {
			return nil
		}
		header.Name = relPath

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if !fi.Mode().IsRegular() {
			return nil
		}

		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer f.Close()

		_, err = io.Copy(tw, f)
		return err
	})

	if err != nil {
		return err
	}

	if err := tw.Close(); err != nil {
		return err
	}
	if err := gw.Close(); err != nil {
		return err
	}

	return i.store.Write(ctx, path, &buf)
}

func (i *Ingester) writeMetadata(ctx context.Context, path string, manifest ChunkManifest) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(manifest); err != nil {
		return err
	}
	return i.store.Write(ctx, path, &buf)
}

// writeMetricSeriesIndex persists the chunk series index: one entry per
// distinct metric series with its first/last sample time.
func (i *Ingester) writeMetricSeriesIndex(ctx context.Context, path string, series []MetricSeriesMeta) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(series); err != nil {
		return err
	}
	return i.store.Write(ctx, path, &buf)
}

// collectMetricSeries builds the per-series index for a chunk. Series identity
// is the metric name plus the full label set; the first/last sample times
// bound where the series has data so presence aggregations stay cheap.
func collectMetricSeries(metrics []MetricRecord) []MetricSeriesMeta {
	type seriesEntry struct {
		meta MetricSeriesMeta
	}
	byKey := make(map[string]*seriesEntry)
	for _, metric := range metrics {
		if metric.MetricName == "" || metric.Timestamp.IsZero() {
			continue
		}
		key := metricSeriesKey(metric)
		entry := byKey[key]
		if entry == nil {
			labels := make(map[string]string, len(metric.Labels))
			for name, value := range metric.Labels {
				labels[name] = value
			}
			entry = &seriesEntry{meta: MetricSeriesMeta{
				MetricName: metric.MetricName,
				Service:    metric.Service,
				Labels:     labels,
				MinTime:    metric.Timestamp,
				MaxTime:    metric.Timestamp,
			}}
			byKey[key] = entry
			continue
		}
		if metric.Timestamp.Before(entry.meta.MinTime) {
			entry.meta.MinTime = metric.Timestamp
		}
		if metric.Timestamp.After(entry.meta.MaxTime) {
			entry.meta.MaxTime = metric.Timestamp
		}
	}
	if len(byKey) == 0 {
		return nil
	}
	out := make([]MetricSeriesMeta, 0, len(byKey))
	for _, entry := range byKey {
		out = append(out, entry.meta)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].MetricName != out[b].MetricName {
			return out[a].MetricName < out[b].MetricName
		}
		return out[a].Service < out[b].Service
	})
	return out
}

// metricSeriesKey builds the identity key for a metric record.
func metricSeriesKey(metric MetricRecord) string {
	names := make([]string, 0, len(metric.Labels))
	for name := range metric.Labels {
		names = append(names, name)
	}
	sort.Strings(names)
	var builder strings.Builder
	builder.WriteString(metric.MetricName)
	builder.WriteByte(0)
	builder.WriteString(metric.Service)
	for _, name := range names {
		builder.WriteByte(0)
		builder.WriteString(name)
		builder.WriteByte('=')
		builder.WriteString(metric.Labels[name])
	}
	return builder.String()
}

func collectLogServices(logs []LogRecord) []string {
	services := make(map[string]struct{})
	for _, log := range logs {
		if log.Service == "" {
			continue
		}
		services[log.Service] = struct{}{}
	}
	return sortedKeys(services)
}

func collectLogLevels(logs []LogRecord) []string {
	levels := make(map[string]struct{})
	for _, log := range logs {
		if log.Level == "" {
			continue
		}
		levels[log.Level] = struct{}{}
		if len(levels) > maxManifestLabelValues {
			return nil
		}
	}
	return sortedKeys(levels)
}

// Manifest label dictionaries are only used for safe positive pruning. Omit a
// key when its cardinality exceeds the cap so a missing value never becomes a
// false negative.
const (
	maxManifestLabelKeys   = 64
	maxManifestLabelValues = 64
)

func collectLogLabelValues(logs []LogRecord) map[string][]string {
	valuesByName := make(map[string]map[string]struct{})
	for _, log := range logs {
		for name, value := range log.Labels {
			if name == "" || value == "" {
				continue
			}
			values := valuesByName[name]
			if values == nil {
				if len(valuesByName) >= maxManifestLabelKeys {
					continue
				}
				values = make(map[string]struct{})
				valuesByName[name] = values
			}
			if len(values) >= maxManifestLabelValues {
				continue
			}
			values[value] = struct{}{}
		}
	}
	if len(valuesByName) == 0 {
		return nil
	}
	out := make(map[string][]string, len(valuesByName))
	for name, values := range valuesByName {
		// Only retain complete dictionaries. A truncated list could be mistaken
		// for "all values in this chunk" and prune a real match.
		if len(values) >= maxManifestLabelValues {
			continue
		}
		out[name] = sortedKeys(values)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func buildLogTraceBloom(logs []LogRecord) []byte {
	traceIDs := make([]string, 0, len(logs))
	for _, log := range logs {
		if log.TraceID != "" {
			traceIDs = append(traceIDs, log.TraceID)
		}
	}
	return buildTraceBloomFromIDs(traceIDs)
}

func collectMetricServices(metrics []MetricRecord) []string {
	services := make(map[string]struct{})
	for _, metric := range metrics {
		if metric.Service == "" {
			continue
		}
		services[metric.Service] = struct{}{}
	}
	return sortedKeys(services)
}

func collectMetricNames(metrics []MetricRecord) []string {
	names := make(map[string]struct{})
	for _, metric := range metrics {
		if metric.MetricName == "" {
			continue
		}
		names[metric.MetricName] = struct{}{}
	}
	return sortedKeys(names)
}

func collectMetricLabelValues(metrics []MetricRecord) map[string][]string {
	valuesByName := make(map[string]map[string]struct{})
	for _, metric := range metrics {
		for name, value := range metric.Labels {
			if name == "" || value == "" {
				continue
			}
			values := valuesByName[name]
			if values == nil {
				values = make(map[string]struct{})
				valuesByName[name] = values
			}
			values[value] = struct{}{}
		}
	}
	if len(valuesByName) == 0 {
		return nil
	}
	out := make(map[string][]string, len(valuesByName))
	for name, values := range valuesByName {
		out[name] = sortedKeys(values)
	}
	return out
}

func collectTraceServices(spans []SpanRecord) []string {
	services := make(map[string]struct{})
	for _, span := range spans {
		if span.Service == "" {
			continue
		}
		services[span.Service] = struct{}{}
	}
	return sortedKeys(services)
}

func sortedKeys(values map[string]struct{}) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func buildTraceBloom(spans []SpanRecord) []byte {
	traceIDs := make([]string, 0, len(spans))
	for _, span := range spans {
		if span.TraceID != "" {
			traceIDs = append(traceIDs, span.TraceID)
		}
	}
	return buildTraceBloomFromIDs(traceIDs)
}

func buildTraceBloomFromIDs(traceIDs []string) []byte {
	if len(traceIDs) == 0 {
		return nil
	}
	const bloomBytes = 256
	const bloomHashes = 4

	bloom := make([]byte, bloomBytes)
	for _, traceID := range traceIDs {
		indexes := bloomIndexes(traceID, bloomBytes*8, bloomHashes)
		for _, idx := range indexes {
			byteIdx := idx / 8
			bitIdx := idx % 8
			bloom[byteIdx] |= 1 << bitIdx
		}
	}
	return bloom
}

func bloomIndexes(value string, bitCount, hashCount int) []int {
	primary := fnv.New64a()
	_, _ = primary.Write([]byte(value))
	primarySum := primary.Sum64()

	secondary := fnv.New64()
	_, _ = secondary.Write([]byte(value))
	secondarySum := secondary.Sum64()
	if secondarySum == 0 {
		secondarySum = 1
	}

	indexes := make([]int, 0, hashCount)
	for idx := 0; idx < hashCount; idx++ {
		combined := primarySum + uint64(idx)*secondarySum
		indexes = append(indexes, int(combined%uint64(bitCount)))
	}
	return indexes
}
