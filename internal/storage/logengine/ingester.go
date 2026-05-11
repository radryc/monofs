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
	"sync"
	"time"

	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/compress"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/schema"
	"github.com/blugelabs/bluge"
)

// Ingester handles the chunking and dual-write architecture.
type Ingester struct {
	store    ObjectStoreBackend
	chunkDur time.Duration
}

// NewIngester creates a new Ingester.
func NewIngester(store ObjectStoreBackend, chunkDur time.Duration) *Ingester {
	return &Ingester{
		store:    store,
		chunkDur: chunkDur,
	}
}

// FlushChunk writes a buffer of telemetry records for the given signal type.
// Exactly one of logs/metrics/spans should be non-nil; the signal parameter selects
// the storage path and Parquet schema.
func (i *Ingester) FlushChunk(ctx context.Context, signal Signal, chunkID string, logs []LogRecord, metrics []MetricRecord, spans []SpanRecord) error {
	switch signal {
	case SignalLogs:
		return i.flushLogs(ctx, chunkID, logs)
	case SignalMetrics:
		return i.flushMetrics(ctx, chunkID, metrics)
	case SignalTraces:
		return i.flushTraces(ctx, chunkID, spans)
	default:
		return fmt.Errorf("unknown signal: %s", signal)
	}
}

func (i *Ingester) flushLogs(ctx context.Context, chunkID string, logs []LogRecord) error {
	if len(logs) == 0 {
		return nil
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 3)

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
		ChunkID:  chunkID,
		Signal:   SignalLogs,
		MinTime:  minTime,
		MaxTime:  maxTime,
		Services: collectLogServices(logs),
	}
	chunkPrefix := filepath.Join("chunks", string(SignalLogs), chunkID)

	wg.Add(1)
	go func() {
		defer wg.Done()
		errCh <- i.writeLogParquet(ctx, filepath.Join(chunkPrefix, "data.parquet"), logs)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		errCh <- i.writeBlugeIndex(ctx, filepath.Join(chunkPrefix, "text.index.tar.gz"), logs)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		errCh <- i.writeMetadata(ctx, filepath.Join(chunkPrefix, "metadata.json"), manifest)
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

func (i *Ingester) flushMetrics(ctx context.Context, chunkID string, metrics []MetricRecord) error {
	if len(metrics) == 0 {
		return nil
	}

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
		MetricLabelValues: collectMetricLabelValues(metrics),
	}
	chunkPrefix := filepath.Join("chunks", string(SignalMetrics), chunkID)

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	wg.Add(1)
	go func() {
		defer wg.Done()
		errCh <- i.writeMetricParquet(ctx, filepath.Join(chunkPrefix, "data.parquet"), metrics)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		errCh <- i.writeMetadata(ctx, filepath.Join(chunkPrefix, "metadata.json"), manifest)
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

func (i *Ingester) flushTraces(ctx context.Context, chunkID string, spans []SpanRecord) error {
	if len(spans) == 0 {
		return nil
	}

	minTime, maxTime := spans[0].Timestamp, spans[0].Timestamp
	for _, s := range spans {
		if s.Timestamp.Before(minTime) {
			minTime = s.Timestamp
		}
		if s.EndTime.After(maxTime) {
			maxTime = s.EndTime
		}
	}

	manifest := ChunkManifest{
		ChunkID:    chunkID,
		Signal:     SignalTraces,
		MinTime:    minTime,
		MaxTime:    maxTime,
		Services:   collectTraceServices(spans),
		TraceBloom: buildTraceBloom(spans),
	}
	chunkPrefix := filepath.Join("chunks", string(SignalTraces), chunkID)

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	wg.Add(1)
	go func() {
		defer wg.Done()
		errCh <- i.writeTraceParquet(ctx, filepath.Join(chunkPrefix, "data.parquet"), spans)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		errCh <- i.writeMetadata(ctx, filepath.Join(chunkPrefix, "metadata.json"), manifest)
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

func (i *Ingester) writeLogParquet(ctx context.Context, path string, logs []LogRecord) error {
	fields := schema.FieldList{
		schema.NewInt64Node("timestamp", parquet.Repetitions.Required, -1),
		schema.NewByteArrayNode("level", parquet.Repetitions.Required, -1),
		schema.NewByteArrayNode("service", parquet.Repetitions.Required, -1),
		schema.NewByteArrayNode("trace_id", parquet.Repetitions.Required, -1),
		schema.NewByteArrayNode("raw_message", parquet.Repetitions.Required, -1),
	}
	parquetSchema, err := schema.NewGroupNode("log_record", parquet.Repetitions.Required, fields, -1)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	props := parquet.NewWriterProperties(
		parquet.WithCompression(compress.Codecs.Zstd),
		parquet.WithDictionaryDefault(true),
	)

	writer := file.NewParquetWriter(&buf, parquetSchema, file.WithWriterProps(props))
	defer writer.Close()

	rgw := writer.AppendRowGroup()

	// Timestamp
	cw, err := rgw.NextColumn()
	if err != nil {
		return err
	}
	tsWriter := cw.(*file.Int64ColumnChunkWriter)
	timestamps := make([]int64, len(logs))
	for idx, l := range logs {
		timestamps[idx] = l.Timestamp.UnixNano()
	}
	tsWriter.WriteBatch(timestamps, nil, nil)
	tsWriter.Close()

	// Level
	cw, err = rgw.NextColumn()
	if err != nil {
		return err
	}
	lvlWriter := cw.(*file.ByteArrayColumnChunkWriter)
	levels := make([]parquet.ByteArray, len(logs))
	for idx, l := range logs {
		levels[idx] = parquet.ByteArray(l.Level)
	}
	lvlWriter.WriteBatch(levels, nil, nil)
	lvlWriter.Close()

	// Service
	cw, err = rgw.NextColumn()
	if err != nil {
		return err
	}
	svcWriter := cw.(*file.ByteArrayColumnChunkWriter)
	services := make([]parquet.ByteArray, len(logs))
	for idx, l := range logs {
		services[idx] = parquet.ByteArray(l.Service)
	}
	svcWriter.WriteBatch(services, nil, nil)
	svcWriter.Close()

	// TraceID
	cw, err = rgw.NextColumn()
	if err != nil {
		return err
	}
	traceWriter := cw.(*file.ByteArrayColumnChunkWriter)
	traces := make([]parquet.ByteArray, len(logs))
	for idx, l := range logs {
		traces[idx] = parquet.ByteArray(l.TraceID)
	}
	traceWriter.WriteBatch(traces, nil, nil)
	traceWriter.Close()

	// RawMessage
	cw, err = rgw.NextColumn()
	if err != nil {
		return err
	}
	msgWriter := cw.(*file.ByteArrayColumnChunkWriter)
	messages := make([]parquet.ByteArray, len(logs))
	for idx, l := range logs {
		messages[idx] = parquet.ByteArray(l.RawMessage)
	}
	msgWriter.WriteBatch(messages, nil, nil)
	msgWriter.Close()

	if err := rgw.Close(); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}

	return i.store.Write(ctx, path, &buf)
}

func (i *Ingester) writeMetricParquet(ctx context.Context, path string, metrics []MetricRecord) error {
	fields := schema.FieldList{
		schema.NewInt64Node("timestamp", parquet.Repetitions.Required, -1),
		schema.NewByteArrayNode("service", parquet.Repetitions.Required, -1),
		schema.NewByteArrayNode("metric_name", parquet.Repetitions.Required, -1),
		schema.NewFloat64Node("value", parquet.Repetitions.Required, -1),
		schema.NewByteArrayNode("labels_json", parquet.Repetitions.Required, -1),
	}
	parquetSchema, err := schema.NewGroupNode("metric_record", parquet.Repetitions.Required, fields, -1)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	props := parquet.NewWriterProperties(
		parquet.WithCompression(compress.Codecs.Zstd),
		parquet.WithDictionaryDefault(true),
	)
	writer := file.NewParquetWriter(&buf, parquetSchema, file.WithWriterProps(props))
	defer writer.Close()
	rgw := writer.AppendRowGroup()

	// Timestamp
	cw, err := rgw.NextColumn()
	if err != nil {
		return err
	}
	tsWriter := cw.(*file.Int64ColumnChunkWriter)
	timestamps := make([]int64, len(metrics))
	for idx, m := range metrics {
		timestamps[idx] = m.Timestamp.UnixNano()
	}
	tsWriter.WriteBatch(timestamps, nil, nil)
	tsWriter.Close()

	// Service
	cw, err = rgw.NextColumn()
	if err != nil {
		return err
	}
	svcWriter := cw.(*file.ByteArrayColumnChunkWriter)
	services := make([]parquet.ByteArray, len(metrics))
	for idx, m := range metrics {
		services[idx] = parquet.ByteArray(m.Service)
	}
	svcWriter.WriteBatch(services, nil, nil)
	svcWriter.Close()

	// MetricName
	cw, err = rgw.NextColumn()
	if err != nil {
		return err
	}
	nameWriter := cw.(*file.ByteArrayColumnChunkWriter)
	names := make([]parquet.ByteArray, len(metrics))
	for idx, m := range metrics {
		names[idx] = parquet.ByteArray(m.MetricName)
	}
	nameWriter.WriteBatch(names, nil, nil)
	nameWriter.Close()

	// Value
	cw, err = rgw.NextColumn()
	if err != nil {
		return err
	}
	valWriter := cw.(*file.Float64ColumnChunkWriter)
	values := make([]float64, len(metrics))
	for idx, m := range metrics {
		values[idx] = m.Value
	}
	valWriter.WriteBatch(values, nil, nil)
	valWriter.Close()

	// Labels JSON
	cw, err = rgw.NextColumn()
	if err != nil {
		return err
	}
	lblWriter := cw.(*file.ByteArrayColumnChunkWriter)
	labels := make([]parquet.ByteArray, len(metrics))
	for idx, m := range metrics {
		b, _ := json.Marshal(m.Labels)
		labels[idx] = b
	}
	lblWriter.WriteBatch(labels, nil, nil)
	lblWriter.Close()

	if err := rgw.Close(); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return i.store.Write(ctx, path, &buf)
}

func (i *Ingester) writeTraceParquet(ctx context.Context, path string, spans []SpanRecord) error {
	fields := schema.FieldList{
		schema.NewInt64Node("timestamp", parquet.Repetitions.Required, -1),
		schema.NewInt64Node("end_time", parquet.Repetitions.Required, -1),
		schema.NewByteArrayNode("trace_id", parquet.Repetitions.Required, -1),
		schema.NewByteArrayNode("span_id", parquet.Repetitions.Required, -1),
		schema.NewByteArrayNode("parent_span_id", parquet.Repetitions.Required, -1),
		schema.NewByteArrayNode("service", parquet.Repetitions.Required, -1),
		schema.NewByteArrayNode("name", parquet.Repetitions.Required, -1),
		schema.NewByteArrayNode("status_code", parquet.Repetitions.Required, -1),
		schema.NewByteArrayNode("attributes_json", parquet.Repetitions.Required, -1),
	}
	parquetSchema, err := schema.NewGroupNode("span_record", parquet.Repetitions.Required, fields, -1)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	props := parquet.NewWriterProperties(
		parquet.WithCompression(compress.Codecs.Zstd),
		parquet.WithDictionaryDefault(true),
	)
	writer := file.NewParquetWriter(&buf, parquetSchema, file.WithWriterProps(props))
	defer writer.Close()
	rgw := writer.AppendRowGroup()

	writeByteArrayCol := func(vals []parquet.ByteArray) error {
		cw, err := rgw.NextColumn()
		if err != nil {
			return err
		}
		w := cw.(*file.ByteArrayColumnChunkWriter)
		w.WriteBatch(vals, nil, nil)
		w.Close()
		return nil
	}
	writeInt64Col := func(vals []int64) error {
		cw, err := rgw.NextColumn()
		if err != nil {
			return err
		}
		w := cw.(*file.Int64ColumnChunkWriter)
		w.WriteBatch(vals, nil, nil)
		w.Close()
		return nil
	}

	n := len(spans)
	timestamps := make([]int64, n)
	endTimes := make([]int64, n)
	traceIDs := make([]parquet.ByteArray, n)
	spanIDs := make([]parquet.ByteArray, n)
	parentSpanIDs := make([]parquet.ByteArray, n)
	services := make([]parquet.ByteArray, n)
	names := make([]parquet.ByteArray, n)
	statusCodes := make([]parquet.ByteArray, n)
	attrJSONs := make([]parquet.ByteArray, n)

	for idx, s := range spans {
		timestamps[idx] = s.Timestamp.UnixNano()
		endTimes[idx] = s.EndTime.UnixNano()
		traceIDs[idx] = parquet.ByteArray(s.TraceID)
		spanIDs[idx] = parquet.ByteArray(s.SpanID)
		parentSpanIDs[idx] = parquet.ByteArray(s.ParentSpanID)
		services[idx] = parquet.ByteArray(s.Service)
		names[idx] = parquet.ByteArray(s.Name)
		statusCodes[idx] = parquet.ByteArray(s.StatusCode)
		b, _ := json.Marshal(s.Attributes)
		attrJSONs[idx] = b
	}

	for _, fn := range []func() error{
		func() error { return writeInt64Col(timestamps) },
		func() error { return writeInt64Col(endTimes) },
		func() error { return writeByteArrayCol(traceIDs) },
		func() error { return writeByteArrayCol(spanIDs) },
		func() error { return writeByteArrayCol(parentSpanIDs) },
		func() error { return writeByteArrayCol(services) },
		func() error { return writeByteArrayCol(names) },
		func() error { return writeByteArrayCol(statusCodes) },
		func() error { return writeByteArrayCol(attrJSONs) },
	} {
		if err := fn(); err != nil {
			return err
		}
	}

	if err := rgw.Close(); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return i.store.Write(ctx, path, &buf)
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
	if len(spans) == 0 {
		return nil
	}
	const bloomBytes = 256
	const bloomHashes = 4

	bloom := make([]byte, bloomBytes)
	for _, span := range spans {
		if span.TraceID == "" {
			continue
		}
		indexes := bloomIndexes(span.TraceID, bloomBytes*8, bloomHashes)
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
