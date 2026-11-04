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
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

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

	// maxMetricSeries bounds the number of distinct series StreamMetricSeries
	// may merge. The stream is used by discovery and presence aggregations, so
	// an unbounded high-cardinality store must fail with a typed limit error
	// instead of growing a map until the process is OOM-killed.
	maxMetricSeries int
	// maxQueryChunks bounds chunk candidates and maxQueryRecords bounds rows
	// yielded by a streaming query.
	maxQueryChunks  int
	maxQueryRecords int64
	discoveryCache  *metricDiscoveryCache
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
	metricDiscoveryModeLabels  = "label_names"
	metricDiscoveryModeValues  = "label_values:"

	// maxDiscoveredLabelValues bounds how many distinct values a single label
	// discovery call returns. High-cardinality labels (trace_id, task ids) would
	// otherwise make the value union unbounded.
	maxDiscoveredLabelValues = 10000

	// defaultMaxMetricSeries bounds the cross-chunk series merge performed by
	// StreamMetricSeries. The limit is intentionally generous enough for
	// normal tenants while still turning pathological cardinality into a
	// ResourceExhausted error rather than an OOM kill.
	defaultMaxMetricSeries = 200_000

	// maxDiscoveredMetricNames bounds exact, partition-filtered metric-name
	// discovery. There are far fewer metric names than series, so a limit this
	// high only trips for a genuinely misconfigured store.
	maxDiscoveredMetricNames = 20_000

	// defaultMaxQueryChunks and defaultMaxQueryRecords bound an unbounded
	// telemetry query. Compaction uses the scan helpers directly and is not
	// subject to these limits.
	defaultMaxQueryChunks  = 20_000
	defaultMaxQueryRecords = 1_000_000
)

// ErrQueryTooBroad is returned when a query exceeds a configured chunk or
// record budget.
var ErrQueryTooBroad = errors.New("telemetry query exceeds configured resource budget")

// Exported discovery protocol values for alternative telemetry backends.
// Doctor sends these as a synthetic label matcher; backends that do not share
// logengine's internal constants must use the same strings.
const (
	MetricDiscoveryMatcherName = metricDiscoveryMatcherName
	MetricDiscoveryModeNames   = metricDiscoveryModeNames
	MetricDiscoveryModeLabels  = metricDiscoveryModeLabels
	MetricDiscoveryModeValues  = metricDiscoveryModeValues
)

// StripMetricDiscoveryMatchers removes the discovery sentinel from a metric
// query and returns the requested discovery mode.
func StripMetricDiscoveryMatchers(query MetricQuery) (MetricQuery, string) {
	return stripMetricDiscoveryMatchers(query)
}

// ErrMetricSeriesLimit is returned when a metric-series scan exceeds the
// configured bounded cardinality budget.
var ErrMetricSeriesLimit = errors.New("metric series limit reached")

// NewQueryEngine creates a new QueryEngine.
func NewQueryEngine(store *CachedStore) *QueryEngine {
	return &QueryEngine{
		store:           store,
		maxMetricSeries: defaultMaxMetricSeries,
		maxQueryChunks:  defaultMaxQueryChunks,
		maxQueryRecords: defaultMaxQueryRecords,
		discoveryCache:  newMetricDiscoveryCache(),
	}
}

func (q *QueryEngine) chunkLimit() int {
	if q == nil || q.maxQueryChunks <= 0 {
		return defaultMaxQueryChunks
	}
	return q.maxQueryChunks
}

func (q *QueryEngine) recordLimit() int64 {
	if q == nil || q.maxQueryRecords <= 0 {
		return defaultMaxQueryRecords
	}
	return q.maxQueryRecords
}

func (q *QueryEngine) checkChunkBudget(count int) error {
	if count > q.chunkLimit() {
		return fmt.Errorf("%w: %d chunks exceeds the limit of %d", ErrQueryTooBroad, count, q.chunkLimit())
	}
	return nil
}

func (q *QueryEngine) metricSeriesLimit() int {
	if q == nil || q.maxMetricSeries <= 0 {
		return defaultMaxMetricSeries
	}
	return q.maxMetricSeries
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

	scope, _ := QueryScopeFromContext(ctx)
	if scope.Restricted {
		next := yield
		yield = func(record LogRecord) error {
			if !scope.allowsLog(record) {
				return nil
			}
			return next(record)
		}
	}
	recordCount := int64(0)
	recordLimit := q.recordLimit()
	if limit > 0 && int64(limit) > recordLimit {
		return fmt.Errorf("%w: limit %d exceeds the record budget of %d", ErrQueryTooBroad, limit, recordLimit)
	}
	nextYield := yield
	yield = func(record LogRecord) error {
		recordCount++
		if recordCount > recordLimit {
			return fmt.Errorf("%w: more than %d log records", ErrQueryTooBroad, recordLimit)
		}
		return nextYield(record)
	}

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
	chunkIDs, err := q.store.ListChunkPaths(ctx, SignalLogs, from, to)
	observer.observeStage("chunk_listing", listStart)
	if err != nil {
		return fmt.Errorf("failed to list chunks: %w", err)
	}
	observer.addChunksListed(len(chunkIDs))

	candidates, err := q.logCandidates(ctx, chunkIDs, serviceFilter, compiled, from, to, observer)
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

			parquetPath := filepath.Join("chunks", string(SignalLogs), candidate.chunkID, chunkDataFileName)
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
	for _, filter := range textFilters {
		if !lineFilterIndexable(filter) {
			// The Bluge query analyzer emits no terms for punctuation-only or
			// empty filters, which would incorrectly produce an empty result.
			// Fall back to scanning and let the exact line filter decide.
			return nil, nil
		}
	}

	indexPath := filepath.Join("chunks", string(SignalLogs), chunkID, "text.index.tar.gz")
	localIndexPath, err := q.store.GetLocalIndexPath(ctx, indexPath)
	if err != nil {
		if errors.Is(err, ErrGhostChunk) {
			// The data is still queryable; just fall back to scanning the
			// Parquet rows and applying the exact line filters.
			return nil, nil
		}
		return nil, err
	}

	config := bluge.DefaultConfig(localIndexPath)
	reader, err := bluge.OpenReader(config)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	blugeQuery := buildBlugeLineFilterQuery(textFilters)
	searchReq := bluge.NewAllMatches(blugeQuery)
	documentMatchIterator, err := reader.Search(ctx, searchReq)
	if err != nil {
		return nil, err
	}

	validRows := roaring.New()
	sawRowID := false
	legacyIndex := false
	match, err := documentMatchIterator.Next()
	for err == nil && match != nil {
		foundRowID := false
		visitErr := reader.VisitStoredFields(match.Number, func(field string, value []byte) bool {
			if field != logRowIDField {
				return true
			}
			rowID, parseErr := strconv.ParseUint(string(value), 10, 32)
			if parseErr != nil {
				return true
			}
			validRows.Add(uint32(rowID))
			foundRowID = true
			sawRowID = true
			return false
		})
		if visitErr != nil {
			return nil, visitErr
		}
		if !foundRowID {
			// Chunks written before the row-id field was added can still be
			// searched, but their document numbers cannot be used as Parquet
			// row indexes. Fall back to a full scan for those chunks.
			legacyIndex = true
		}
		match, err = documentMatchIterator.Next()
	}
	if err != nil {
		return nil, err
	}
	if legacyIndex {
		return nil, nil
	}
	if !sawRowID {
		// No matches at all. The index query is a superset of the exact line
		// filters, so an empty result is authoritative even for legacy chunks.
		return validRows, nil
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

	parquetPath := filepath.Join("chunks", string(SignalLogs), candidate.chunkID, chunkDataFileName)
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

	rowBase := 0
	for rgIdx := 0; rgIdx < rdr.NumRowGroups(); rgIdx++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		rg := rdr.RowGroup(rgIdx)
		n := int(rg.NumRows())
		rowStart := rowBase
		rowBase += n
		if n == 0 {
			continue
		}

		// Phase 1: Read filter column col 0 (timestamp) and intersect with validRows.
		// Only decode payload columns if at least one row in the row group passes.
		tsBuf, err := readInt64Column(rg, 0, n)
		if err != nil {
			return err
		}

		candidateIdxs := make([]int, 0, 64)
		for i := 0; i < n; i++ {
			if validRows != nil && !validRows.ContainsInt(rowStart+i) {
				continue
			}
			ts := time.Unix(0, tsBuf[i])
			if !from.IsZero() && ts.Before(from) {
				continue
			}
			if !to.IsZero() && ts.After(to) {
				continue
			}
			candidateIdxs = append(candidateIdxs, i)
		}
		if len(candidateIdxs) == 0 {
			continue
		}

		// Phase 2: Materialize payload columns only for row groups with matching rows.
		// col 1: level (ByteArray)
		lvlBuf, err := readByteArrayColumn(rg, 1, n)
		if err != nil {
			return err
		}

		// col 2: service (ByteArray)
		svcBuf, err := readByteArrayColumn(rg, 2, n)
		if err != nil {
			return err
		}

		// col 3: trace_id (ByteArray)
		traceBuf, err := readByteArrayColumn(rg, 3, n)
		if err != nil {
			return err
		}

		// col 4: raw_message (ByteArray)
		msgBuf, err := readByteArrayColumn(rg, 4, n)
		if err != nil {
			return err
		}

		// col 5: span_id, col 6: labels_json. Optional for backward
		// compatibility with chunks written before the schema extension.
		var spanBuf []parquet.ByteArray
		if rg.NumColumns() > 5 {
			spanBuf, err = readByteArrayColumn(rg, 5, n)
			if err != nil {
				return err
			}
		}
		var lblBuf []parquet.ByteArray
		if rg.NumColumns() > 6 {
			lblBuf, err = readByteArrayColumn(rg, 6, n)
			if err != nil {
				return err
			}
		}

		for _, i := range candidateIdxs {
			ts := time.Unix(0, tsBuf[i])
			record := LogRecord{
				Timestamp:  ts,
				Level:      string(lvlBuf[i]),
				Service:    string(svcBuf[i]),
				TraceID:    string(traceBuf[i]),
				RawMessage: string(msgBuf[i]),
			}
			if spanBuf != nil {
				record.SpanID = string(spanBuf[i])
			}
			if lblBuf != nil && len(lblBuf[i]) > 0 {
				json.Unmarshal(lblBuf[i], &record.Labels) //nolint:errcheck
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
			re, err := regexp.Compile(anchorLabelRegex(matcher.Value))
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
	case "service", "service_name":
		return record.Service, true
	case "level", "severity_text":
		return record.Level, true
	case "trace_id":
		return record.TraceID, record.TraceID != ""
	case "span_id":
		return record.SpanID, record.SpanID != ""
	case "body", "raw_message":
		return record.RawMessage, true
	default:
		return lookupLabelValue(record.Labels, name)
	}
}

// lookupLabelValue resolves a label through both common normalization forms.
// OTel keeps dotted attribute keys while Grafana/PromQL emit underscore names;
// callers may use either spelling.
func lookupLabelValue(values map[string]string, name string) (string, bool) {
	if value, ok := values[name]; ok {
		return value, true
	}
	for _, alt := range []string{
		strings.ReplaceAll(name, "_", "."),
		strings.ReplaceAll(name, ".", "_"),
	} {
		if alt == name {
			continue
		}
		if value, ok := values[alt]; ok {
			return value, true
		}
	}
	return "", false
}

// anchorLabelRegex implements Prometheus/Loki label-matcher semantics: label
// regexes are fully anchored, unlike line filters.
func anchorLabelRegex(pattern string) string {
	return "^(?:" + pattern + ")$"
}

func matchesLogMatcher(value string, found bool, matcher compiledLogMatcher) bool {
	if !found {
		// Prometheus/Loki label matchers evaluate against the empty string when
		// a label is absent, so `foo=""` and `foo=~".*"` match missing labels.
		value = ""
	}
	switch matcher.matcher.Op {
	case "=":
		return value == matcher.matcher.Value
	case "!=":
		return value != matcher.matcher.Value
	case "=~":
		return matcher.regex != nil && matcher.regex.MatchString(value)
	case "!~":
		return matcher.regex == nil || !matcher.regex.MatchString(value)
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

// lineFilterIndexable reports whether the standard analyzer will produce at
// least one term for a line filter. Filters containing only punctuation are
// valid substring/equality filters but cannot be looked up in the term index.
func lineFilterIndexable(value string) bool {
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

func (q *QueryEngine) prefetchManifests(ctx context.Context, signal Signal, chunkIDs []string) {
	if len(chunkIDs) <= 16 {
		return
	}
	// Maintain enough parallelism to keep remote manifest reads overlapped, but
	// bound it so a large historical chunk list cannot create a goroutine
	// stampede.
	sem := make(chan struct{}, 64)
	var wg sync.WaitGroup
	for _, chunkID := range chunkIDs {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			_, _ = q.store.ReadManifest(ctx, signal, id)
		}(chunkID)
	}
	wg.Wait()
}

func (q *QueryEngine) logCandidates(ctx context.Context, chunkIDs []string, service string, compiled compiledLogQuery, from, to time.Time, observer *queryPathObserver) ([]candidateChunk, error) {
	pruneStart := time.Now()
	defer observer.observeStage("manifest_pruning", pruneStart)

	remaining := make([]string, 0, len(chunkIDs))
	for _, chunkID := range chunkIDs {
		if minT, maxT, ok := ParseChunkTimeRangeFromID(chunkID); ok {
			if !to.IsZero() && minT.After(to) {
				observer.addChunksPruned("after_range", 1)
				continue
			}
			if !from.IsZero() && maxT.Before(from) {
				observer.addChunksPruned("before_range", 1)
				continue
			}
		}
		remaining = append(remaining, chunkID)
	}
	if err := q.checkChunkBudget(len(remaining)); err != nil {
		return nil, err
	}
	q.prefetchManifests(ctx, SignalLogs, remaining)

	candidates := make([]candidateChunk, 0, len(remaining))
	for _, chunkID := range remaining {
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
		if !manifestMatchesLogQuery(manifest, compiled) {
			observer.addChunksPruned("label_mismatch", 1)
			continue
		}
		candidates = append(candidates, candidateChunk{chunkID: chunkID, manifest: manifest, cutoffMax: manifest.MaxTime})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].manifest.MaxTime.After(candidates[j].manifest.MaxTime)
	})
	return candidates, nil
}

// manifestMatchesLogQuery prunes chunks using the bounded dictionaries written
// into new manifests. Only positive equality/regex matchers are used: a missing
// or truncated dictionary must never become a false negative.
func manifestMatchesLogQuery(manifest ChunkManifest, compiled compiledLogQuery) bool {
	for _, compiledMatcher := range compiled.matchers {
		matcher := compiledMatcher.matcher
		switch matcher.Name {
		case "service", "service_name":
			continue // handled by the exact service pushdown
		case "trace_id":
			if matcher.Op == "=" && !traceBloomMayContain(manifest.TraceBloom, matcher.Value) {
				return false
			}
			continue
		case "body", "raw_message", "span_id":
			continue
		case "level", "severity_text":
			if !manifestValuesMayMatch(manifest.Levels, compiledMatcher) {
				return false
			}
		default:
			values := manifestLabelValues(manifest.LabelValues, matcher.Name)
			if !manifestValuesMayMatch(values, compiledMatcher) {
				return false
			}
		}
	}
	return true
}

func manifestValuesMayMatch(values []string, matcher compiledLogMatcher) bool {
	if len(values) == 0 {
		return true
	}
	switch matcher.matcher.Op {
	case "=":
		if matcher.matcher.Value == "" {
			// Missing labels compare as empty; manifests do not store empty
			// values, so an empty equality matcher must keep the chunk.
			return true
		}
		for _, value := range values {
			if value == matcher.matcher.Value {
				return true
			}
		}
		return false
	case "=~":
		if matcher.regex == nil {
			return true
		}
		if matcher.regex.MatchString("") {
			return true
		}
		for _, value := range values {
			if matcher.regex.MatchString(value) {
				return true
			}
		}
		return false
	default:
		return true
	}
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
	switch {
	case discoveryMode == metricDiscoveryModeNames:
		results, err := q.discoverMetricNames(ctx, query, from, to)
		if err != nil {
			return err
		}
		return emitQueryResults(results, yield)
	case discoveryMode == metricDiscoveryModeLabels:
		results, err := q.discoverMetricLabels(ctx, query, from, to)
		if err != nil {
			return err
		}
		return emitQueryResults(results, yield)
	case strings.HasPrefix(discoveryMode, metricDiscoveryModeValues):
		label := strings.TrimPrefix(discoveryMode, metricDiscoveryModeValues)
		results, err := q.discoverMetricLabelValues(ctx, query, label, from, to)
		if err != nil {
			return err
		}
		return emitQueryResults(results, yield)
	}

	observer := beginQueryPathObservation(SignalMetrics)
	defer observer.finish()

	recordCount := int64(0)
	recordLimit := q.recordLimit()
	nextYield := yield
	yield = func(record MetricRecord) error {
		recordCount++
		if recordCount > recordLimit {
			return fmt.Errorf("%w: more than %d metric samples", ErrQueryTooBroad, recordLimit)
		}
		return nextYield(record)
	}

	listStart := time.Now()
	chunkIDs, err := q.store.ListChunkPaths(ctx, SignalMetrics, from, to)
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
		if err := q.scanMetricParquet(ctx, filepath.Join("chunks", string(SignalMetrics), candidate.chunkID, chunkDataFileName), query, compiledMatchers, from, to, observer, func(record MetricRecord) error {
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

// metricDiscoveryCandidates lists the metric chunks whose manifests match the
// query and time range. Discovery never scans sample data, so it stays cheap
// regardless of metric frequency.
func (q *QueryEngine) metricDiscoveryCandidates(ctx context.Context, query MetricQuery, from, to time.Time) ([]candidateChunk, *queryPathObserver, error) {
	observer := beginQueryPathObservation(SignalMetrics)

	listStart := time.Now()
	chunkIDs, err := q.store.ListChunkPaths(ctx, SignalMetrics, from, to)
	observer.observeStage("chunk_listing", listStart)
	if err != nil {
		observer.finish()
		return nil, nil, fmt.Errorf("failed to list metric chunks: %w", err)
	}
	observer.addChunksListed(len(chunkIDs))

	compiledMatchers, err := compileMetricMatchers(query.LabelMatchers)
	if err != nil {
		observer.finish()
		return nil, nil, err
	}
	candidates, err := q.metricCandidates(ctx, chunkIDs, query, compiledMatchers, from, to, observer)
	if err != nil {
		observer.finish()
		return nil, nil, err
	}
	return candidates, observer, nil
}

// StreamMetricSeries streams per-series metadata (identity plus first/last
// sample time) from the chunk series index. It never reads sample data, so
// presence aggregations such as `count by (__name__)` and series discovery are
// served from manifests and small per-chunk index files only. Series that
// appear in multiple chunks are merged across the range.
func (q *QueryEngine) StreamMetricSeries(ctx context.Context, query MetricQuery, from, to time.Time, yield func(MetricSeriesMeta) error) error {
	observer := beginQueryPathObservation(SignalMetrics)
	defer observer.finish()

	listStart := time.Now()
	chunkIDs, err := q.store.ListChunkPaths(ctx, SignalMetrics, from, to)
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

	indexStart := time.Now()
	merged := make(map[string]*MetricSeriesMeta)
	order := make([]string, 0)
	limit := q.metricSeriesLimit()
	for _, candidate := range candidates {
		series, err := q.store.ReadSeriesIndex(ctx, candidate.chunkID)
		if err != nil {
			if !errors.Is(err, ErrGhostChunk) {
				return err
			}
			// Chunks written before the series index existed still need to be
			// discoverable. Fall back to the sample scan for that chunk only;
			// new chunks continue to use the metadata path.
			if scanErr := q.scanMetricParquet(ctx, filepath.Join("chunks", string(SignalMetrics), candidate.chunkID, chunkDataFileName), query, compiledMatchers, from, to, observer, func(record MetricRecord) error {
				if record.MetricName == "" || record.Timestamp.IsZero() {
					return nil
				}
				meta := MetricSeriesMeta{
					MetricName: record.MetricName,
					Service:    record.Service,
					Labels:     record.Labels,
					MinTime:    record.Timestamp,
					MaxTime:    record.Timestamp,
				}
				if !metricSeriesMatches(meta, query, compiledMatchers) {
					return nil
				}
				return mergeMetricSeriesMeta(merged, &order, meta, limit)
			}); scanErr != nil && !errors.Is(scanErr, ErrGhostChunk) {
				return scanErr
			}
			continue
		}
		for _, meta := range series {
			if !metricSeriesMatches(meta, query, compiledMatchers) {
				continue
			}
			if err := mergeMetricSeriesMeta(merged, &order, meta, limit); err != nil {
				return err
			}
		}
	}
	observer.observeStage("series_index_scan", indexStart)

	sort.Strings(order)
	for _, key := range order {
		if err := yield(*merged[key]); err != nil {
			return err
		}
	}
	observer.addReturnedRecords(len(order))
	return nil
}

// metricSeriesMatches applies the query's name, service, and label matchers to
// one series index entry.
func metricSeriesMatches(meta MetricSeriesMeta, query MetricQuery, matchers []compiledMetricMatcher) bool {
	if query.MetricName != "" && meta.MetricName != query.MetricName {
		return false
	}
	if query.Service != "" && meta.Service != query.Service {
		return false
	}
	return metricLabelsMatch(meta.MetricName, meta.Service, meta.Labels, matchers)
}

func mergeMetricSeriesMeta(merged map[string]*MetricSeriesMeta, order *[]string, meta MetricSeriesMeta, limit int) error {
	key := metricSeriesMetaKey(meta)
	existing := merged[key]
	if existing == nil {
		if limit > 0 && len(merged) >= limit {
			return fmt.Errorf("%w: more than %d distinct series", ErrMetricSeriesLimit, limit)
		}
		copied := meta
		merged[key] = &copied
		*order = append(*order, key)
		return nil
	}
	if meta.MinTime.Before(existing.MinTime) {
		existing.MinTime = meta.MinTime
	}
	if meta.MaxTime.After(existing.MaxTime) {
		existing.MaxTime = meta.MaxTime
	}
	return nil
}

// metricSeriesMetaKey builds the cross-chunk identity key for a series entry.
func metricSeriesMetaKey(meta MetricSeriesMeta) string {
	names := make([]string, 0, len(meta.Labels))
	for name := range meta.Labels {
		names = append(names, name)
	}
	sort.Strings(names)
	var builder strings.Builder
	builder.WriteString(meta.MetricName)
	builder.WriteByte(0)
	builder.WriteString(meta.Service)
	for _, name := range names {
		builder.WriteByte(0)
		builder.WriteString(name)
		builder.WriteByte('=')
		builder.WriteString(meta.Labels[name])
	}
	return builder.String()
}

func (q *QueryEngine) discoverMetricNames(ctx context.Context, query MetricQuery, from, to time.Time) ([]MetricRecord, error) {
	cacheKey := metricDiscoveryCacheKey("names", "", query, from, to)
	if cached, ok := q.discoveryCache.get(cacheKey); ok {
		return cached, nil
	}

	candidates, observer, err := q.metricDiscoveryCandidates(ctx, query, from, to)
	if err != nil {
		return nil, err
	}
	defer observer.finish()

	// With a label or service filter, chunk manifests alone cannot prove that
	// a name belongs to the requested partition: a chunk may contain series
	// from several partitions. Scan the per-chunk series index instead, but
	// retain only metric-name strings (never full label sets), which keeps the
	// result bounded even for high-cardinality tenants.
	if len(query.LabelMatchers) > 0 || query.Service != "" {
		results, err := q.discoverMetricNamesFromSeries(ctx, query, from, to, candidates, observer)
		if err != nil {
			return nil, err
		}
		q.discoveryCache.put(cacheKey, results)
		return results, nil
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
	q.discoveryCache.put(cacheKey, results)
	return results, nil
}

// discoverMetricNamesFromSeries walks the per-chunk series indexes and keeps
// only the distinct metric names that match the query. Unlike StreamMetricSeries
// it never retains full per-series label maps, so partition-filtered discovery
// stays memory-bounded.
func (q *QueryEngine) discoverMetricNamesFromSeries(ctx context.Context, query MetricQuery, from, to time.Time, candidates []candidateChunk, observer *queryPathObserver) ([]MetricRecord, error) {
	compiledMatchers, err := compileMetricMatchers(query.LabelMatchers)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	add := func(name string) error {
		if name == "" {
			return nil
		}
		if _, ok := seen[name]; ok {
			return nil
		}
		if len(seen) >= maxDiscoveredMetricNames {
			return fmt.Errorf("%w: more than %d metric names", ErrMetricSeriesLimit, maxDiscoveredMetricNames)
		}
		seen[name] = struct{}{}
		return nil
	}

	for _, candidate := range candidates {
		series, err := q.store.ReadSeriesIndexUncached(ctx, candidate.chunkID)
		if err != nil {
			if !errors.Is(err, ErrGhostChunk) {
				return nil, err
			}
			// Legacy chunks have no series index. Project names from the
			// sample scan for that chunk only and keep the same bounds.
			if scanErr := q.scanMetricParquet(ctx, filepath.Join("chunks", string(SignalMetrics), candidate.chunkID, chunkDataFileName), query, compiledMatchers, from, to, observer, func(record MetricRecord) error {
				meta := MetricSeriesMeta{
					MetricName: record.MetricName,
					Service:    record.Service,
					Labels:     record.Labels,
				}
				if !metricSeriesMatches(meta, query, compiledMatchers) {
					return nil
				}
				return add(record.MetricName)
			}); scanErr != nil && !errors.Is(scanErr, ErrGhostChunk) {
				return nil, scanErr
			}
			continue
		}
		for _, meta := range series {
			if !metricSeriesMatches(meta, query, compiledMatchers) {
				continue
			}
			if err := add(meta.MetricName); err != nil {
				return nil, err
			}
		}
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)

	results := make([]MetricRecord, 0, len(names))
	for _, name := range names {
		results = append(results, MetricRecord{MetricName: name})
	}
	observer.addReturnedRecords(len(results))
	return results, nil
}

// discoverMetricLabels returns one record per distinct metric label name found
// in the matching chunk manifests. Label names are carried in the record's
// Labels map (with empty values).
func (q *QueryEngine) discoverMetricLabels(ctx context.Context, query MetricQuery, from, to time.Time) ([]MetricRecord, error) {
	cacheKey := metricDiscoveryCacheKey("labels", "", query, from, to)
	if cached, ok := q.discoveryCache.get(cacheKey); ok {
		return cached, nil
	}
	candidates, observer, err := q.metricDiscoveryCandidates(ctx, query, from, to)
	if err != nil {
		return nil, err
	}
	defer observer.finish()

	seen := make(map[string]struct{})
	names := make([]string, 0)
	for _, candidate := range candidates {
		if candidate.manifest.MetricLabelValues != nil && !candidate.manifest.MetricLabelValuesTruncated {
			for name := range candidate.manifest.MetricLabelValues {
				if name == "" {
					continue
				}
				if _, ok := seen[name]; ok {
					continue
				}
				seen[name] = struct{}{}
				names = append(names, name)
			}
			continue
		}
		series, err := q.metricSeriesForChunk(ctx, candidate, observer)
		if err != nil {
			return nil, err
		}
		for _, meta := range series {
			for name := range meta.Labels {
				if name == "" {
					continue
				}
				if _, ok := seen[name]; ok {
					continue
				}
				seen[name] = struct{}{}
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)

	results := make([]MetricRecord, 0, len(names))
	for _, name := range names {
		results = append(results, MetricRecord{Labels: map[string]string{name: ""}})
	}
	observer.addReturnedRecords(len(results))
	q.discoveryCache.put(cacheKey, results)
	return results, nil
}

// metricSeriesForChunk reads the per-chunk series index, falling back to a
// sample scan for chunks written before the series index existed.
func (q *QueryEngine) metricSeriesForChunk(ctx context.Context, candidate candidateChunk, observer *queryPathObserver) ([]MetricSeriesMeta, error) {
	series, err := q.store.ReadSeriesIndexUncached(ctx, candidate.chunkID)
	if err == nil {
		return series, nil
	}
	if !errors.Is(err, ErrGhostChunk) {
		return nil, err
	}
	path := filepath.Join("chunks", string(SignalMetrics), candidate.chunkID, chunkDataFileName)
	collected := make([]MetricSeriesMeta, 0, len(candidate.manifest.MetricNames))
	if err := q.scanMetricParquet(ctx, path, MetricQuery{}, nil, time.Time{}, time.Time{}, observer, func(record MetricRecord) error {
		collected = append(collected, MetricSeriesMeta{
			MetricName: record.MetricName,
			Service:    record.Service,
			Labels:     record.Labels,
		})
		return nil
	}); err != nil {
		return nil, err
	}
	return collected, nil
}

// discoverMetricLabelValues returns one record per distinct value of the given
// label found in the matching chunk manifests.
func (q *QueryEngine) discoverMetricLabelValues(ctx context.Context, query MetricQuery, labelName string, from, to time.Time) ([]MetricRecord, error) {
	if labelName == "" {
		return nil, fmt.Errorf("label name is required")
	}
	cacheKey := metricDiscoveryCacheKey("values", labelName, query, from, to)
	if cached, ok := q.discoveryCache.get(cacheKey); ok {
		return cached, nil
	}
	candidates, observer, err := q.metricDiscoveryCandidates(ctx, query, from, to)
	if err != nil {
		return nil, err
	}
	defer observer.finish()

	seen := make(map[string]struct{})
	values := make([]string, 0)
	for _, candidate := range candidates {
		candidateValues := []string(nil)
		if candidate.manifest.MetricLabelValues != nil && !candidate.manifest.MetricLabelValuesTruncated {
			candidateValues = manifestLabelValues(candidate.manifest.MetricLabelValues, labelName)
		} else {
			series, err := q.metricSeriesForChunk(ctx, candidate, observer)
			if err != nil {
				return nil, err
			}
			for _, meta := range series {
				if value, ok := lookupLabelValue(meta.Labels, labelName); ok && value != "" {
					candidateValues = append(candidateValues, value)
				}
			}
		}
		for _, value := range candidateValues {
			if value == "" {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			values = append(values, value)
			if len(values) >= maxDiscoveredLabelValues {
				break
			}
		}
		if len(values) >= maxDiscoveredLabelValues {
			break
		}
	}
	sort.Strings(values)

	results := make([]MetricRecord, 0, len(values))
	for _, value := range values {
		results = append(results, MetricRecord{Labels: map[string]string{labelName: value}})
	}
	observer.addReturnedRecords(len(results))
	q.discoveryCache.put(cacheKey, results)
	return results, nil
}

// metricDiscoveryCacheKey builds a stable key for discovery results. Time is
// bucketed to five minutes so dashboard refreshes hit the cache while the
// result still tracks the requested discovery window.
func metricDiscoveryCacheKey(mode, label string, query MetricQuery, from, to time.Time) string {
	var builder strings.Builder
	builder.WriteString(mode)
	builder.WriteByte(0)
	builder.WriteString(label)
	builder.WriteByte(0)
	builder.WriteString(query.MetricName)
	builder.WriteByte(0)
	builder.WriteString(query.Service)

	matchers := append([]MetricLabelMatcher(nil), query.LabelMatchers...)
	sort.Slice(matchers, func(i, j int) bool {
		if matchers[i].Name != matchers[j].Name {
			return matchers[i].Name < matchers[j].Name
		}
		if matchers[i].Value != matchers[j].Value {
			return matchers[i].Value < matchers[j].Value
		}
		return matchers[i].Type < matchers[j].Type
	})
	for _, matcher := range matchers {
		builder.WriteByte(0)
		builder.WriteString(matcher.Name)
		builder.WriteByte('=')
		builder.WriteString(matcher.Value)
		builder.WriteByte(':')
		builder.WriteString(string(matcher.Type))
	}

	builder.WriteByte(0)
	if !from.IsZero() {
		builder.WriteString(strconv.FormatInt(from.Unix()/300, 10))
	}
	builder.WriteByte('/')
	if !to.IsZero() {
		builder.WriteString(strconv.FormatInt(to.Unix()/300, 10))
	}
	return builder.String()
}

func (q *QueryEngine) metricCandidates(ctx context.Context, chunkIDs []string, query MetricQuery, compiledMatchers []compiledMetricMatcher, from, to time.Time, observer *queryPathObserver) ([]candidateChunk, error) {
	pruneStart := time.Now()
	defer observer.observeStage("manifest_pruning", pruneStart)

	remaining := make([]string, 0, len(chunkIDs))
	for _, chunkID := range chunkIDs {
		if minT, maxT, ok := ParseChunkTimeRangeFromID(chunkID); ok {
			if !to.IsZero() && minT.After(to) {
				observer.addChunksPruned("after_range", 1)
				continue
			}
			if !from.IsZero() && maxT.Before(from) {
				observer.addChunksPruned("before_range", 1)
				continue
			}
		}
		remaining = append(remaining, chunkID)
	}
	if err := q.checkChunkBudget(len(remaining)); err != nil {
		return nil, err
	}
	q.prefetchManifests(ctx, SignalMetrics, remaining)

	candidates := make([]candidateChunk, 0, len(remaining))
	for _, chunkID := range remaining {
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
		if err := ctx.Err(); err != nil {
			return err
		}
		rg := rdr.RowGroup(rgIdx)
		n := int(rg.NumRows())
		if n == 0 {
			continue
		}

		// Phase 1: Read filter columns (timestamp, service, metric_name) first.
		tsBuf, err := readInt64Column(rg, 0, n)
		if err != nil {
			return err
		}

		svcBuf, err := readByteArrayColumn(rg, 1, n)
		if err != nil {
			return err
		}

		nameBuf, err := readByteArrayColumn(rg, 2, n)
		if err != nil {
			return err
		}

		candidateIdxs := make([]int, 0, 64)
		for i := 0; i < n; i++ {
			ts := time.Unix(0, tsBuf[i])
			if !from.IsZero() && ts.Before(from) {
				continue
			}
			if !to.IsZero() && ts.After(to) {
				continue
			}
			if query.Service != "" && string(svcBuf[i]) != query.Service {
				continue
			}
			if query.MetricName != "" && string(nameBuf[i]) != query.MetricName {
				continue
			}
			candidateIdxs = append(candidateIdxs, i)
		}
		if len(candidateIdxs) == 0 {
			continue
		}

		// Phase 2: Materialize value & labels_json only when candidate rows exist.
		valBuf, err := readFloat64Column(rg, 3, n)
		if err != nil {
			return err
		}

		lblBuf, err := readByteArrayColumn(rg, 4, n)
		if err != nil {
			return err
		}

		for _, i := range candidateIdxs {
			ts := time.Unix(0, tsBuf[i])
			svc := string(svcBuf[i])
			name := string(nameBuf[i])
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

	scope, _ := QueryScopeFromContext(ctx)
	if scope.Restricted {
		next := yield
		yield = func(record SpanRecord) error {
			if !scope.allowsSpan(record) {
				return nil
			}
			return next(record)
		}
	}
	recordCount := int64(0)
	recordLimit := q.recordLimit()
	if limit > 0 && int64(limit) > recordLimit {
		return fmt.Errorf("%w: limit %d exceeds the record budget of %d", ErrQueryTooBroad, limit, recordLimit)
	}
	nextYield := yield
	yield = func(record SpanRecord) error {
		recordCount++
		if recordCount > recordLimit {
			return fmt.Errorf("%w: more than %d span records", ErrQueryTooBroad, recordLimit)
		}
		return nextYield(record)
	}

	listStart := time.Now()
	chunkIDs, err := q.store.ListChunkPaths(ctx, SignalTraces, from, to)
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

			if err := q.scanTraceParquet(ctx, filepath.Join("chunks", string(SignalTraces), candidate.chunkID, chunkDataFileName), traceID, service, from, to, observer, func(record SpanRecord) error {
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

	remaining := make([]string, 0, len(chunkIDs))
	for _, chunkID := range chunkIDs {
		if minT, maxT, ok := ParseChunkTimeRangeFromID(chunkID); ok {
			if !to.IsZero() && minT.After(to) {
				observer.addChunksPruned("after_range", 1)
				continue
			}
			if !from.IsZero() && maxT.Before(from) {
				observer.addChunksPruned("before_range", 1)
				continue
			}
		}
		remaining = append(remaining, chunkID)
	}
	if err := q.checkChunkBudget(len(remaining)); err != nil {
		return nil, err
	}
	q.prefetchManifests(ctx, SignalTraces, remaining)

	candidates := make([]candidateChunk, 0, len(remaining))
	for _, chunkID := range remaining {
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
	if err := q.scanTraceParquet(ctx, filepath.Join("chunks", string(SignalTraces), candidate.chunkID, chunkDataFileName), traceID, service, from, to, observer, func(record SpanRecord) error {
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
		if err := ctx.Err(); err != nil {
			return err
		}
		rg := rdr.RowGroup(rgIdx)
		n := int(rg.NumRows())
		if n == 0 {
			continue
		}

		// Phase 1: Read filter columns (timestamp, trace_id, service) first.
		tsBuf, err := readInt64Column(rg, 0, n)
		if err != nil {
			return err
		}

		traceIDs, err := readByteArrayColumn(rg, 2, n)
		if err != nil {
			return err
		}
		services, err := readByteArrayColumn(rg, 5, n)
		if err != nil {
			return err
		}

		candidateIdxs := make([]int, 0, 64)
		for i := 0; i < n; i++ {
			ts := time.Unix(0, tsBuf[i])
			if !from.IsZero() && ts.Before(from) {
				continue
			}
			if !to.IsZero() && ts.After(to) {
				continue
			}
			if traceID != "" && string(traceIDs[i]) != traceID {
				continue
			}
			if service != "" && string(services[i]) != service {
				continue
			}
			candidateIdxs = append(candidateIdxs, i)
		}
		if len(candidateIdxs) == 0 {
			continue
		}

		// Phase 2: Materialize payload columns only when candidate rows exist.
		etBuf, err := readInt64Column(rg, 1, n)
		if err != nil {
			return err
		}

		spanIDs, err := readByteArrayColumn(rg, 3, n)
		if err != nil {
			return err
		}
		parentSpanIDs, err := readByteArrayColumn(rg, 4, n)
		if err != nil {
			return err
		}
		names, err := readByteArrayColumn(rg, 6, n)
		if err != nil {
			return err
		}
		statusCodes, err := readByteArrayColumn(rg, 7, n)
		if err != nil {
			return err
		}
		attrJSONs, err := readByteArrayColumn(rg, 8, n)
		if err != nil {
			return err
		}

		for _, i := range candidateIdxs {
			ts := time.Unix(0, tsBuf[i])
			tid := string(traceIDs[i])
			svc := string(services[i])
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

func readInt64Column(rg *file.RowGroupReader, column, rows int) ([]int64, error) {
	chunk, err := rg.Column(column)
	if err != nil {
		return nil, err
	}
	reader, ok := chunk.(*file.Int64ColumnChunkReader)
	if !ok {
		return nil, fmt.Errorf("column %d: unexpected reader type %T (want int64)", column, chunk)
	}
	values := make([]int64, rows)
	total, valuesRead, err := reader.ReadBatch(int64(rows), values, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("read column %d: %w", column, err)
	}
	if total != int64(rows) || valuesRead != rows {
		return nil, fmt.Errorf("read column %d: short read (%d/%d values, %d/%d rows)", column, total, rows, valuesRead, rows)
	}
	return values, nil
}

func readFloat64Column(rg *file.RowGroupReader, column, rows int) ([]float64, error) {
	chunk, err := rg.Column(column)
	if err != nil {
		return nil, err
	}
	reader, ok := chunk.(*file.Float64ColumnChunkReader)
	if !ok {
		return nil, fmt.Errorf("column %d: unexpected reader type %T (want float64)", column, chunk)
	}
	values := make([]float64, rows)
	total, valuesRead, err := reader.ReadBatch(int64(rows), values, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("read column %d: %w", column, err)
	}
	if total != int64(rows) || valuesRead != rows {
		return nil, fmt.Errorf("read column %d: short read (%d/%d values, %d/%d rows)", column, total, rows, valuesRead, rows)
	}
	return values, nil
}

func readByteArrayColumn(rg *file.RowGroupReader, column, rows int) ([]parquet.ByteArray, error) {
	chunk, err := rg.Column(column)
	if err != nil {
		return nil, err
	}
	reader, ok := chunk.(*file.ByteArrayColumnChunkReader)
	if !ok {
		return nil, fmt.Errorf("column %d: unexpected reader type %T (want byte array)", column, chunk)
	}
	values := make([]parquet.ByteArray, rows)
	total, valuesRead, err := reader.ReadBatch(int64(rows), values, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("read column %d: %w", column, err)
	}
	if total != int64(rows) || valuesRead != rows {
		return nil, fmt.Errorf("read column %d: short read (%d/%d values, %d/%d rows)", column, total, rows, valuesRead, rows)
	}
	return values, nil
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

// manifestLabelValues returns the union of the dictionaries for the query
// spelling and both normalization forms. A chunk may contain series using
// dotted OTel keys and others using underscore-normalized keys, so taking only
// the first non-empty dictionary would falsely prune chunks.
func manifestLabelValues(values map[string][]string, name string) []string {
	keys := []string{name}
	for _, alt := range []string{
		strings.ReplaceAll(name, "_", "."),
		strings.ReplaceAll(name, ".", "_"),
	} {
		if alt != name {
			keys = append(keys, alt)
		}
	}
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, key := range keys {
		for _, value := range values[key] {
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	return out
}

func manifestMatchesMetricLabels(manifest ChunkManifest, matchers []compiledMetricMatcher) bool {
	if manifest.MetricLabelValuesTruncated {
		// The manifest dictionary is incomplete and cannot prove absence.
		return true
	}
	for _, matcher := range matchers {
		if matcher.matcher.Type != labels.MatchEqual {
			continue
		}
		if matcher.name == labels.MetricName || matcher.name == "service" {
			continue
		}
		if matcher.matcher.Value == "" {
			// Missing labels compare as empty and manifests omit empty values.
			continue
		}
		values := manifestLabelValues(manifest.MetricLabelValues, matcher.name)
		if len(values) == 0 {
			// Chunks written before this label was indexed cannot prove the
			// label is absent. Keep them as candidates; the record-level scan
			// still applies the matcher exactly.
			continue
		}
		if !manifestContains(values, matcher.matcher.Value) {
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
			value, _ = lookupLabelValue(values, matcher.name)
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
