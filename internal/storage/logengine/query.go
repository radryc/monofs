package logengine

import (
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/blugelabs/bluge"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/radryc/monofs/internal/storage/logquery"
)

// QueryEngine handles parsing and executing MonoFS log queries against the storage.
type QueryEngine struct {
	store *CachedStore
}

type candidateChunk struct {
	chunkID   string
	manifest  ChunkManifest
	cutoffMax time.Time
}

type logRecordMinHeap []LogRecord

func (h logRecordMinHeap) Len() int { return len(h) }

func (h logRecordMinHeap) Less(i, j int) bool { return h[i].Timestamp.Before(h[j].Timestamp) }

func (h logRecordMinHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *logRecordMinHeap) Push(x any) { *h = append(*h, x.(LogRecord)) }

func (h *logRecordMinHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

type spanRecordMinHeap []SpanRecord

func (h spanRecordMinHeap) Len() int { return len(h) }

func (h spanRecordMinHeap) Less(i, j int) bool { return h[i].Timestamp.Before(h[j].Timestamp) }

func (h spanRecordMinHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *spanRecordMinHeap) Push(x any) { *h = append(*h, x.(SpanRecord)) }

func (h *spanRecordMinHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

type logChunkCursor struct {
	records []LogRecord
	index   int
}

func (c *logChunkCursor) current() LogRecord {
	return c.records[c.index]
}

type logChunkCursorHeap []*logChunkCursor

func (h logChunkCursorHeap) Len() int { return len(h) }

func (h logChunkCursorHeap) Less(i, j int) bool {
	return h[i].current().Timestamp.After(h[j].current().Timestamp)
}

func (h logChunkCursorHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *logChunkCursorHeap) Push(x any) { *h = append(*h, x.(*logChunkCursor)) }

func (h *logChunkCursorHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

type spanChunkCursor struct {
	records []SpanRecord
	index   int
}

func (c *spanChunkCursor) current() SpanRecord {
	return c.records[c.index]
}

type spanChunkCursorHeap []*spanChunkCursor

func (h spanChunkCursorHeap) Len() int { return len(h) }

func (h spanChunkCursorHeap) Less(i, j int) bool {
	return h[i].current().Timestamp.After(h[j].current().Timestamp)
}

func (h spanChunkCursorHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *spanChunkCursorHeap) Push(x any) { *h = append(*h, x.(*spanChunkCursor)) }

func (h *spanChunkCursorHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

type compiledMetricMatcher struct {
	matcher *labels.Matcher
	name    string
}

type compiledLogMatcher struct {
	matcher logquery.Matcher
	regex   *regexp.Regexp
}

type compiledLineFilter struct {
	filter logquery.LineFilter
	regex  *regexp.Regexp
}

type compiledLogQuery struct {
	query       logquery.Query
	matchers    []compiledLogMatcher
	lineFilters []compiledLineFilter
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

// Query executes a MonoFS log query and is kept for backward compatibility.
func (q *QueryEngine) Query(ctx context.Context, queryStr string) ([]LogRecord, error) {
	return q.QueryLogs(ctx, queryStr, "", time.Time{}, time.Time{}, 0)
}

func collectQueryResults[T any](stream func(func(T) error) error) ([]T, error) {
	results := make([]T, 0)
	if err := stream(func(record T) error {
		results = append(results, record)
		return nil
	}); err != nil {
		return nil, err
	}
	return results, nil
}

func emitQueryResults[T any](records []T, yield func(T) error) error {
	for _, record := range records {
		if err := yield(record); err != nil {
			return err
		}
	}
	return nil
}

// QueryLogs executes a MonoFS log query over the log chunks.
// limit 0 means no limit.
func (q *QueryEngine) QueryLogs(ctx context.Context, queryStr, service string, from, to time.Time, limit int) ([]LogRecord, error) {
	return collectQueryResults(func(yield func(LogRecord) error) error {
		return q.StreamLogs(ctx, queryStr, service, from, to, limit, yield)
	})
}

// StreamLogs executes a MonoFS log query and yields matching log records.
// limit 0 means no limit.
func (q *QueryEngine) StreamLogs(ctx context.Context, queryStr, service string, from, to time.Time, limit int, yield func(LogRecord) error) error {
	observer := beginQueryPathObservation(SignalLogs)
	defer observer.finish()

	// 1. Parse the MonoFS-compatible query subset.
	parsed, err := logquery.Parse(queryStr)
	if err != nil {
		return fmt.Errorf("failed to parse log query: %w", err)
	}
	if service != "" {
		parsed.Matchers = append(parsed.Matchers, logquery.Matcher{Name: "service", Op: "=", Value: service})
	}
	compiled, err := compileLogQuery(parsed)
	if err != nil {
		return fmt.Errorf("failed to compile log query: %w", err)
	}

	textFilters := compiled.PositiveLineContainsFilters()
	serviceFilter := compiled.ServiceEquals()

	// 2. Fetch all metadata manifests (Time Pruning)
	listStart := time.Now()
	chunkIDs, err := q.store.ListChunks(ctx, "chunks/logs/")
	observer.observeStage("chunk_listing", listStart)
	if err != nil {
		return fmt.Errorf("failed to list chunks: %w", err)
	}
	observer.addChunksListed(len(chunkIDs))

	candidates, err := q.logCandidates(ctx, chunkIDs, serviceFilter, from, to, observer)
	if err != nil {
		return err
	}

	if limit > 0 {
		top := make(logRecordMinHeap, 0, limit)
		heap.Init(&top)
		for _, candidate := range candidates {
			if top.Len() >= limit && candidate.cutoffMax.Before(top[0].Timestamp) {
				break
			}

			validRows, err := q.logValidRows(ctx, candidate.chunkID, textFilters)
			if err != nil {
				if errors.Is(err, ErrGhostChunk) {
					continue
				}
				return err
			}
			if validRows != nil && validRows.IsEmpty() {
				continue
			}

			parquetPath := filepath.Join("chunks", string(SignalLogs), candidate.chunkID, "data.parquet")
			if err := q.scanLogParquet(ctx, parquetPath, validRows, compiled, from, to, observer, func(record LogRecord) error {
				if top.Len() < limit {
					heap.Push(&top, record)
					return nil
				}
				if record.Timestamp.After(top[0].Timestamp) {
					top[0] = record
					heap.Fix(&top, 0)
				}
				return nil
			}); err != nil {
				if errors.Is(err, ErrGhostChunk) {
					continue
				}
				return err
			}
		}

		results := append([]LogRecord(nil), top...)
		sort.Slice(results, func(i, j int) bool {
			return results[i].Timestamp.After(results[j].Timestamp)
		})
		observer.addReturnedRecords(len(results))
		return emitQueryResults(results, yield)
	}

	returned, err := q.emitMergedLogChunks(ctx, candidates, compiled, textFilters, from, to, observer, yield)
	if err != nil {
		return err
	}
	observer.addReturnedRecords(returned)
	return nil
}

func (q *QueryEngine) logValidRows(ctx context.Context, chunkID string, textFilters []string) (*roaring.Bitmap, error) {
	if len(textFilters) == 0 {
		return nil, nil
	}

	indexPath := filepath.Join("chunks", string(SignalLogs), chunkID, "text.index.tar.gz")
	localIndexPath, err := q.store.GetLocalIndexPath(ctx, indexPath)
	if err != nil {
		return nil, err
	}

	config := bluge.DefaultConfig(localIndexPath)
	reader, err := bluge.OpenReader(config)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	blugeQuery := buildBlugeLineFilterQuery(textFilters)
	searchReq := bluge.NewTopNSearch(10000, blugeQuery)
	documentMatchIterator, err := reader.Search(ctx, searchReq)
	if err != nil {
		return nil, err
	}

	validRows := roaring.New()
	match, err := documentMatchIterator.Next()
	for err == nil && match != nil {
		validRows.Add(uint32(match.Number))
		match, err = documentMatchIterator.Next()
	}
	if err != nil {
		return nil, err
	}
	return validRows, nil
}

func (q *QueryEngine) collectLogChunkRecords(ctx context.Context, candidate candidateChunk, compiled compiledLogQuery, textFilters []string, from, to time.Time, observer *queryPathObserver) ([]LogRecord, error) {
	validRows, err := q.logValidRows(ctx, candidate.chunkID, textFilters)
	if err != nil {
		return nil, err
	}
	if validRows != nil && validRows.IsEmpty() {
		return nil, nil
	}

	parquetPath := filepath.Join("chunks", string(SignalLogs), candidate.chunkID, "data.parquet")
	records := make([]LogRecord, 0)
	if err := q.scanLogParquet(ctx, parquetPath, validRows, compiled, from, to, observer, func(record LogRecord) error {
		records = append(records, record)
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].Timestamp.After(records[j].Timestamp)
	})
	return records, nil
}

func (q *QueryEngine) emitMergedLogChunks(ctx context.Context, candidates []candidateChunk, compiled compiledLogQuery, textFilters []string, from, to time.Time, observer *queryPathObserver, yield func(LogRecord) error) (int, error) {
	active := make(logChunkCursorHeap, 0)
	heap.Init(&active)
	nextCandidate := 0
	returned := 0

	for {
		for nextCandidate < len(candidates) {
			if active.Len() > 0 && candidates[nextCandidate].cutoffMax.Before(active[0].current().Timestamp) {
				break
			}

			records, err := q.collectLogChunkRecords(ctx, candidates[nextCandidate], compiled, textFilters, from, to, observer)
			nextCandidate++
			if err != nil {
				if errors.Is(err, ErrGhostChunk) {
					continue
				}
				return 0, err
			}
			if len(records) == 0 {
				continue
			}
			heap.Push(&active, &logChunkCursor{records: records})
		}

		if active.Len() == 0 {
			return returned, nil
		}

		cursor := heap.Pop(&active).(*logChunkCursor)
		if err := yield(cursor.current()); err != nil {
			return 0, err
		}
		returned++
		cursor.index++
		if cursor.index < len(cursor.records) {
			heap.Push(&active, cursor)
		}
	}
}

func (q *QueryEngine) scanLogParquet(ctx context.Context, path string, validRows *roaring.Bitmap, compiled compiledLogQuery, from, to time.Time, observer *queryPathObserver, yield func(LogRecord) error) error {
	rdr, rc, err := q.openSignalParquet(ctx, path, observer)
	if err != nil {
		return err
	}
	defer rc.Close()
	defer rdr.Close()

	for rgIdx := 0; rgIdx < rdr.NumRowGroups(); rgIdx++ {
		rg := rdr.RowGroup(rgIdx)
		n := int(rg.NumRows())
		if n == 0 {
			continue
		}

		// col 0: timestamp (int64 UnixNano)
		tsCol, err := rg.Column(0)
		if err != nil {
			return err
		}
		tsBuf := make([]int64, n)
		tsCol.(*file.Int64ColumnChunkReader).ReadBatch(int64(n), tsBuf, nil, nil) //nolint:errcheck

		// col 1: level (ByteArray)
		lvlCol, err := rg.Column(1)
		if err != nil {
			return err
		}
		lvlBuf := make([]parquet.ByteArray, n)
		lvlCol.(*file.ByteArrayColumnChunkReader).ReadBatch(int64(n), lvlBuf, nil, nil) //nolint:errcheck

		// col 2: service (ByteArray)
		svcCol, err := rg.Column(2)
		if err != nil {
			return err
		}
		svcBuf := make([]parquet.ByteArray, n)
		svcCol.(*file.ByteArrayColumnChunkReader).ReadBatch(int64(n), svcBuf, nil, nil) //nolint:errcheck

		// col 3: trace_id (ByteArray)
		traceCol, err := rg.Column(3)
		if err != nil {
			return err
		}
		traceBuf := make([]parquet.ByteArray, n)
		traceCol.(*file.ByteArrayColumnChunkReader).ReadBatch(int64(n), traceBuf, nil, nil) //nolint:errcheck

		// col 4: raw_message (ByteArray)
		msgCol, err := rg.Column(4)
		if err != nil {
			return err
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
			record := LogRecord{
				Timestamp:  ts,
				Level:      string(lvlBuf[i]),
				Service:    string(svcBuf[i]),
				TraceID:    string(traceBuf[i]),
				RawMessage: string(msgBuf[i]),
			}
			if !compiled.matchesRecord(record) {
				continue
			}
			if err := yield(record); err != nil {
				return err
			}
		}
	}
	return nil
}

func compileLogQuery(query logquery.Query) (compiledLogQuery, error) {
	compiled := compiledLogQuery{
		query:       query,
		matchers:    make([]compiledLogMatcher, 0, len(query.Matchers)),
		lineFilters: make([]compiledLineFilter, 0, len(query.LineFilters)),
	}
	for _, matcher := range query.Matchers {
		compiledMatcher := compiledLogMatcher{matcher: matcher}
		if matcher.Op == "=~" || matcher.Op == "!~" {
			re, err := regexp.Compile(matcher.Value)
			if err != nil {
				return compiledLogQuery{}, fmt.Errorf("invalid matcher regexp for %s: %w", matcher.Name, err)
			}
			compiledMatcher.regex = re
		}
		compiled.matchers = append(compiled.matchers, compiledMatcher)
	}
	for _, filter := range query.LineFilters {
		compiledFilter := compiledLineFilter{filter: filter}
		if filter.Op == "|~" || filter.Op == "!~" {
			re, err := regexp.Compile(filter.Value)
			if err != nil {
				return compiledLogQuery{}, fmt.Errorf("invalid line filter regexp: %w", err)
			}
			compiledFilter.regex = re
		}
		compiled.lineFilters = append(compiled.lineFilters, compiledFilter)
	}
	return compiled, nil
}

func (q compiledLogQuery) ServiceEquals() string {
	return q.query.ServiceEquals()
}

func (q compiledLogQuery) PositiveLineContainsFilters() []string {
	return q.query.PositiveLineContainsFilters()
}

func (q compiledLogQuery) matchesRecord(record LogRecord) bool {
	for _, matcher := range q.matchers {
		value, found := logRecordField(record, matcher.matcher.Name)
		if !matchesLogMatcher(value, found, matcher) {
			return false
		}
	}
	for _, filter := range q.lineFilters {
		if !matchesLineFilter(record.RawMessage, filter) {
			return false
		}
	}
	return true
}

func logRecordField(record LogRecord, name string) (string, bool) {
	switch name {
	case "service":
		return record.Service, true
	case "level", "severity_text":
		return record.Level, true
	case "trace_id":
		return record.TraceID, record.TraceID != ""
	case "body", "raw_message":
		return record.RawMessage, true
	default:
		return "", false
	}
}

func matchesLogMatcher(value string, found bool, matcher compiledLogMatcher) bool {
	switch matcher.matcher.Op {
	case "=":
		return found && value == matcher.matcher.Value
	case "!=":
		return !found || value != matcher.matcher.Value
	case "=~":
		return found && matcher.regex != nil && matcher.regex.MatchString(value)
	case "!~":
		return !found || matcher.regex == nil || !matcher.regex.MatchString(value)
	default:
		return false
	}
}

func matchesLineFilter(message string, filter compiledLineFilter) bool {
	switch filter.filter.Op {
	case "|=":
		return strings.Contains(message, filter.filter.Value)
	case "!=":
		return !strings.Contains(message, filter.filter.Value)
	case "|~":
		return filter.regex != nil && filter.regex.MatchString(message)
	case "!~":
		return filter.regex == nil || !filter.regex.MatchString(message)
	default:
		return false
	}
}

func buildBlugeLineFilterQuery(filters []string) bluge.Query {
	if len(filters) == 1 {
		return bluge.NewMatchQuery(filters[0]).SetField("raw_message")
	}
	query := bluge.NewBooleanQuery()
	for _, filter := range filters {
		query.AddMust(bluge.NewMatchQuery(filter).SetField("raw_message"))
	}
	return query
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
	return collectQueryResults(func(yield func(MetricRecord) error) error {
		return q.StreamMetrics(ctx, query, from, to, yield)
	})
}

// StreamMetrics yields metric data points matching the given query in the time range.
func (q *QueryEngine) StreamMetrics(ctx context.Context, query MetricQuery, from, to time.Time, yield func(MetricRecord) error) error {
	query, discoveryMode := stripMetricDiscoveryMatchers(query)
	if discoveryMode == metricDiscoveryModeNames {
		results, err := q.discoverMetricNames(ctx, query, from, to)
		if err != nil {
			return err
		}
		return emitQueryResults(results, yield)
	}

	observer := beginQueryPathObservation(SignalMetrics)
	defer observer.finish()

	listStart := time.Now()
	chunkIDs, err := q.store.ListChunks(ctx, "chunks/metrics/")
	observer.observeStage("chunk_listing", listStart)
	if err != nil {
		return fmt.Errorf("failed to list metric chunks: %w", err)
	}
	observer.addChunksListed(len(chunkIDs))
	compiledMatchers, err := compileMetricMatchers(query.LabelMatchers)
	if err != nil {
		return err
	}
	candidates, err := q.metricCandidates(ctx, chunkIDs, query, compiledMatchers, from, to, observer)
	if err != nil {
		return err
	}

	returned := 0
	for _, candidate := range candidates {
		if err := q.scanMetricParquet(ctx, filepath.Join("chunks", string(SignalMetrics), candidate.chunkID, "data.parquet"), query, compiledMatchers, from, to, observer, func(record MetricRecord) error {
			if err := yield(record); err != nil {
				return err
			}
			returned++
			return nil
		}); err != nil {
			if errors.Is(err, ErrGhostChunk) {
				continue
			}
			return err
		}
	}
	observer.addReturnedRecords(returned)
	return nil
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

func (q *QueryEngine) scanMetricParquet(ctx context.Context, path string, query MetricQuery, matchers []compiledMetricMatcher, from, to time.Time, observer *queryPathObserver, yield func(MetricRecord) error) error {
	rdr, rc, err := q.openSignalParquet(ctx, path, observer)
	if err != nil {
		return err
	}
	defer rc.Close()
	defer rdr.Close()

	for rgIdx := 0; rgIdx < rdr.NumRowGroups(); rgIdx++ {
		rg := rdr.RowGroup(rgIdx)
		n := int(rg.NumRows())
		if n == 0 {
			continue
		}

		// col 0: timestamp (int64 UnixNano)
		tsCol, err := rg.Column(0)
		if err != nil {
			return err
		}
		tsBuf := make([]int64, n)
		tsCol.(*file.Int64ColumnChunkReader).ReadBatch(int64(n), tsBuf, nil, nil) //nolint:errcheck

		// col 1: service (ByteArray)
		svcCol, err := rg.Column(1)
		if err != nil {
			return err
		}
		svcBuf := make([]parquet.ByteArray, n)
		svcCol.(*file.ByteArrayColumnChunkReader).ReadBatch(int64(n), svcBuf, nil, nil) //nolint:errcheck

		// col 2: metric_name (ByteArray)
		nameCol, err := rg.Column(2)
		if err != nil {
			return err
		}
		nameBuf := make([]parquet.ByteArray, n)
		nameCol.(*file.ByteArrayColumnChunkReader).ReadBatch(int64(n), nameBuf, nil, nil) //nolint:errcheck

		// col 3: value (float64)
		valCol, err := rg.Column(3)
		if err != nil {
			return err
		}
		valBuf := make([]float64, n)
		valCol.(*file.Float64ColumnChunkReader).ReadBatch(int64(n), valBuf, nil, nil) //nolint:errcheck

		// col 4: labels_json (ByteArray)
		lblCol, err := rg.Column(4)
		if err != nil {
			return err
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
			if err := yield(MetricRecord{
				Timestamp:  ts,
				Service:    svc,
				MetricName: name,
				Value:      valBuf[i],
				Labels:     labels,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// QueryTraces returns trace spans matching the given traceID and/or service in the time range.
func (q *QueryEngine) QueryTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int) ([]SpanRecord, error) {
	return collectQueryResults(func(yield func(SpanRecord) error) error {
		return q.StreamTraces(ctx, traceID, service, from, to, limit, yield)
	})
}

// StreamTraces yields trace spans matching the given traceID and/or service in the time range.
func (q *QueryEngine) StreamTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int, yield func(SpanRecord) error) error {
	observer := beginQueryPathObservation(SignalTraces)
	defer observer.finish()

	listStart := time.Now()
	chunkIDs, err := q.store.ListChunks(ctx, "chunks/traces/")
	observer.observeStage("chunk_listing", listStart)
	if err != nil {
		return fmt.Errorf("failed to list trace chunks: %w", err)
	}
	observer.addChunksListed(len(chunkIDs))
	candidates, err := q.traceCandidates(ctx, chunkIDs, traceID, service, from, to, observer)
	if err != nil {
		return err
	}

	if limit > 0 {
		top := make(spanRecordMinHeap, 0, limit)
		heap.Init(&top)
		for _, candidate := range candidates {
			if top.Len() >= limit && candidate.cutoffMax.Before(top[0].Timestamp) {
				break
			}

			if err := q.scanTraceParquet(ctx, filepath.Join("chunks", string(SignalTraces), candidate.chunkID, "data.parquet"), traceID, service, from, to, observer, func(record SpanRecord) error {
				if top.Len() < limit {
					heap.Push(&top, record)
					return nil
				}
				if record.Timestamp.After(top[0].Timestamp) {
					top[0] = record
					heap.Fix(&top, 0)
				}
				return nil
			}); err != nil {
				if errors.Is(err, ErrGhostChunk) {
					continue
				}
				return err
			}
		}

		results := append([]SpanRecord(nil), top...)
		sort.Slice(results, func(i, j int) bool {
			return results[i].Timestamp.After(results[j].Timestamp)
		})
		observer.addReturnedRecords(len(results))
		return emitQueryResults(results, yield)
	}

	returned, err := q.emitMergedTraceChunks(ctx, candidates, traceID, service, from, to, observer, yield)
	if err != nil {
		return err
	}
	observer.addReturnedRecords(returned)
	return nil
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

func (q *QueryEngine) collectTraceChunkRecords(ctx context.Context, candidate candidateChunk, traceID, service string, from, to time.Time, observer *queryPathObserver) ([]SpanRecord, error) {
	records := make([]SpanRecord, 0)
	if err := q.scanTraceParquet(ctx, filepath.Join("chunks", string(SignalTraces), candidate.chunkID, "data.parquet"), traceID, service, from, to, observer, func(record SpanRecord) error {
		records = append(records, record)
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].Timestamp.After(records[j].Timestamp)
	})
	return records, nil
}

func (q *QueryEngine) emitMergedTraceChunks(ctx context.Context, candidates []candidateChunk, traceID, service string, from, to time.Time, observer *queryPathObserver, yield func(SpanRecord) error) (int, error) {
	active := make(spanChunkCursorHeap, 0)
	heap.Init(&active)
	nextCandidate := 0
	returned := 0

	for {
		for nextCandidate < len(candidates) {
			if active.Len() > 0 && candidates[nextCandidate].cutoffMax.Before(active[0].current().Timestamp) {
				break
			}

			records, err := q.collectTraceChunkRecords(ctx, candidates[nextCandidate], traceID, service, from, to, observer)
			nextCandidate++
			if err != nil {
				if errors.Is(err, ErrGhostChunk) {
					continue
				}
				return 0, err
			}
			if len(records) == 0 {
				continue
			}
			heap.Push(&active, &spanChunkCursor{records: records})
		}

		if active.Len() == 0 {
			return returned, nil
		}

		cursor := heap.Pop(&active).(*spanChunkCursor)
		if err := yield(cursor.current()); err != nil {
			return 0, err
		}
		returned++
		cursor.index++
		if cursor.index < len(cursor.records) {
			heap.Push(&active, cursor)
		}
	}
}

func (q *QueryEngine) scanTraceParquet(ctx context.Context, path, traceID, service string, from, to time.Time, observer *queryPathObserver, yield func(SpanRecord) error) error {
	rdr, rc, err := q.openSignalParquet(ctx, path, observer)
	if err != nil {
		return err
	}
	defer rc.Close()
	defer rdr.Close()

	for rgIdx := 0; rgIdx < rdr.NumRowGroups(); rgIdx++ {
		rg := rdr.RowGroup(rgIdx)
		n := int(rg.NumRows())
		if n == 0 {
			continue
		}

		// col 0: timestamp (int64 UnixNano)
		tsCol, err := rg.Column(0)
		if err != nil {
			return err
		}
		tsBuf := make([]int64, n)
		tsCol.(*file.Int64ColumnChunkReader).ReadBatch(int64(n), tsBuf, nil, nil) //nolint:errcheck

		// col 1: end_time (int64 UnixNano)
		etCol, err := rg.Column(1)
		if err != nil {
			return err
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
			return err
		}
		spanIDs, err := readBytes(3)
		if err != nil {
			return err
		}
		parentSpanIDs, err := readBytes(4)
		if err != nil {
			return err
		}
		services, err := readBytes(5)
		if err != nil {
			return err
		}
		names, err := readBytes(6)
		if err != nil {
			return err
		}
		statusCodes, err := readBytes(7)
		if err != nil {
			return err
		}
		attrJSONs, err := readBytes(8)
		if err != nil {
			return err
		}

		for i := 0; i < n; i++ {
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
			if err := yield(SpanRecord{
				Timestamp:    ts,
				EndTime:      time.Unix(0, etBuf[i]),
				TraceID:      tid,
				SpanID:       string(spanIDs[i]),
				ParentSpanID: string(parentSpanIDs[i]),
				Service:      svc,
				Name:         string(names[i]),
				StatusCode:   string(statusCodes[i]),
				Attributes:   attrs,
			}); err != nil {
				return err
			}
		}
	}
	return nil
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
