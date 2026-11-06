package logengine

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/compress"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/schema"
)

const defaultParquetRowGroupRows = 256_000

func parquetStringNode(name string) *schema.PrimitiveNode {
	return schema.MustPrimitive(schema.NewPrimitiveNodeLogical(
		name,
		parquet.Repetitions.Required,
		schema.StringLogicalType{},
		parquet.Types.ByteArray,
		-1,
		-1,
	))
}

func (i *Ingester) parquetRowGroupRows() int {
	if i.rowGroupRows > 0 {
		return i.rowGroupRows
	}
	return defaultParquetRowGroupRows
}

func writeLogRowGroup(rgw file.SerialRowGroupWriter, logs []LogRecord) error {
	writeInt64 := func(values []int64) error {
		cw, err := rgw.NextColumn()
		if err != nil {
			return err
		}
		writer := cw.(*file.Int64ColumnChunkWriter)
		writer.WriteBatch(values, nil, nil)
		return writer.Close()
	}
	writeBytes := func(values []parquet.ByteArray) error {
		cw, err := rgw.NextColumn()
		if err != nil {
			return err
		}
		writer := cw.(*file.ByteArrayColumnChunkWriter)
		writer.WriteBatch(values, nil, nil)
		return writer.Close()
	}

	n := len(logs)
	timestamps := make([]int64, n)
	levels := make([]parquet.ByteArray, n)
	services := make([]parquet.ByteArray, n)
	traceIDs := make([]parquet.ByteArray, n)
	messages := make([]parquet.ByteArray, n)
	spanIDs := make([]parquet.ByteArray, n)
	labels := make([]parquet.ByteArray, n)
	partitions := make([]parquet.ByteArray, n)
	for idx, record := range logs {
		timestamps[idx] = record.Timestamp.UnixNano()
		levels[idx] = parquet.ByteArray(record.Level)
		services[idx] = parquet.ByteArray(record.Service)
		traceIDs[idx] = parquet.ByteArray(record.TraceID)
		messages[idx] = parquet.ByteArray(record.RawMessage)
		spanIDs[idx] = parquet.ByteArray(record.SpanID)
		encoded, _ := json.Marshal(record.Labels)
		labels[idx] = encoded
		partitions[idx] = parquet.ByteArray(recordPartition(record.Labels))
	}
	for _, write := range []func() error{
		func() error { return writeInt64(timestamps) },
		func() error { return writeBytes(levels) },
		func() error { return writeBytes(services) },
		func() error { return writeBytes(traceIDs) },
		func() error { return writeBytes(messages) },
		func() error { return writeBytes(spanIDs) },
		func() error { return writeBytes(labels) },
		func() error { return writeBytes(partitions) },
	} {
		if err := write(); err != nil {
			return err
		}
	}
	return nil
}

func writeMetricRowGroup(rgw file.SerialRowGroupWriter, metrics []MetricRecord) error {
	writeInt64 := func(values []int64) error {
		cw, err := rgw.NextColumn()
		if err != nil {
			return err
		}
		writer := cw.(*file.Int64ColumnChunkWriter)
		writer.WriteBatch(values, nil, nil)
		return writer.Close()
	}
	writeFloat64 := func(values []float64) error {
		cw, err := rgw.NextColumn()
		if err != nil {
			return err
		}
		writer := cw.(*file.Float64ColumnChunkWriter)
		writer.WriteBatch(values, nil, nil)
		return writer.Close()
	}
	writeBytes := func(values []parquet.ByteArray) error {
		cw, err := rgw.NextColumn()
		if err != nil {
			return err
		}
		writer := cw.(*file.ByteArrayColumnChunkWriter)
		writer.WriteBatch(values, nil, nil)
		return writer.Close()
	}

	n := len(metrics)
	timestamps := make([]int64, n)
	services := make([]parquet.ByteArray, n)
	names := make([]parquet.ByteArray, n)
	values := make([]float64, n)
	labels := make([]parquet.ByteArray, n)
	partitions := make([]parquet.ByteArray, n)
	metricTypes := make([]parquet.ByteArray, n)
	temporalities := make([]parquet.ByteArray, n)
	startTimes := make([]int64, n)
	for idx, record := range metrics {
		timestamps[idx] = record.Timestamp.UnixNano()
		services[idx] = parquet.ByteArray(record.Service)
		names[idx] = parquet.ByteArray(record.MetricName)
		values[idx] = record.Value
		encoded, _ := json.Marshal(record.Labels)
		labels[idx] = encoded
		partitions[idx] = parquet.ByteArray(recordPartition(record.Labels))
		metricTypes[idx] = parquet.ByteArray(record.MetricType)
		temporalities[idx] = parquet.ByteArray(record.Temporality)
		if !record.StartTime.IsZero() {
			startTimes[idx] = record.StartTime.UnixNano()
		}
	}
	for _, write := range []func() error{
		func() error { return writeInt64(timestamps) },
		func() error { return writeBytes(services) },
		func() error { return writeBytes(names) },
		func() error { return writeFloat64(values) },
		func() error { return writeBytes(labels) },
		func() error { return writeBytes(partitions) },
		func() error { return writeBytes(metricTypes) },
		func() error { return writeBytes(temporalities) },
		func() error { return writeInt64(startTimes) },
	} {
		if err := write(); err != nil {
			return err
		}
	}
	return nil
}

func writeSpanRowGroup(rgw file.SerialRowGroupWriter, spans []SpanRecord) error {
	writeInt64 := func(values []int64) error {
		cw, err := rgw.NextColumn()
		if err != nil {
			return err
		}
		writer := cw.(*file.Int64ColumnChunkWriter)
		writer.WriteBatch(values, nil, nil)
		return writer.Close()
	}
	writeBytes := func(values []parquet.ByteArray) error {
		cw, err := rgw.NextColumn()
		if err != nil {
			return err
		}
		writer := cw.(*file.ByteArrayColumnChunkWriter)
		writer.WriteBatch(values, nil, nil)
		return writer.Close()
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
	attributes := make([]parquet.ByteArray, n)
	partitions := make([]parquet.ByteArray, n)
	for idx, record := range spans {
		timestamps[idx] = record.Timestamp.UnixNano()
		endTimes[idx] = record.EndTime.UnixNano()
		traceIDs[idx] = parquet.ByteArray(record.TraceID)
		spanIDs[idx] = parquet.ByteArray(record.SpanID)
		parentSpanIDs[idx] = parquet.ByteArray(record.ParentSpanID)
		services[idx] = parquet.ByteArray(record.Service)
		names[idx] = parquet.ByteArray(record.Name)
		statusCodes[idx] = parquet.ByteArray(record.StatusCode)
		encoded, _ := json.Marshal(record.Attributes)
		attributes[idx] = encoded
		partitions[idx] = parquet.ByteArray(recordPartition(record.Attributes))
	}
	for _, write := range []func() error{
		func() error { return writeInt64(timestamps) },
		func() error { return writeInt64(endTimes) },
		func() error { return writeBytes(traceIDs) },
		func() error { return writeBytes(spanIDs) },
		func() error { return writeBytes(parentSpanIDs) },
		func() error { return writeBytes(services) },
		func() error { return writeBytes(names) },
		func() error { return writeBytes(statusCodes) },
		func() error { return writeBytes(attributes) },
		func() error { return writeBytes(partitions) },
	} {
		if err := write(); err != nil {
			return err
		}
	}
	return nil
}

func (i *Ingester) writeLogParquetBatched(ctx context.Context, path string, logs []LogRecord) (int64, int, error) {
	parquetSchema, err := schema.NewGroupNode("log_record", parquet.Repetitions.Required, schema.FieldList{
		schema.NewInt64Node("timestamp", parquet.Repetitions.Required, -1),
		parquetStringNode("level"),
		parquetStringNode("service"),
		parquetStringNode("trace_id"),
		parquetStringNode("raw_message"),
		parquetStringNode("span_id"),
		parquetStringNode("labels_json"),
		parquetStringNode("partition_name"),
	}, -1)
	if err != nil {
		return 0, 0, err
	}
	props := parquet.NewWriterProperties(
		parquet.WithCompression(compress.Codecs.Zstd),
		parquet.WithCompressionLevel(3),
		parquet.WithDictionaryDefault(true),
		parquet.WithBloomFilterEnabledFor("trace_id", true),
		parquet.WithBloomFilterEnabledFor("span_id", true),
		parquet.WithBloomFilterFPPFor("trace_id", 0.01),
		parquet.WithBloomFilterFPPFor("span_id", 0.01),
		parquet.WithPageIndexEnabledFor("timestamp", true),
	)
	var buf bytes.Buffer
	writer := file.NewParquetWriter(&buf, parquetSchema, file.WithWriterProps(props))
	rowGroups := 0
	for start := 0; start < len(logs); {
		end := start + i.parquetRowGroupRows()
		if end > len(logs) {
			end = len(logs)
		}
		rgw := writer.AppendRowGroup()
		if err := writeLogRowGroup(rgw, logs[start:end]); err != nil {
			_ = writer.Close()
			return 0, rowGroups, err
		}
		if err := rgw.Close(); err != nil {
			_ = writer.Close()
			return 0, rowGroups, err
		}
		rowGroups++
		start = end
	}
	if err := writer.Close(); err != nil {
		return 0, rowGroups, err
	}
	size := int64(buf.Len())
	if err := i.store.Write(ctx, path, &buf); err != nil {
		return 0, rowGroups, err
	}
	return size, rowGroups, nil
}

func (i *Ingester) writeMetricParquetBatched(ctx context.Context, path string, metrics []MetricRecord) (int64, int, error) {
	parquetSchema, err := schema.NewGroupNode("metric_record", parquet.Repetitions.Required, schema.FieldList{
		schema.NewInt64Node("timestamp", parquet.Repetitions.Required, -1),
		parquetStringNode("service"),
		parquetStringNode("metric_name"),
		schema.NewFloat64Node("value", parquet.Repetitions.Required, -1),
		parquetStringNode("labels_json"),
		parquetStringNode("partition_name"),
		parquetStringNode("metric_type"),
		parquetStringNode("temporality"),
		schema.NewInt64Node("start_time", parquet.Repetitions.Required, -1),
	}, -1)
	if err != nil {
		return 0, 0, err
	}
	props := parquet.NewWriterProperties(
		parquet.WithCompression(compress.Codecs.Zstd),
		parquet.WithCompressionLevel(3),
		parquet.WithDictionaryDefault(true),
		parquet.WithPageIndexEnabledFor("timestamp", true),
	)
	var buf bytes.Buffer
	writer := file.NewParquetWriter(&buf, parquetSchema, file.WithWriterProps(props))
	rowGroups := 0
	for start := 0; start < len(metrics); {
		end := start + i.parquetRowGroupRows()
		if end > len(metrics) {
			end = len(metrics)
		}
		rgw := writer.AppendRowGroup()
		if err := writeMetricRowGroup(rgw, metrics[start:end]); err != nil {
			_ = writer.Close()
			return 0, rowGroups, err
		}
		if err := rgw.Close(); err != nil {
			_ = writer.Close()
			return 0, rowGroups, err
		}
		rowGroups++
		start = end
	}
	if err := writer.Close(); err != nil {
		return 0, rowGroups, err
	}
	size := int64(buf.Len())
	if err := i.store.Write(ctx, path, &buf); err != nil {
		return 0, rowGroups, err
	}
	return size, rowGroups, nil
}

func (i *Ingester) writeTraceParquetBatched(ctx context.Context, path string, spans []SpanRecord) (int64, int, error) {
	parquetSchema, err := schema.NewGroupNode("span_record", parquet.Repetitions.Required, schema.FieldList{
		schema.NewInt64Node("timestamp", parquet.Repetitions.Required, -1),
		schema.NewInt64Node("end_time", parquet.Repetitions.Required, -1),
		parquetStringNode("trace_id"),
		parquetStringNode("span_id"),
		parquetStringNode("parent_span_id"),
		parquetStringNode("service"),
		parquetStringNode("name"),
		parquetStringNode("status_code"),
		parquetStringNode("attributes_json"),
		parquetStringNode("partition_name"),
	}, -1)
	if err != nil {
		return 0, 0, err
	}
	props := parquet.NewWriterProperties(
		parquet.WithCompression(compress.Codecs.Zstd),
		parquet.WithCompressionLevel(3),
		parquet.WithDictionaryDefault(true),
		parquet.WithBloomFilterEnabledFor("trace_id", true),
		parquet.WithBloomFilterEnabledFor("span_id", true),
		parquet.WithBloomFilterFPPFor("trace_id", 0.01),
		parquet.WithBloomFilterFPPFor("span_id", 0.01),
		parquet.WithPageIndexEnabledFor("timestamp", true),
	)
	var buf bytes.Buffer
	writer := file.NewParquetWriter(&buf, parquetSchema, file.WithWriterProps(props))
	rowGroups := 0
	for start := 0; start < len(spans); {
		end := start + i.parquetRowGroupRows()
		if end > len(spans) {
			end = len(spans)
		}
		rgw := writer.AppendRowGroup()
		if err := writeSpanRowGroup(rgw, spans[start:end]); err != nil {
			_ = writer.Close()
			return 0, rowGroups, err
		}
		if err := rgw.Close(); err != nil {
			_ = writer.Close()
			return 0, rowGroups, err
		}
		rowGroups++
		start = end
	}
	if err := writer.Close(); err != nil {
		return 0, rowGroups, err
	}
	size := int64(buf.Len())
	if err := i.store.Write(ctx, path, &buf); err != nil {
		return 0, rowGroups, err
	}
	return size, rowGroups, nil
}
