# DuckDB query sidecar

`monofs-logquery` is the DuckDB query sidecar for a MonoFS storage node. It
reads the same logengine data directory as `monofs-server` and serves the
telemetry query RPCs (`QueryLogs`, `StreamQueryLogs`, metrics, traces and
metric series). The router and UI keep using `monofs-server`; the server
proxies read queries to the sidecar and keeps ingest and compaction local.

## Storage layout

New chunks are written as:

```text
chunks/<signal>/partition=<partition>/date=YYYY-MM-DD/hour=HH/<chunkID>/segment.parquet
```

`partition` is derived from `guardian.partition` (falling back to
`guardian_partition` and `partition`). Chunks containing more than one
partition use `partition=_mixed`; the per-row `partition_name` Parquet column
remains authoritative. This release intentionally does not support pre-rename
`data.parquet` segment filenames or the older layout.

Every Parquet row group is sorted by `(partition, service/metric, timestamp)`.
Logs and traces include Split Block Bloom filters for `trace_id` and `span_id`;
timestamps have a page index.

## Build

The sidecar requires CGo and the `duckdb` build tag:

```bash
make build-logquery
# equivalent:
CGO_ENABLED=1 go build -tags duckdb -o bin/monofs-logquery ./cmd/monofs-logquery
```

The Docker image is `Dockerfile.logquery` and is Debian/glibc based because
DuckDB's prebuilt bindings do not support Alpine/musl.

## Run

```bash
monofs-logquery \
  --addr=:9101 \
  --logengine-dir=/var/lib/monofs/doctor \
  --duckdb-memory-limit=2GB \
  --duckdb-threads=4 \
  --duckdb-temp-dir=/var/cache/monofs/duckdb
```

Then start the storage server with:

```bash
monofs-server ... --logquery-sidecar=127.0.0.1:9101 --logquery-sidecar-fallback=true
```

The sidecar exposes gRPC health. Readiness is updated by a periodic
`SELECT 1` check. `--self-test` runs the same validation once and exits, which
is suitable for a container `HEALTHCHECK` or a post-deploy job.

## Query behavior

- Only manifest-selected, local Parquet files are passed to
  `read_parquet()`; no user-controlled globs or paths are accepted.
- Projections and time/service/label predicates are pushed into the Parquet
  scan.
- `memory_limit`, `threads`, `temp_directory`, `max_temp_directory_size` and
  `preserve_insertion_order=false` are set once at startup.
- Queries are bounded by `MaxOpenConns` and `MaxConcurrentQueries`.
- The native scanner remains the fallback for transport or resource failures.
  SQL/schema errors are returned instead of silently falling back.
- Restricted partition scopes are applied both by manifest pruning and by a
  row-level predicate.

## Scatter-gather aggregation

`AggregateTelemetry` is a unary RPC. The router sends the same request to every
healthy storage node; each node returns partial groups from its local
logengine/DuckDB scan. The router merges `count`, `sum`, `min`, and `max`
values by group key and applies the final limit. Grouping supports top-level
fields (`service`, `partition`, `level`, `metric_name`, `status_code`,
`trace_id`, `span_id`, `timestamp`, `end_time`, `value`) and label paths such
as `labels.env` or `attributes.http.method`.

Requests are partition-scoped exactly like raw log/trace queries. Nodes do not
apply the final limit, so groups cannot be lost before the coordinator merge.

## Compaction

`logengine` compacts raw chunks into larger outputs targeting 256 MB with
multiple row groups. The background worker looks back up to 24 hours; older
backfill is available through `monofs-logengine-compact`.

## Tests

```bash
# native writer/compactor/layout tests
go test ./internal/storage/logengine/...

# DuckDB parity tests
CGO_ENABLED=1 go test -tags duckdb ./internal/storage/logengine/...
```
