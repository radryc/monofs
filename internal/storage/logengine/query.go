package logengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/blugelabs/bluge"
	"github.com/grafana/loki/v3/pkg/logql/syntax"
	"github.com/prometheus/prometheus/model/labels"
)

// QueryEngine handles parsing and executing LogQL queries against the storage.
type QueryEngine struct {
	store *CachedStore
}

// NewQueryEngine creates a new QueryEngine.
func NewQueryEngine(store *CachedStore) *QueryEngine {
	return &QueryEngine{
		store: store,
	}
}

// Query executes a LogQL query and is kept for backward compatibility.
func (q *QueryEngine) Query(ctx context.Context, queryStr string) ([]LogRecord, error) {
	return q.QueryLogs(ctx, queryStr, 0)
}

// QueryLogs executes a LogQL query over the log chunks.
// limit 0 means no limit.
func (q *QueryEngine) QueryLogs(ctx context.Context, queryStr string, limit int) ([]LogRecord, error) {
	// 1. Parse Query using Loki's logql syntax
	expr, err := syntax.ParseExpr(queryStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse LogQL: %w", err)
	}

	var textFilter string
	var serviceFilter string

	var extract func(e syntax.Expr)
	extract = func(e syntax.Expr) {
		switch node := e.(type) {
		case *syntax.PipelineExpr:
			extract(node.Left)
			for _, stage := range node.MultiStages {
				if lf, ok := stage.(*syntax.LineFilterExpr); ok {
					textFilter = lf.Match
				}
			}
		case *syntax.MatchersExpr:
			for _, m := range node.Matchers() {
				if m.Name == "service" && m.Type == labels.MatchEqual {
					serviceFilter = m.Value
				}
			}
		}
	}
	extract(expr)

	// 2. Fetch all metadata manifests (Time Pruning)
	chunkIDs, err := q.store.ListChunks(ctx, "chunks/logs/")
	if err != nil {
		return nil, fmt.Errorf("failed to list chunks: %w", err)
	}

	var results []LogRecord

	for _, chunkID := range chunkIDs {
		if limit > 0 && len(results) >= limit {
			break
		}
		// Load Metadata
		manifestPath := filepath.Join("chunks", string(SignalLogs), chunkID, "metadata.json")
		rc, err := q.store.remote.Read(ctx, manifestPath)
		if err != nil {
			if errors.Is(err, ErrGhostChunk) {
				// Handle "Ghost Chunks" silently
				continue
			}
			return nil, err
		}

		var manifest ChunkManifest
		if err := json.NewDecoder(rc).Decode(&manifest); err != nil {
			rc.Close()
			return nil, err
		}
		rc.Close()

		// (Time pruning would happen here comparing manifest.MinTime / MaxTime)

		// 3. Free-Text Phase (Bluge)
		var validRows *roaring.Bitmap

		if textFilter != "" {
			indexPath := filepath.Join("chunks", string(SignalLogs), chunkID, "text.index.tar.gz")

			// Trigger local cache download if necessary
			localIndexPath, err := q.store.GetLocalIndexPath(ctx, indexPath)
			if err != nil {
				if errors.Is(err, ErrGhostChunk) {
					continue
				}
				return nil, err
			}

			// Query Bluge
			config := bluge.DefaultConfig(localIndexPath)
			reader, err := bluge.OpenReader(config)
			if err != nil {
				return nil, err
			}

			blugeQuery := bluge.NewMatchQuery(textFilter).SetField("raw_message")
			req := bluge.NewTopNSearch(10000, blugeQuery)

			documentMatchIterator, err := reader.Search(ctx, req)
			if err != nil {
				reader.Close()
				return nil, err
			}

			validRows = roaring.New()
			match, err := documentMatchIterator.Next()
			for err == nil && match != nil {
				validRows.Add(uint32(match.Number))
				match, err = documentMatchIterator.Next()
			}
			reader.Close()

			if validRows.IsEmpty() {
				// No matches in this chunk
				continue
			}
		}

		// 4. Structured Phase (Parquet)
		parquetPath := filepath.Join("chunks", string(SignalLogs), chunkID, "data.parquet")

		chunkResults, err := q.readParquet(ctx, parquetPath, validRows, serviceFilter)
		if err != nil {
			if errors.Is(err, ErrGhostChunk) {
				continue
			}
			return nil, err
		}

		results = append(results, chunkResults...)
	}

	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func (q *QueryEngine) readParquet(ctx context.Context, path string, validRows *roaring.Bitmap, serviceFilter string) ([]LogRecord, error) {
	rc, err := q.store.remote.Read(ctx, path)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return nil, err
	}

	rdr, err := file.NewParquetReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("open parquet: %w", err)
	}
	defer rdr.Close()

	var records []LogRecord
	for rgIdx := 0; rgIdx < rdr.NumRowGroups(); rgIdx++ {
		rg := rdr.RowGroup(rgIdx)
		n := int(rg.NumRows())
		if n == 0 {
			continue
		}

		// col 0: timestamp (int64 UnixNano)
		tsCol, err := rg.Column(0)
		if err != nil {
			return nil, err
		}
		tsBuf := make([]int64, n)
		tsCol.(*file.Int64ColumnChunkReader).ReadBatch(int64(n), tsBuf, nil, nil) //nolint:errcheck

		// col 1: level (ByteArray)
		lvlCol, err := rg.Column(1)
		if err != nil {
			return nil, err
		}
		lvlBuf := make([]parquet.ByteArray, n)
		lvlCol.(*file.ByteArrayColumnChunkReader).ReadBatch(int64(n), lvlBuf, nil, nil) //nolint:errcheck

		// col 2: service (ByteArray)
		svcCol, err := rg.Column(2)
		if err != nil {
			return nil, err
		}
		svcBuf := make([]parquet.ByteArray, n)
		svcCol.(*file.ByteArrayColumnChunkReader).ReadBatch(int64(n), svcBuf, nil, nil) //nolint:errcheck

		// col 3: trace_id (ByteArray)
		traceCol, err := rg.Column(3)
		if err != nil {
			return nil, err
		}
		traceBuf := make([]parquet.ByteArray, n)
		traceCol.(*file.ByteArrayColumnChunkReader).ReadBatch(int64(n), traceBuf, nil, nil) //nolint:errcheck

		// col 4: raw_message (ByteArray)
		msgCol, err := rg.Column(4)
		if err != nil {
			return nil, err
		}
		msgBuf := make([]parquet.ByteArray, n)
		msgCol.(*file.ByteArrayColumnChunkReader).ReadBatch(int64(n), msgBuf, nil, nil) //nolint:errcheck

		for i := 0; i < n; i++ {
			if validRows != nil && !validRows.ContainsInt(i) {
				continue
			}
			svc := string(svcBuf[i])
			if serviceFilter != "" && svc != serviceFilter {
				continue
			}
			records = append(records, LogRecord{
				Timestamp:  time.Unix(0, tsBuf[i]),
				Level:      string(lvlBuf[i]),
				Service:    svc,
				TraceID:    string(traceBuf[i]),
				RawMessage: string(msgBuf[i]),
			})
		}
	}
	return records, nil
}

// QueryMetrics returns metric data points matching metricName in the given time range.
func (q *QueryEngine) QueryMetrics(ctx context.Context, metricName string, from, to time.Time) ([]MetricRecord, error) {
	chunkIDs, err := q.store.ListChunks(ctx, "chunks/metrics/")
	if err != nil {
		return nil, fmt.Errorf("failed to list metric chunks: %w", err)
	}

	var results []MetricRecord
	for _, chunkID := range chunkIDs {
		manifestPath := filepath.Join("chunks", string(SignalMetrics), chunkID, "metadata.json")
		rc, err := q.store.remote.Read(ctx, manifestPath)
		if err != nil {
			if errors.Is(err, ErrGhostChunk) {
				continue
			}
			return nil, err
		}
		var manifest ChunkManifest
		if err := json.NewDecoder(rc).Decode(&manifest); err != nil {
			rc.Close()
			return nil, err
		}
		rc.Close()

		// Time-range pruning
		if !to.IsZero() && manifest.MinTime.After(to) {
			continue
		}
		if !from.IsZero() && manifest.MaxTime.Before(from) {
			continue
		}

		points, err := q.readMetricParquet(ctx, filepath.Join("chunks", string(SignalMetrics), chunkID, "data.parquet"), metricName, from, to)
		if err != nil {
			if errors.Is(err, ErrGhostChunk) {
				continue
			}
			return nil, err
		}
		results = append(results, points...)
	}
	return results, nil
}

func (q *QueryEngine) readMetricParquet(ctx context.Context, path, metricName string, from, to time.Time) ([]MetricRecord, error) {
	rc, err := q.store.remote.Read(ctx, path)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return nil, err
	}

	rdr, err := file.NewParquetReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("open parquet: %w", err)
	}
	defer rdr.Close()

	var records []MetricRecord
	for rgIdx := 0; rgIdx < rdr.NumRowGroups(); rgIdx++ {
		rg := rdr.RowGroup(rgIdx)
		n := int(rg.NumRows())
		if n == 0 {
			continue
		}

		// col 0: timestamp (int64 UnixNano)
		tsCol, err := rg.Column(0)
		if err != nil {
			return nil, err
		}
		tsBuf := make([]int64, n)
		tsCol.(*file.Int64ColumnChunkReader).ReadBatch(int64(n), tsBuf, nil, nil) //nolint:errcheck

		// col 1: service (ByteArray)
		svcCol, err := rg.Column(1)
		if err != nil {
			return nil, err
		}
		svcBuf := make([]parquet.ByteArray, n)
		svcCol.(*file.ByteArrayColumnChunkReader).ReadBatch(int64(n), svcBuf, nil, nil) //nolint:errcheck

		// col 2: metric_name (ByteArray)
		nameCol, err := rg.Column(2)
		if err != nil {
			return nil, err
		}
		nameBuf := make([]parquet.ByteArray, n)
		nameCol.(*file.ByteArrayColumnChunkReader).ReadBatch(int64(n), nameBuf, nil, nil) //nolint:errcheck

		// col 3: value (float64)
		valCol, err := rg.Column(3)
		if err != nil {
			return nil, err
		}
		valBuf := make([]float64, n)
		valCol.(*file.Float64ColumnChunkReader).ReadBatch(int64(n), valBuf, nil, nil) //nolint:errcheck

		// col 4: labels_json (ByteArray)
		lblCol, err := rg.Column(4)
		if err != nil {
			return nil, err
		}
		lblBuf := make([]parquet.ByteArray, n)
		lblCol.(*file.ByteArrayColumnChunkReader).ReadBatch(int64(n), lblBuf, nil, nil) //nolint:errcheck

		for i := 0; i < n; i++ {
			ts := time.Unix(0, tsBuf[i])
			if !from.IsZero() && ts.Before(from) {
				continue
			}
			if !to.IsZero() && ts.After(to) {
				continue
			}
			name := string(nameBuf[i])
			if metricName != "" && name != metricName {
				continue
			}
			var labels map[string]string
			if len(lblBuf[i]) > 0 {
				json.Unmarshal(lblBuf[i], &labels) //nolint:errcheck
			}
			records = append(records, MetricRecord{
				Timestamp:  ts,
				Service:    string(svcBuf[i]),
				MetricName: name,
				Value:      valBuf[i],
				Labels:     labels,
			})
		}
	}
	return records, nil
}

// QueryTraces returns trace spans matching the given traceID and/or service in the time range.
func (q *QueryEngine) QueryTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int) ([]SpanRecord, error) {
	chunkIDs, err := q.store.ListChunks(ctx, "chunks/traces/")
	if err != nil {
		return nil, fmt.Errorf("failed to list trace chunks: %w", err)
	}

	var results []SpanRecord
	for _, chunkID := range chunkIDs {
		if limit > 0 && len(results) >= limit {
			break
		}
		manifestPath := filepath.Join("chunks", string(SignalTraces), chunkID, "metadata.json")
		rc, err := q.store.remote.Read(ctx, manifestPath)
		if err != nil {
			if errors.Is(err, ErrGhostChunk) {
				continue
			}
			return nil, err
		}
		var manifest ChunkManifest
		if err := json.NewDecoder(rc).Decode(&manifest); err != nil {
			rc.Close()
			return nil, err
		}
		rc.Close()

		if !to.IsZero() && manifest.MinTime.After(to) {
			continue
		}
		if !from.IsZero() && manifest.MaxTime.Before(from) {
			continue
		}

		spans, err := q.readTraceParquet(ctx, filepath.Join("chunks", string(SignalTraces), chunkID, "data.parquet"), traceID, service, from, to, limit-len(results))
		if err != nil {
			if errors.Is(err, ErrGhostChunk) {
				continue
			}
			return nil, err
		}
		results = append(results, spans...)
	}
	return results, nil
}

func (q *QueryEngine) readTraceParquet(ctx context.Context, path, traceID, service string, from, to time.Time, remaining int) ([]SpanRecord, error) {
	rc, err := q.store.remote.Read(ctx, path)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return nil, err
	}

	rdr, err := file.NewParquetReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("open parquet: %w", err)
	}
	defer rdr.Close()

	var records []SpanRecord
	for rgIdx := 0; rgIdx < rdr.NumRowGroups(); rgIdx++ {
		if remaining > 0 && len(records) >= remaining {
			break
		}
		rg := rdr.RowGroup(rgIdx)
		n := int(rg.NumRows())
		if n == 0 {
			continue
		}

		// col 0: timestamp (int64 UnixNano)
		tsCol, err := rg.Column(0)
		if err != nil {
			return nil, err
		}
		tsBuf := make([]int64, n)
		tsCol.(*file.Int64ColumnChunkReader).ReadBatch(int64(n), tsBuf, nil, nil) //nolint:errcheck

		// col 1: end_time (int64 UnixNano)
		etCol, err := rg.Column(1)
		if err != nil {
			return nil, err
		}
		etBuf := make([]int64, n)
		etCol.(*file.Int64ColumnChunkReader).ReadBatch(int64(n), etBuf, nil, nil) //nolint:errcheck

		readBytes := func(colIdx int) ([]parquet.ByteArray, error) {
			col, err := rg.Column(colIdx)
			if err != nil {
				return nil, err
			}
			buf := make([]parquet.ByteArray, n)
			col.(*file.ByteArrayColumnChunkReader).ReadBatch(int64(n), buf, nil, nil) //nolint:errcheck
			return buf, nil
		}

		traceIDs, err := readBytes(2)
		if err != nil {
			return nil, err
		}
		spanIDs, err := readBytes(3)
		if err != nil {
			return nil, err
		}
		parentSpanIDs, err := readBytes(4)
		if err != nil {
			return nil, err
		}
		services, err := readBytes(5)
		if err != nil {
			return nil, err
		}
		names, err := readBytes(6)
		if err != nil {
			return nil, err
		}
		statusCodes, err := readBytes(7)
		if err != nil {
			return nil, err
		}
		attrJSONs, err := readBytes(8)
		if err != nil {
			return nil, err
		}

		for i := 0; i < n; i++ {
			if remaining > 0 && len(records) >= remaining {
				break
			}
			ts := time.Unix(0, tsBuf[i])
			if !from.IsZero() && ts.Before(from) {
				continue
			}
			if !to.IsZero() && ts.After(to) {
				continue
			}
			tid := string(traceIDs[i])
			if traceID != "" && tid != traceID {
				continue
			}
			svc := string(services[i])
			if service != "" && svc != service {
				continue
			}
			var attrs map[string]string
			if len(attrJSONs[i]) > 0 {
				json.Unmarshal(attrJSONs[i], &attrs) //nolint:errcheck
			}
			records = append(records, SpanRecord{
				Timestamp:    ts,
				EndTime:      time.Unix(0, etBuf[i]),
				TraceID:      tid,
				SpanID:       string(spanIDs[i]),
				ParentSpanID: string(parentSpanIDs[i]),
				Service:      svc,
				Name:         string(names[i]),
				StatusCode:   string(statusCodes[i]),
				Attributes:   attrs,
			})
		}
	}
	return records, nil
}
