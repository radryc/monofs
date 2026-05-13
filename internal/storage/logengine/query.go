package logengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
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

type candidateChunk struct {
	chunkID   string
	manifest  ChunkManifest
	cutoffMax time.Time
}

type compiledMetricMatcher struct {
	matcher *labels.Matcher
	name    string
}

const (
	metricDiscoveryMatcherName = "__doctor_discovery__"
	metricDiscoveryModeNames   = "metric_names"
)

// NewQueryEngine creates a new QueryEngine.
func NewQueryEngine(store *CachedStore) *QueryEngine {
	return &QueryEngine{
		store: store,
	}
}

// Query executes a LogQL query and is kept for backward compatibility.
func (q *QueryEngine) Query(ctx context.Context, queryStr string) ([]LogRecord, error) {
	return q.QueryLogs(ctx, queryStr, "", time.Time{}, time.Time{}, 0)
}

// QueryLogs executes a LogQL query over the log chunks.
// limit 0 means no limit.
func (q *QueryEngine) QueryLogs(ctx context.Context, queryStr, service string, from, to time.Time, limit int) ([]LogRecord, error) {
	observer := beginQueryPathObservation(SignalLogs)
	defer observer.finish()

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
	if service != "" {
		if serviceFilter != "" && serviceFilter != service {
			return []LogRecord{}, nil
		}
		serviceFilter = service
	}

	// 2. Fetch all metadata manifests (Time Pruning)
	listStart := time.Now()
	chunkIDs, err := q.store.ListChunks(ctx, "chunks/logs/")
	observer.observeStage("chunk_listing", listStart)
	if err != nil {
		return nil, fmt.Errorf("failed to list chunks: %w", err)
	}
	observer.addChunksListed(len(chunkIDs))

	candidates, err := q.logCandidates(ctx, chunkIDs, serviceFilter, from, to, observer)
	if err != nil {
		return nil, err
	}

	var results []LogRecord

	for _, candidate := range candidates {
		if limit > 0 && len(results) >= limit && candidate.cutoffMax.Before(results[limit-1].Timestamp) {
			break
		}

		// 3. Free-Text Phase (Bluge)
		var validRows *roaring.Bitmap

		if textFilter != "" {
			indexPath := filepath.Join("chunks", string(SignalLogs), candidate.chunkID, "text.index.tar.gz")

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
		parquetPath := filepath.Join("chunks", string(SignalLogs), candidate.chunkID, "data.parquet")

		chunkResults, err := q.readParquet(ctx, parquetPath, validRows, serviceFilter, from, to, observer)
		if err != nil {
			if errors.Is(err, ErrGhostChunk) {
				continue
			}
			return nil, err
		}

		results = append(results, chunkResults...)
		if limit > 0 && len(results) > limit {
			sort.Slice(results, func(i, j int) bool {
				return results[i].Timestamp.After(results[j].Timestamp)
			})
			results = results[:limit]
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Timestamp.After(results[j].Timestamp)
	})
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	observer.addReturnedRecords(len(results))
	return results, nil
}

func (q *QueryEngine) readParquet(ctx context.Context, path string, validRows *roaring.Bitmap, serviceFilter string, from, to time.Time, observer *queryPathObserver) ([]LogRecord, error) {
	rdr, rc, err := q.openSignalParquet(ctx, path, observer)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
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
			ts := time.Unix(0, tsBuf[i])
			if !from.IsZero() && ts.Before(from) {
				continue
			}
			if !to.IsZero() && ts.After(to) {
				continue
			}
			svc := string(svcBuf[i])
			if serviceFilter != "" && svc != serviceFilter {
				continue
			}
			records = append(records, LogRecord{
				Timestamp:  ts,
				Level:      string(lvlBuf[i]),
				Service:    svc,
				TraceID:    string(traceBuf[i]),
				RawMessage: string(msgBuf[i]),
			})
		}
	}
	return records, nil
}

func (q *QueryEngine) logCandidates(ctx context.Context, chunkIDs []string, service string, from, to time.Time, observer *queryPathObserver) ([]candidateChunk, error) {
	pruneStart := time.Now()
	defer observer.observeStage("manifest_pruning", pruneStart)

	candidates := make([]candidateChunk, 0, len(chunkIDs))
	for _, chunkID := range chunkIDs {
		manifest, err := q.store.ReadManifest(ctx, SignalLogs, chunkID)
		if err != nil {
			if errors.Is(err, ErrGhostChunk) {
				observer.addChunksPruned("ghost_chunk", 1)
				continue
			}
			return nil, err
		}
		if !to.IsZero() && manifest.MinTime.After(to) {
			observer.addChunksPruned("after_range", 1)
			continue
		}
		if !from.IsZero() && manifest.MaxTime.Before(from) {
			observer.addChunksPruned("before_range", 1)
			continue
		}
		if service != "" && !manifestContains(manifest.Services, service) {
			observer.addChunksPruned("service_mismatch", 1)
			continue
		}
		candidates = append(candidates, candidateChunk{chunkID: chunkID, manifest: manifest, cutoffMax: manifest.MaxTime})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].manifest.MaxTime.After(candidates[j].manifest.MaxTime)
	})
	return candidates, nil
}

// QueryMetrics returns metric data points matching the given query in the time range.
func (q *QueryEngine) QueryMetrics(ctx context.Context, query MetricQuery, from, to time.Time) ([]MetricRecord, error) {
	query, discoveryMode := stripMetricDiscoveryMatchers(query)
	if discoveryMode == metricDiscoveryModeNames {
		return q.discoverMetricNames(ctx, query, from, to)
	}

	observer := beginQueryPathObservation(SignalMetrics)
	defer observer.finish()

	listStart := time.Now()
	chunkIDs, err := q.store.ListChunks(ctx, "chunks/metrics/")
	observer.observeStage("chunk_listing", listStart)
	if err != nil {
		return nil, fmt.Errorf("failed to list metric chunks: %w", err)
	}
	observer.addChunksListed(len(chunkIDs))
	compiledMatchers, err := compileMetricMatchers(query.LabelMatchers)
	if err != nil {
		return nil, err
	}
	candidates, err := q.metricCandidates(ctx, chunkIDs, query, compiledMatchers, from, to, observer)
	if err != nil {
		return nil, err
	}

	var results []MetricRecord
	for _, candidate := range candidates {
		points, err := q.readMetricParquet(ctx, filepath.Join("chunks", string(SignalMetrics), candidate.chunkID, "data.parquet"), query, compiledMatchers, from, to, observer)
		if err != nil {
			if errors.Is(err, ErrGhostChunk) {
				continue
			}
			return nil, err
		}
		results = append(results, points...)
	}
	observer.addReturnedRecords(len(results))
	return results, nil
}

func (q *QueryEngine) discoverMetricNames(ctx context.Context, query MetricQuery, from, to time.Time) ([]MetricRecord, error) {
	observer := beginQueryPathObservation(SignalMetrics)
	defer observer.finish()

	listStart := time.Now()
	chunkIDs, err := q.store.ListChunks(ctx, "chunks/metrics/")
	observer.observeStage("chunk_listing", listStart)
	if err != nil {
		return nil, fmt.Errorf("failed to list metric chunks: %w", err)
	}
	observer.addChunksListed(len(chunkIDs))

	compiledMatchers, err := compileMetricMatchers(query.LabelMatchers)
	if err != nil {
		return nil, err
	}
	candidates, err := q.metricCandidates(ctx, chunkIDs, query, compiledMatchers, from, to, observer)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	results := make([]MetricRecord, 0)
	for _, candidate := range candidates {
		for _, name := range candidate.manifest.MetricNames {
			if name == "" {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			results = append(results, MetricRecord{MetricName: name})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].MetricName < results[j].MetricName
	})
	observer.addReturnedRecords(len(results))
	return results, nil
}

func (q *QueryEngine) metricCandidates(ctx context.Context, chunkIDs []string, query MetricQuery, compiledMatchers []compiledMetricMatcher, from, to time.Time, observer *queryPathObserver) ([]candidateChunk, error) {
	pruneStart := time.Now()
	defer observer.observeStage("manifest_pruning", pruneStart)

	candidates := make([]candidateChunk, 0, len(chunkIDs))
	for _, chunkID := range chunkIDs {
		manifest, err := q.store.ReadManifest(ctx, SignalMetrics, chunkID)
		if err != nil {
			if errors.Is(err, ErrGhostChunk) {
				observer.addChunksPruned("ghost_chunk", 1)
				continue
			}
			return nil, err
		}

		if !to.IsZero() && manifest.MinTime.After(to) {
			observer.addChunksPruned("after_range", 1)
			continue
		}
		if !from.IsZero() && manifest.MaxTime.Before(from) {
			observer.addChunksPruned("before_range", 1)
			continue
		}
		if query.MetricName != "" && !manifestContains(manifest.MetricNames, query.MetricName) {
			observer.addChunksPruned("metric_mismatch", 1)
			continue
		}
		if query.Service != "" && !manifestContains(manifest.Services, query.Service) {
			observer.addChunksPruned("service_mismatch", 1)
			continue
		}
		if !manifestMatchesMetricLabels(manifest, compiledMatchers) {
			observer.addChunksPruned("label_mismatch", 1)
			continue
		}
		candidates = append(candidates, candidateChunk{chunkID: chunkID, manifest: manifest, cutoffMax: manifest.MaxTime})
	}
	return candidates, nil
}

func (q *QueryEngine) readMetricParquet(ctx context.Context, path string, query MetricQuery, matchers []compiledMetricMatcher, from, to time.Time, observer *queryPathObserver) ([]MetricRecord, error) {
	rdr, rc, err := q.openSignalParquet(ctx, path, observer)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
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
			svc := string(svcBuf[i])
			if query.Service != "" && svc != query.Service {
				continue
			}
			name := string(nameBuf[i])
			if query.MetricName != "" && name != query.MetricName {
				continue
			}
			var labels map[string]string
			if len(lblBuf[i]) > 0 || len(matchers) > 0 {
				json.Unmarshal(lblBuf[i], &labels) //nolint:errcheck
			}
			if !metricLabelsMatch(name, svc, labels, matchers) {
				continue
			}
			records = append(records, MetricRecord{
				Timestamp:  ts,
				Service:    svc,
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
	observer := beginQueryPathObservation(SignalTraces)
	defer observer.finish()

	listStart := time.Now()
	chunkIDs, err := q.store.ListChunks(ctx, "chunks/traces/")
	observer.observeStage("chunk_listing", listStart)
	if err != nil {
		return nil, fmt.Errorf("failed to list trace chunks: %w", err)
	}
	observer.addChunksListed(len(chunkIDs))
	candidates, err := q.traceCandidates(ctx, chunkIDs, traceID, service, from, to, observer)
	if err != nil {
		return nil, err
	}

	var results []SpanRecord
	for _, candidate := range candidates {
		if limit > 0 && len(results) >= limit && candidate.cutoffMax.Before(results[limit-1].Timestamp) {
			break
		}

		spans, err := q.readTraceParquet(ctx, filepath.Join("chunks", string(SignalTraces), candidate.chunkID, "data.parquet"), traceID, service, from, to, limit-len(results), observer)
		if err != nil {
			if errors.Is(err, ErrGhostChunk) {
				continue
			}
			return nil, err
		}
		results = append(results, spans...)
		if limit > 0 && len(results) > limit {
			sort.Slice(results, func(i, j int) bool {
				return results[i].Timestamp.After(results[j].Timestamp)
			})
			results = results[:limit]
		}
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].Timestamp.After(results[j].Timestamp)
	})
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	observer.addReturnedRecords(len(results))
	return results, nil
}

func (q *QueryEngine) traceCandidates(ctx context.Context, chunkIDs []string, traceID, service string, from, to time.Time, observer *queryPathObserver) ([]candidateChunk, error) {
	pruneStart := time.Now()
	defer observer.observeStage("manifest_pruning", pruneStart)

	candidates := make([]candidateChunk, 0, len(chunkIDs))
	for _, chunkID := range chunkIDs {
		manifest, err := q.store.ReadManifest(ctx, SignalTraces, chunkID)
		if err != nil {
			if errors.Is(err, ErrGhostChunk) {
				observer.addChunksPruned("ghost_chunk", 1)
				continue
			}
			return nil, err
		}
		if !to.IsZero() && manifest.MinTime.After(to) {
			observer.addChunksPruned("after_range", 1)
			continue
		}
		if !from.IsZero() && manifest.MaxTime.Before(from) {
			observer.addChunksPruned("before_range", 1)
			continue
		}
		if service != "" && !manifestContains(manifest.Services, service) {
			observer.addChunksPruned("service_mismatch", 1)
			continue
		}
		if traceID != "" && !traceBloomMayContain(manifest.TraceBloom, traceID) {
			observer.addChunksPruned("trace_bloom_miss", 1)
			continue
		}
		candidates = append(candidates, candidateChunk{chunkID: chunkID, manifest: manifest, cutoffMax: manifest.MaxTime})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].manifest.MaxTime.After(candidates[j].manifest.MaxTime)
	})
	return candidates, nil
}

func (q *QueryEngine) readTraceParquet(ctx context.Context, path, traceID, service string, from, to time.Time, remaining int, observer *queryPathObserver) ([]SpanRecord, error) {
	rdr, rc, err := q.openSignalParquet(ctx, path, observer)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
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

func (q *QueryEngine) openSignalParquet(ctx context.Context, path string, observer *queryPathObserver) (*file.Reader, io.ReadSeekCloser, error) {
	openStart := time.Now()
	rc, err := q.store.remote.Read(ctx, path)
	if err != nil {
		observer.observeStage("parquet_open", openStart)
		observer.addParquetOpen("error")
		return nil, nil, err
	}

	rdr, err := openParquetReader(rc)
	observer.observeStage("parquet_open", openStart)
	if err != nil {
		observer.addParquetOpen("error")
		rc.Close()
		return nil, nil, fmt.Errorf("open parquet: %w", err)
	}
	observer.addParquetOpen("success")
	return rdr, rc, nil
}

func openParquetReader(reader io.ReadSeekCloser) (*file.Reader, error) {
	if seekable, ok := reader.(parquet.ReaderAtSeeker); ok {
		return file.NewParquetReader(seekable)
	}

	return nil, fmt.Errorf("reader does not support parquet seeking")
}

func compileMetricMatchers(matchers []MetricLabelMatcher) ([]compiledMetricMatcher, error) {
	compiled := make([]compiledMetricMatcher, 0, len(matchers))
	for _, matcher := range matchers {
		matchType, err := metricMatcherTypeToProm(matcher.Type)
		if err != nil {
			return nil, err
		}
		promMatcher, err := labels.NewMatcher(matchType, matcher.Name, matcher.Value)
		if err != nil {
			return nil, err
		}
		compiled = append(compiled, compiledMetricMatcher{matcher: promMatcher, name: matcher.Name})
	}
	return compiled, nil
}

func metricMatcherTypeToProm(matchType MetricMatchType) (labels.MatchType, error) {
	switch matchType {
	case "", MetricMatchEqual:
		return labels.MatchEqual, nil
	case MetricMatchNotEqual:
		return labels.MatchNotEqual, nil
	case MetricMatchRegexp:
		return labels.MatchRegexp, nil
	case MetricMatchNotRegexp:
		return labels.MatchNotRegexp, nil
	default:
		return labels.MatchEqual, fmt.Errorf("unsupported metric matcher type %q", matchType)
	}
}

func stripMetricDiscoveryMatchers(query MetricQuery) (MetricQuery, string) {
	if len(query.LabelMatchers) == 0 {
		return query, ""
	}

	filtered := query
	filtered.LabelMatchers = make([]MetricLabelMatcher, 0, len(query.LabelMatchers))
	mode := ""
	for _, matcher := range query.LabelMatchers {
		if matcher.Name == metricDiscoveryMatcherName && matcher.Type == MetricMatchEqual {
			mode = matcher.Value
			continue
		}
		filtered.LabelMatchers = append(filtered.LabelMatchers, matcher)
	}
	if len(filtered.LabelMatchers) == 0 {
		filtered.LabelMatchers = nil
	}
	return filtered, mode
}

func manifestContains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func manifestMatchesMetricLabels(manifest ChunkManifest, matchers []compiledMetricMatcher) bool {
	for _, matcher := range matchers {
		if matcher.matcher.Type != labels.MatchEqual {
			continue
		}
		if matcher.name == labels.MetricName || matcher.name == "service" {
			continue
		}
		values := manifest.MetricLabelValues[matcher.name]
		if len(values) == 0 || !manifestContains(values, matcher.matcher.Value) {
			return false
		}
	}
	return true
}

func metricLabelsMatch(metricName, service string, values map[string]string, matchers []compiledMetricMatcher) bool {
	for _, matcher := range matchers {
		var value string
		switch matcher.name {
		case labels.MetricName:
			value = metricName
		case "service":
			value = service
		default:
			value = values[matcher.name]
		}
		if !matcher.matcher.Matches(value) {
			return false
		}
	}
	return true
}

func traceBloomMayContain(bloom []byte, traceID string) bool {
	if len(bloom) == 0 || traceID == "" {
		return true
	}
	indexes := bloomIndexes(traceID, len(bloom)*8, 4)
	for _, idx := range indexes {
		byteIdx := idx / 8
		bitIdx := idx % 8
		if bloom[byteIdx]&(1<<bitIdx) == 0 {
			return false
		}
	}
	return true
}
