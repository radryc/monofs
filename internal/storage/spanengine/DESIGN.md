# Span Engine (internal, experimental)

Status: **design scaffold**. Not wired into MonoFS, not a stable API.

The current trace read path reuses the generic log/metric chunk pipeline. That
works, but it is not shaped for the two access patterns that dominate trace
traffic:

1. **Trace lookup by ID** — fetch every span of one trace.
2. **Recent/search** — return the newest traces matching service, operation,
   status, duration, time range, and attributes.

Both should avoid a full columnar scan, avoid DuckDB's single query slot, and
stay fast under many concurrent on-call queries. Analytics (aggregations,
percentiles, joins) can stay on DuckDB/rollups.

This package is the place to iterate on that architecture. `memory.go` is a
reference implementation used to pin down semantics and tests; it is not a
production store.

## Goals

- Sub-100ms p95 trace-by-ID lookups for recent traces.
- Predictable recent/search latency with bounded rows and bytes read.
- No global query slot: reads are stateless and horizontally scalable.
- Trace-centric layout: spans of a trace are close together.
- Cheap pruning by time, service, operation, status, duration, and common
  attributes.
- High ingest throughput without blocking readers.
- Clean separation from DuckDB: point lookups and recent lists never queue
  behind analytical SQL.

## Non-goals (for this package)

- Replacing DuckDB for aggregations, joins, percentiles, and SQL console.
- Full OpenTelemetry attribute search on arbitrary keys in v1.
- Cross-tenant analytics.

## Data model

A span is the unit of ingest:

```
trace_id, span_id, parent_span_id
service, operation
start_time, end_time (duration_ns derived)
status_code
attributes (string map)
```

A trace is the unit of retrieval. A trace summary contains:

```
trace_id, root_service, root_name
start_time, duration
span_count, error_spans, status_code
```

## Storage layout

### Chunks

- Immutable chunks partitioned by **event time** (target 1–5 minutes) and a
  stable hash of `trace_id` when a single time bucket is too large.
- A trace can span multiple chunks when spans arrive late; the trace index
  keeps the list of chunks for that trace.
- Each chunk is columnar (Parquet initially) with columns ordered for the
  common filters:
  `trace_id, start_time, duration_ns, service_id, operation_id, status,
  span_id, parent_span_id, attributes_json`.
- Dictionary-encode `service_id` and `operation_id` inside the chunk.
- Sort rows by `trace_id, start_time` inside the chunk so trace assembly is a
  contiguous row range.

### Per-chunk manifest

The manifest is the query planner's only input unless the chunk is a candidate:

```
time_min, time_max
row_count
service_dictionary, operation_dictionary
status_counts
duration_histogram
trace_id_bloom
attribute_bloom / attribute dictionaries for configured keys
compaction generation
```

### Global trace index

A compact `trace_id → [(chunk_id, row_offset, row_count)]` index. For recent
traces this can be an in-memory shard; for older data it can be a sorted
on-disk table with a bloom filter. Trace lookup should touch one chunk in the
common case and a small list in the late-span case.

### Duration and status indexes

- Store duration histogram buckets in the manifest so `min_duration` /
  `max_duration` can prune whole chunks.
- Status counts prune `status=ERROR` searches.
- Within a chunk, row-group statistics on `duration_ns` and `start_time`
  provide second-level pruning.

## Write path

1. Buffer spans for the current time bucket.
2. Group/sort by `trace_id, start_time`.
3. Encode columns and write the chunk to object storage.
4. Update the chunk manifest and the global trace index atomically.
5. Periodically compact small chunks:
   - merge adjacent time buckets;
   - rebuild dictionaries, blooms, and histograms;
   - drop source chunks only after the new chunk and index are durable.

Late-arriving spans for an existing trace append to the current bucket and add
an index entry. Readers merge the listed chunks for that trace.

## Read path

### GetTrace(traceID)

1. Look up `traceID` in the global trace index (bloom first, then exact).
2. If the trace is in a recent in-memory buffer, read it there.
3. Otherwise read only the listed chunk row ranges.
4. Sort by `start_time` and return.

No time-range scan, no DuckDB, no all-node fan-out.

### Search(query)

1. Prune chunks by `[from,to]`, service/operation dictionaries, status counts,
   and duration histogram.
2. Rank remaining chunks newest-first.
3. Scan candidate row groups only; apply status/duration/attribute filters.
4. Build trace summaries from the matching spans.
5. Stop when the requested limit plus one is reached (to detect `has_more`).
6. Return a cursor that encodes the last `(start_time, trace_id)` so pagination
   is stable.

### Analytics

DuckDB or a rollup table reads the same chunk files for:
`count by service`, `rate`, `p50/p95/p99`, error ratios, and other aggregates.
The point/read path above never waits on DuckDB.

## Concurrency and caching

- Readers are stateless; a query worker needs only object-store access, a
  manifest cache, and a local chunk cache.
- Per-node native query concurrency should be bounded (e.g. 8–16) to protect
  ingest and CPU.
- Cache manifests, dictionaries, and the hot part of the global trace index.
- Per-query budgets: max chunks, max rows decoded, max bytes read, deadline.
  Return partial results with a warning rather than unbounded scanning.
- No single global query slot. Scale query workers horizontally.

## Benchmark harness

Before productionizing, benchmark against the current `logengine` trace path:

- trace-by-ID, recent, and cold traces;
- recent traces by service, operation, status, duration;
- 1h / 6h / 24h ranges;
- 1 / 10 / 100 concurrent queries;
- ingest at the same time as reads.

Measure p50/p95/p99, chunks touched, rows decoded, bytes read, CPU, and memory.
Parity tests must compare trace summaries, ordering, limits, cursors, and
attribute filters with the current implementation.

## Migration path

1. Keep this package internal and un-wired.
2. Add an adapter from `logengine.SpanRecord` to `spanengine.Span`.
3. Dual-write spans into both the existing logengine chunks and the new span
   chunks behind a feature flag.
4. Shadow-read recent/search queries and compare results with the current
   path.
5. Enable reads for a small tenant/partition, then for all on-call trace
   queries.
6. Leave analytical trace queries on DuckDB until rollups exist.

## Open questions

- Chunk boundary: pure time buckets vs. time + trace hash. Late spans and trace
  completeness need a clear rule.
- Trace index retention: how much of the global index stays in memory?
- Attribute indexing: which keys are worth an inverted index vs. a bloom?
- Tail sampling / partial traces: how do we mark a trace as incomplete without
  hiding spans that arrive late?
- Multi-tenant isolation in the trace index and chunk catalog.
- When to replace Parquet with a purpose-built span format.
