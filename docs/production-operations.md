# Production Operations Playbook

## Purpose

This guide focuses on the production development and rollout cycle for a MonoFS-backed workspace:

- edit source and Guardian intent from the existing mounted workspace
- publish changes deliberately
- validate behavior with Doctor and monitoring
- respond to rollout failures
- roll back safely

It also includes supporting notes for cluster access and temporary port-forward workflows when needed.


## Search Index Freshness

MonoFS re-indexes search content as the underlying data changes. The following
table summarizes when a refreshed index is triggered and how quickly it should
converge:

| Trigger              | When                                  | Freshness                          |
|----------------------|---------------------------------------|------------------------------------|
| `ingest`             | On (re-)ingestion of a repository     | After ingest completes             |
| `publish`            | After a workspace publish (per PUBLISHED repo) | After publish job terminal event |
| `source_push`        | After a source push (per PUBLISHED repo) | After push job terminal event    |
| `refresh`            | After a refresh re-ingests a changed repo | Via the re-ingest `ingest` trigger |
| `guardian_upsert`    | After a guardian partition upsert batch | Debounced 5s per partition        |

Symbol search (`sym:` queries, `monofs-session search --symbol`) additionally
requires a universal-ctags binary with `+interactive` support to be available
to the search service. Without it, full-text search still works but `sym:`
queries return nothing.

Search indexing is best-effort: re-index triggers are asynchronous and failures
are logged, never allowed to fail the triggering ingest/publish/push operation.
Indexes converge on the next successful trigger (or the scheduled rebuild) when
a transient failure occurs.

## Auto-Refresh

MonoFS can automatically re-ingest repositories whose upstreams have advanced
so mounted sessions see fresh content without a manual pull.

Enable it on the router:

- `--auto-refresh` — enable the polling worker (default off)
- `--auto-refresh-interval` — probe interval (default 5m, min 30s)
- `--auto-refresh-concurrency` — max concurrent probes (default 4)

The worker probes each ingested repository's upstream head against the commit
recorded at ingest. `ADVANCED` re-ingests via the normal ingest path (bumping
the native namespace generation so sessions observe the change and re-indexing
search). `UNCHANGED` does nothing. `DIVERGED` is recorded as a conflict and
skipped; failed probes back off exponentially (capped at 30m) so dead remotes
are not hammered.

Webhook pushes also trigger immediate re-ingestion of matching repositories
(deduplicated within 60s), independent of the poll interval.
