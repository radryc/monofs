<script setup lang="ts">
import { computed, ref } from 'vue'
import {
  Activity,
  Archive,
  ChartColumn,
  CircleCheck,
  CircleX,
  Clock,
  Database,
  Gauge,
  HardDrive,
  Layers,
  RefreshCw,
  Server,
  Table2,
  Trash2,
  TrendingUp,
} from 'lucide-vue-next'
import { useAutoRefresh, formatBytes, formatNumber } from '../composables/useAutoRefresh'
import PageHeader from '../components/PageHeader.vue'
import StatCard from '../components/StatCard.vue'
import DataCard from '../components/DataCard.vue'
import ProgressBar from '../components/ProgressBar.vue'
import type { LogEngineData, LogEngineNode } from '../types/api'

const logEngine = ref<LogEngineData | null>(null)

async function loadSpanEngine() {
  logEngine.value = await fetch('/api/span-engine').then(r => r.json())
}

const { loading, error } = useAutoRefresh(loadSpanEngine, 10_000)

const spanEngine = computed(() => (logEngine.value?.span_engine === 'clickhouse' ? 'clickhouse' : 'native'))
const isClickHouse = computed(() => spanEngine.value === 'clickhouse')
const clickHouse = computed(() => logEngine.value?.clickhouse ?? null)

const nodes = computed<LogEngineNode[]>(() => logEngine.value?.nodes ?? [])

// --- Native engine (local Parquet chunks + compaction + DuckDB) ---
const storedBuckets = computed(() => logEngine.value?.stored_buckets ?? 0)
const compactedBuckets = computed(() => logEngine.value?.compacted_buckets ?? 0)
const rawBuckets = computed(() => Math.max(0, storedBuckets.value - compactedBuckets.value))
const compactionPct = computed(() =>
  storedBuckets.value > 0 ? (compactedBuckets.value / storedBuckets.value) * 100 : 0
)

const signalRows = computed(() => [
  {
    signal: 'Logs',
    chunks: logEngine.value?.log_chunks ?? 0,
    buckets: logEngine.value?.log_buckets ?? 0,
    compacted: logEngine.value?.log_compacted_buckets ?? 0,
    raw: Math.max(0, (logEngine.value?.log_buckets ?? 0) - (logEngine.value?.log_compacted_buckets ?? 0)),
  },
  {
    signal: 'Metrics',
    chunks: logEngine.value?.metric_chunks ?? 0,
    buckets: logEngine.value?.metric_buckets ?? 0,
    compacted: logEngine.value?.metric_compacted_buckets ?? 0,
    raw: Math.max(0, (logEngine.value?.metric_buckets ?? 0) - (logEngine.value?.metric_compacted_buckets ?? 0)),
  },
  {
    signal: 'Traces',
    chunks: logEngine.value?.trace_chunks ?? 0,
    buckets: logEngine.value?.trace_buckets ?? 0,
    compacted: logEngine.value?.trace_compacted_buckets ?? 0,
    raw: Math.max(0, (logEngine.value?.trace_buckets ?? 0) - (logEngine.value?.trace_compacted_buckets ?? 0)),
  },
])

const totalChunks = computed(() =>
  (logEngine.value?.log_chunks ?? 0) +
  (logEngine.value?.metric_chunks ?? 0) +
  (logEngine.value?.trace_chunks ?? 0)
)

const duckdbNodes = computed(() => nodes.value.filter((node) => node.duckdb_enabled))

// --- ClickHouse engine ---
const chHealthy = computed(() => clickHouse.value?.healthy === true)
const chDiskPct = computed(() => {
  const total = clickHouse.value?.disk_total_bytes ?? 0
  if (!total) return 0
  return Math.min(100, ((clickHouse.value?.disk_used_bytes ?? 0) / total) * 100)
})
const chFreePct = computed(() => {
  const total = clickHouse.value?.disk_total_bytes ?? 0
  if (!total) return 0
  return Math.min(100, ((clickHouse.value?.disk_free_bytes ?? 0) / total) * 100)
})

const clickHouseRows = computed(() => {
  const ch = clickHouse.value
  const rows = [
    { signal: 'Spans', rows: ch?.spans_rows ?? 0, bytes: ch?.spans_bytes ?? 0 },
    { signal: 'Logs', rows: ch?.logs_rows ?? 0, bytes: ch?.logs_bytes ?? 0 },
    { signal: 'Metrics', rows: ch?.metrics_rows ?? 0, bytes: ch?.metrics_bytes ?? 0 },
  ]
  return rows.map((row) => ({
    ...row,
    share: (ch?.total_bytes ?? 0) > 0 ? (row.bytes / (ch?.total_bytes ?? 1)) * 100 : 0,
  }))
})

const retentionLabel = computed(() => {
  const seconds = clickHouse.value?.retention_seconds ?? 0
  if (!seconds) return 'no TTL'
  const days = seconds / 86_400
  return Number.isInteger(days) ? `${days}d` : `${(days).toFixed(1)}d`
})

const engineDescription = computed(() =>
  isClickHouse.value
    ? 'Logs, metrics, and traces are written to the shared ClickHouse cluster. Every MonoFS node uses the same tables, so the router reads from one node.'
    : 'Logs, metrics, and traces are stored in shard-local Parquet chunks. The background compactor merges hourly buckets, and the optional DuckDB sidecar serves ad-hoc SQL.'
)

function formatNano(value?: number): string {
  if (!value) return 'never'
  return new Date(value / 1_000_000).toLocaleString()
}

function nodeBuckets(node: LogEngineNode): number {
  return (node.log_buckets ?? 0) + (node.metric_buckets ?? 0) + (node.trace_buckets ?? 0)
}

function nodeCompactedBuckets(node: LogEngineNode): number {
  return (
    (node.log_compacted_buckets ?? 0) +
    (node.metric_compacted_buckets ?? 0) +
    (node.trace_compacted_buckets ?? 0)
  )
}
</script>

<template>
  <div>
    <PageHeader
      title="Span engine"
      :subtitle="isClickHouse ? 'Shared ClickHouse telemetry storage' : 'Native Parquet storage, compaction, and DuckDB'"
    >
      <template #actions>
        <div class="flex items-center gap-2">
          <span
            class="inline-flex items-center gap-1.5 rounded-full border px-3 py-1.5 text-xs font-semibold uppercase tracking-wide"
            :class="isClickHouse
              ? 'border-sky-500/30 bg-sky-500/10 text-sky-300'
              : 'border-violet-500/30 bg-violet-500/10 text-violet-300'"
          >
            <Activity class="h-3.5 w-3.5" />
            {{ isClickHouse ? 'ClickHouse' : 'Native' }}
          </span>
          <button
            type="button"
            class="inline-flex items-center gap-2 rounded-lg border border-slate-700/50 bg-slate-900/60 px-3 py-2 text-xs font-semibold text-slate-300 hover:border-slate-500 hover:text-slate-100"
            @click="loadSpanEngine"
          >
            <RefreshCw class="h-3.5 w-3.5" :class="{ 'animate-spin': loading }" />
            Refresh
          </button>
        </div>
      </template>
    </PageHeader>

    <div class="mb-6 rounded-2xl border border-slate-700/40 bg-slate-900/60 p-5">
      <div class="flex flex-wrap items-start gap-4">
        <div
          class="flex h-11 w-11 shrink-0 items-center justify-center rounded-xl"
          :class="isClickHouse ? 'bg-sky-500/15 text-sky-300' : 'bg-violet-500/15 text-violet-300'"
        >
          <Database v-if="isClickHouse" class="h-5 w-5" />
          <Archive v-else class="h-5 w-5" />
        </div>
        <div class="min-w-0 flex-1">
          <h2 class="text-sm font-semibold text-slate-200">
            {{ isClickHouse ? 'ClickHouse span engine' : 'Native span engine' }}
          </h2>
          <p class="mt-1 max-w-3xl text-xs text-slate-400">{{ engineDescription }}</p>
        </div>
        <div v-if="isClickHouse" class="flex items-center gap-2 text-xs">
          <span
            class="inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 font-semibold"
            :class="chHealthy
              ? 'border-emerald-500/30 bg-emerald-500/10 text-emerald-300'
              : 'border-rose-500/30 bg-rose-500/10 text-rose-300'"
          >
            <CircleCheck v-if="chHealthy" class="h-3 w-3" />
            <CircleX v-else class="h-3 w-3" />
            {{ chHealthy ? 'Healthy' : 'Unreachable' }}
          </span>
          <span v-if="clickHouse?.version" class="rounded-full border border-slate-700/50 bg-slate-800/40 px-2.5 py-1 font-mono text-slate-400">
            v{{ clickHouse.version }}
          </span>
        </div>
      </div>
    </div>

    <div
      v-if="error"
      class="mb-6 rounded-xl border border-rose-500/30 bg-rose-500/10 px-4 py-3 text-xs text-rose-300"
    >
      {{ error }}
    </div>

    <!-- ================= ClickHouse engine ================= -->
    <template v-if="isClickHouse">
      <div
        v-if="clickHouse?.error"
        class="mb-6 rounded-xl border border-rose-500/30 bg-rose-500/10 px-4 py-3 text-xs text-rose-300"
      >
        <span class="font-semibold">ClickHouse probe failed:</span> {{ clickHouse.error }}
      </div>

      <div class="mb-6 grid grid-cols-2 gap-4 sm:grid-cols-4">
        <StatCard
          :icon="chHealthy ? CircleCheck : CircleX"
          label="Health"
          :value="chHealthy ? 'Healthy' : 'Unreachable'"
          :sub="chHealthy ? `ClickHouse ${clickHouse?.version || ''}`.trim() : 'No response from ClickHouse'"
        />
        <StatCard
          :icon="HardDrive"
          label="Free space"
          :value="formatBytes(clickHouse?.disk_free_bytes ?? 0)"
          :sub="`${chFreePct.toFixed(1)}% free of ${formatBytes(clickHouse?.disk_total_bytes ?? 0)}`"
        />
        <StatCard
          :icon="Database"
          label="Used by ClickHouse"
          :value="formatBytes(clickHouse?.disk_used_bytes ?? 0)"
          :sub="`${chDiskPct.toFixed(1)}% of server disk`"
        />
        <StatCard
          :icon="Table2"
          label="Telemetry on disk"
          :value="formatBytes(clickHouse?.total_bytes ?? 0)"
          :sub="`${formatNumber(clickHouse?.total_rows ?? 0)} rows · ${formatNumber(clickHouse?.parts ?? 0)} parts`"
        />
        <StatCard
          :icon="Layers"
          label="Spans"
          :value="formatNumber(clickHouse?.spans_rows ?? 0)"
          :sub="`${formatBytes(clickHouse?.spans_bytes ?? 0)} on disk`"
        />
        <StatCard
          :icon="ChartColumn"
          label="Logs"
          :value="formatNumber(clickHouse?.logs_rows ?? 0)"
          :sub="`${formatBytes(clickHouse?.logs_bytes ?? 0)} on disk`"
        />
        <StatCard
          :icon="Gauge"
          label="Metrics"
          :value="formatNumber(clickHouse?.metrics_rows ?? 0)"
          :sub="`${formatBytes(clickHouse?.metrics_bytes ?? 0)} on disk`"
        />
        <StatCard
          :icon="Clock"
          label="Retention"
          :value="retentionLabel"
          :sub="`${clickHouse?.database || 'strata'} database`"
        />
      </div>

      <DataCard class="mb-6">
        <template #header>
          <div class="flex flex-wrap items-center justify-between gap-3">
            <div>
              <h2 class="text-sm font-semibold text-slate-200">Storage by signal</h2>
              <p class="mt-0.5 text-xs text-slate-400">
                Active ClickHouse parts for the shared telemetry tables.
              </p>
            </div>
            <div class="flex items-center gap-2 text-xs">
              <span class="inline-flex items-center gap-1 rounded-full border border-slate-700/50 bg-slate-800/40 px-2 py-1 text-slate-400">
                <Server class="h-3 w-3" />
                {{ nodes.length }} MonoFS node{{ nodes.length === 1 ? '' : 's' }}
              </span>
            </div>
          </div>
        </template>
        <div class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="border-b border-slate-700/40 text-xs uppercase tracking-wider text-slate-400">
                <th class="px-6 py-3 text-left font-medium">Signal</th>
                <th class="px-6 py-3 text-right font-medium">Rows</th>
                <th class="px-6 py-3 text-right font-medium">On disk</th>
                <th class="px-6 py-3 text-left font-medium">Share of telemetry</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-slate-700/20">
              <tr v-for="row in clickHouseRows" :key="row.signal" class="transition-colors hover:bg-slate-800/30">
                <td class="px-6 py-3 font-medium text-slate-200">{{ row.signal }}</td>
                <td class="px-6 py-3 text-right text-slate-300">{{ formatNumber(row.rows) }}</td>
                <td class="px-6 py-3 text-right text-slate-300">{{ formatBytes(row.bytes) }}</td>
                <td class="px-6 py-3">
                  <div class="flex items-center gap-3">
                    <ProgressBar :value="row.share" class="max-w-[220px]" />
                    <span class="w-12 text-right text-xs text-slate-400">{{ row.share.toFixed(1) }}%</span>
                  </div>
                </td>
              </tr>
            </tbody>
            <tfoot class="border-t border-slate-700/40 bg-slate-800/20">
              <tr class="font-semibold">
                <td class="px-6 py-3 text-slate-200">Total</td>
                <td class="px-6 py-3 text-right text-slate-200">{{ formatNumber(clickHouse?.total_rows ?? 0) }}</td>
                <td class="px-6 py-3 text-right text-slate-200">{{ formatBytes(clickHouse?.total_bytes ?? 0) }}</td>
                <td class="px-6 py-3 text-xs font-normal text-slate-400">{{ formatNumber(clickHouse?.parts ?? 0) }} active parts</td>
              </tr>
            </tfoot>
          </table>
        </div>
      </DataCard>

      <div class="grid gap-6 lg:grid-cols-2">
        <DataCard>
          <template #header>
            <div>
              <h2 class="text-sm font-semibold text-slate-200">Server disk</h2>
              <p class="mt-0.5 text-xs text-slate-400">Space reported by ClickHouse's <code>system.disks</code>.</p>
            </div>
          </template>
          <div class="space-y-5 px-6 py-5">
            <div>
              <div class="mb-2 flex items-center justify-between text-xs">
                <span class="text-slate-400">Used</span>
                <span class="text-slate-300">{{ formatBytes(clickHouse?.disk_used_bytes ?? 0) }} / {{ formatBytes(clickHouse?.disk_total_bytes ?? 0) }}</span>
              </div>
              <ProgressBar :value="chDiskPct" />
            </div>
            <div class="grid grid-cols-3 gap-3 text-center">
              <div class="rounded-lg border border-slate-700/40 bg-slate-800/30 px-3 py-2">
                <div class="text-[10px] uppercase tracking-wide text-slate-500">Total</div>
                <div class="mt-1 text-sm font-semibold text-slate-200">{{ formatBytes(clickHouse?.disk_total_bytes ?? 0) }}</div>
              </div>
              <div class="rounded-lg border border-slate-700/40 bg-slate-800/30 px-3 py-2">
                <div class="text-[10px] uppercase tracking-wide text-slate-500">Used</div>
                <div class="mt-1 text-sm font-semibold text-amber-300">{{ formatBytes(clickHouse?.disk_used_bytes ?? 0) }}</div>
              </div>
              <div class="rounded-lg border border-slate-700/40 bg-slate-800/30 px-3 py-2">
                <div class="text-[10px] uppercase tracking-wide text-slate-500">Free</div>
                <div class="mt-1 text-sm font-semibold text-emerald-300">{{ formatBytes(clickHouse?.disk_free_bytes ?? 0) }}</div>
              </div>
            </div>
          </div>
        </DataCard>

        <DataCard>
          <template #header>
            <div>
              <h2 class="text-sm font-semibold text-slate-200">Connection</h2>
              <p class="mt-0.5 text-xs text-slate-400">Values reported by the node serving this snapshot.</p>
            </div>
          </template>
          <dl class="divide-y divide-slate-700/20 text-sm">
            <div class="flex items-center justify-between gap-4 px-6 py-3">
              <dt class="text-xs uppercase tracking-wide text-slate-500">Endpoint</dt>
              <dd class="max-w-[300px] truncate font-mono text-xs text-slate-300" :title="clickHouse?.url">{{ clickHouse?.url || '—' }}</dd>
            </div>
            <div class="flex items-center justify-between gap-4 px-6 py-3">
              <dt class="text-xs uppercase tracking-wide text-slate-500">Database</dt>
              <dd class="font-mono text-xs text-slate-300">{{ clickHouse?.database || '—' }}</dd>
            </div>
            <div class="flex items-center justify-between gap-4 px-6 py-3">
              <dt class="text-xs uppercase tracking-wide text-slate-500">Tenant</dt>
              <dd class="font-mono text-xs text-slate-300">{{ clickHouse?.tenant || '—' }}</dd>
            </div>
            <div class="flex items-center justify-between gap-4 px-6 py-3">
              <dt class="text-xs uppercase tracking-wide text-slate-500">Reporting node</dt>
              <dd class="font-mono text-xs text-slate-300">{{ clickHouse?.reporting_node || '—' }}</dd>
            </div>
            <div class="flex items-center justify-between gap-4 px-6 py-3">
              <dt class="text-xs uppercase tracking-wide text-slate-500">Measured</dt>
              <dd class="text-xs text-slate-400">{{ formatNano(clickHouse?.measured_unix_nano) }}</dd>
            </div>
          </dl>
        </DataCard>
      </div>
    </template>

    <!-- ================= Native engine ================= -->
    <template v-else>
      <div v-if="!nodes.length && !loading" class="mb-6 rounded-2xl border border-slate-700/40 bg-slate-900/60 p-10 text-center">
        <Archive class="mx-auto h-8 w-8 text-slate-500" />
        <h2 class="mt-3 text-sm font-semibold text-slate-200">No log engine nodes reported yet</h2>
        <p class="mt-1 text-xs text-slate-400">
          The compactor and DuckDB statistics appear after a MonoFS node registers with telemetry enabled.
        </p>
      </div>

      <div class="mb-6 grid grid-cols-2 gap-4 sm:grid-cols-4">
        <StatCard
          :icon="Layers"
          label="Stored buckets"
          :value="formatNumber(storedBuckets)"
          :sub="`${formatNumber(totalChunks)} chunks across ${nodes.length} node${nodes.length === 1 ? '' : 's'}`"
          tooltip="Sum of per-signal hourly buckets. A log and metric bucket in the same hour are counted separately. New chunks use partition/date/hour layout; legacy year/month/day/hour buckets are counted too."
        />
        <StatCard
          :icon="Archive"
          label="Compacted buckets"
          :value="formatNumber(compactedBuckets)"
          :sub="`${compactionPct.toFixed(1)}% of stored buckets`"
          tooltip="Hourly buckets that contain at least one compact-* output chunk. A bucket can still contain raw chunks alongside a compacted output."
        />
        <StatCard
          :icon="Layers"
          label="Raw buckets"
          :value="formatNumber(rawBuckets)"
          sub="Waiting for compaction"
          tooltip="Stored buckets without a compacted output chunk yet. Older buckets outside the 24h catch-up window need monofs-logengine-compact."
        />
        <StatCard
          :icon="Archive"
          label="Output chunks"
          :value="formatNumber(logEngine?.compaction_output_chunks ?? 0)"
          :sub="`${formatNumber(logEngine?.compaction_deleted_chunks ?? 0)} source chunks deleted`"
          tooltip="Compacted chunks written by the background worker, and the raw source chunks removed after a successful merge."
        />
        <StatCard
          :icon="ChartColumn"
          label="Records compacted"
          :value="formatNumber(logEngine?.compaction_records ?? 0)"
          sub="Total records read by compaction"
          tooltip="Sum of records read from source chunks during compaction."
        />
        <StatCard
          :icon="RefreshCw"
          label="Compaction runs"
          :value="formatNumber(logEngine?.compaction_runs ?? 0)"
          sub="Since the node started"
          tooltip="Number of successful and failed compaction runs reported by the local compactor."
        />
        <StatCard
          :icon="Database"
          label="DuckDB nodes"
          :value="`${duckdbNodes.length}/${nodes.length}`"
          :sub="`${formatNumber(logEngine?.duckdb_queries ?? 0)} queries · ${formatNumber(logEngine?.duckdb_errors ?? 0)} errors`"
          tooltip="Nodes whose DuckDB sidecar is enabled. DuckDB executes ad-hoc SQL against local Parquet chunks."
        />
        <StatCard
          :icon="HardDrive"
          label="Compaction target"
          :value="formatBytes(nodes[0]?.compaction_target_bytes ?? 0)"
          sub="Maximum output chunk size"
          tooltip="Target maximum size for a compacted output chunk. Larger chunks reduce directory and manifest overhead."
        />
      </div>

      <DataCard class="mb-6">
        <template #header>
          <div>
            <h2 class="text-sm font-semibold text-slate-200">Signals</h2>
            <p class="mt-0.5 text-xs text-slate-400">
              Buckets are counted per signal so an hourly log and metric bucket stay distinct.
            </p>
          </div>
        </template>
        <div class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="border-b border-slate-700/40 text-xs uppercase tracking-wider text-slate-400">
                <th class="px-6 py-3 text-left font-medium">Signal</th>
                <th class="px-6 py-3 text-right font-medium">Chunks</th>
                <th class="px-6 py-3 text-right font-medium">Stored buckets</th>
                <th class="px-6 py-3 text-right font-medium">Compacted buckets</th>
                <th class="px-6 py-3 text-right font-medium">Raw buckets</th>
                <th class="px-6 py-3 text-right font-medium">Compacted</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-slate-700/20">
              <tr v-for="row in signalRows" :key="row.signal" class="transition-colors hover:bg-slate-800/30">
                <td class="px-6 py-3 font-medium text-slate-200">{{ row.signal }}</td>
                <td class="px-6 py-3 text-right text-slate-300">{{ formatNumber(row.chunks) }}</td>
                <td class="px-6 py-3 text-right text-slate-300">{{ formatNumber(row.buckets) }}</td>
                <td class="px-6 py-3 text-right text-emerald-400">{{ formatNumber(row.compacted) }}</td>
                <td class="px-6 py-3 text-right text-amber-400">{{ formatNumber(row.raw) }}</td>
                <td class="px-6 py-3 text-right text-slate-300">
                  {{ row.buckets > 0 ? `${((row.compacted / row.buckets) * 100).toFixed(1)}%` : '—' }}
                </td>
              </tr>
            </tbody>
            <tfoot class="border-t border-slate-700/40 bg-slate-800/20">
              <tr class="font-semibold">
                <td class="px-6 py-3 text-slate-200">Total</td>
                <td class="px-6 py-3 text-right text-slate-200">{{ formatNumber(totalChunks) }}</td>
                <td class="px-6 py-3 text-right text-slate-200">{{ formatNumber(storedBuckets) }}</td>
                <td class="px-6 py-3 text-right text-emerald-400">{{ formatNumber(compactedBuckets) }}</td>
                <td class="px-6 py-3 text-right text-amber-400">{{ formatNumber(rawBuckets) }}</td>
                <td class="px-6 py-3 text-right text-slate-200">{{ compactionPct.toFixed(1) }}%</td>
              </tr>
            </tfoot>
          </table>
        </div>
      </DataCard>

      <DataCard class="mb-6">
        <template #header>
          <div>
            <h2 class="text-sm font-semibold text-slate-200">Compaction by node</h2>
            <p class="mt-0.5 text-xs text-slate-400">Per-node chunk and bucket counts plus the local compactor counters.</p>
          </div>
        </template>
        <div class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="border-b border-slate-700/40 text-xs uppercase tracking-wider text-slate-400">
                <th class="px-4 py-3 text-left font-medium">Node</th>
                <th class="px-4 py-3 text-right font-medium">Logs</th>
                <th class="px-4 py-3 text-right font-medium">Metrics</th>
                <th class="px-4 py-3 text-right font-medium">Traces</th>
                <th class="px-4 py-3 text-right font-medium">Buckets</th>
                <th class="px-4 py-3 text-right font-medium">Compacted</th>
                <th class="px-4 py-3 text-right font-medium">Runs</th>
                <th class="px-4 py-3 text-right font-medium">Inputs</th>
                <th class="px-4 py-3 text-right font-medium">Outputs</th>
                <th class="px-4 py-3 text-right font-medium">Deleted</th>
                <th class="px-4 py-3 text-right font-medium">Records</th>
                <th class="px-4 py-3 text-right font-medium">Last run</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-slate-700/20">
              <tr v-for="node in nodes" :key="node.node_id || node.address" class="transition-colors hover:bg-slate-800/30">
                <td class="px-4 py-3">
                  <div class="font-mono text-xs text-slate-200">{{ node.node_id || node.address }}</div>
                  <div class="mt-0.5 text-[10px] text-slate-500">{{ node.address }}</div>
                </td>
                <td class="px-4 py-3 text-right text-slate-300">{{ formatNumber(node.log_chunks) }}</td>
                <td class="px-4 py-3 text-right text-slate-300">{{ formatNumber(node.metric_chunks) }}</td>
                <td class="px-4 py-3 text-right text-slate-300">{{ formatNumber(node.trace_chunks) }}</td>
                <td class="px-4 py-3 text-right text-slate-300">{{ formatNumber(nodeBuckets(node)) }}</td>
                <td class="px-4 py-3 text-right text-emerald-400">{{ formatNumber(nodeCompactedBuckets(node)) }}</td>
                <td class="px-4 py-3 text-right text-slate-300">
                  <span v-if="node.compaction_running" class="inline-flex items-center gap-1 text-amber-400">
                    <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-amber-400"></span>
                    running
                  </span>
                  <span v-else>{{ formatNumber(node.compaction_runs ?? 0) }}</span>
                </td>
                <td class="px-4 py-3 text-right text-slate-300">{{ formatNumber(node.compaction_input_chunks ?? 0) }}</td>
                <td class="px-4 py-3 text-right text-slate-300">{{ formatNumber(node.compaction_output_chunks ?? 0) }}</td>
                <td class="px-4 py-3 text-right text-slate-300">{{ formatNumber(node.compaction_deleted_chunks ?? 0) }}</td>
                <td class="px-4 py-3 text-right text-slate-300">{{ formatNumber(node.compaction_records ?? 0) }}</td>
                <td class="px-4 py-3 text-right text-xs text-slate-400">{{ formatNano(node.last_compaction_unix_nano) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </DataCard>

      <DataCard>
        <template #header>
          <div class="flex flex-wrap items-center justify-between gap-3">
            <div>
              <h2 class="text-sm font-semibold text-slate-200">DuckDB query engine</h2>
              <p class="mt-0.5 text-xs text-slate-400">
                Local Parquet SQL sidecar used for ad-hoc queries. Queries and errors are cumulative since startup.
              </p>
            </div>
            <div class="flex items-center gap-2 text-xs">
              <span class="inline-flex items-center gap-1 rounded-full border border-emerald-500/30 bg-emerald-500/10 px-2 py-1 font-semibold text-emerald-300">
                <Database class="h-3 w-3" />
                {{ duckdbNodes.length }} enabled
              </span>
              <span class="inline-flex items-center gap-1 rounded-full border border-slate-700/50 bg-slate-800/40 px-2 py-1 text-slate-400">
                <Layers class="h-3 w-3" />
                {{ nodes.length }} total nodes
              </span>
            </div>
          </div>
        </template>
        <div v-if="!duckdbNodes.length" class="px-6 py-10 text-center">
          <HardDrive class="mx-auto h-7 w-7 text-slate-500" />
          <p class="mt-2 text-sm text-slate-300">DuckDB sidecars are not enabled on these nodes.</p>
          <p class="mt-1 text-xs text-slate-500">Deploy the logquery sidecar and enable DuckDB on the log engine.</p>
        </div>
        <div v-else class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="border-b border-slate-700/40 text-xs uppercase tracking-wider text-slate-400">
                <th class="px-6 py-3 text-left font-medium">Node</th>
                <th class="px-6 py-3 text-left font-medium">State</th>
                <th class="px-6 py-3 text-left font-medium">Memory</th>
                <th class="px-6 py-3 text-right font-medium">Threads</th>
                <th class="px-6 py-3 text-left font-medium">Temp directory</th>
                <th class="px-6 py-3 text-right font-medium">Queries</th>
                <th class="px-6 py-3 text-right font-medium">Errors</th>
                <th class="px-6 py-3 text-right font-medium">Fallbacks</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-slate-700/20">
              <tr v-for="node in nodes" :key="`duck-${node.node_id || node.address}`" class="transition-colors hover:bg-slate-800/30">
                <td class="px-6 py-3 font-mono text-xs text-slate-300">{{ node.node_id || node.address }}</td>
                <td class="px-6 py-3">
                  <span
                    class="inline-flex rounded-full border px-2 py-0.5 text-[10px] font-bold uppercase tracking-wide"
                    :class="node.duckdb_enabled
                      ? 'border-emerald-500/30 bg-emerald-500/10 text-emerald-300'
                      : 'border-slate-600/30 bg-slate-700/20 text-slate-400'"
                  >
                    {{ node.duckdb_enabled ? 'enabled' : 'native' }}
                  </span>
                </td>
                <td class="px-6 py-3 text-slate-300">{{ node.duckdb_memory_limit || '—' }}</td>
                <td class="px-6 py-3 text-right text-slate-300">{{ node.duckdb_threads ?? '—' }}</td>
                <td class="max-w-[260px] truncate px-6 py-3 font-mono text-xs text-slate-400" :title="node.duckdb_temp_dir">
                  {{ node.duckdb_temp_dir || '—' }}
                </td>
                <td class="px-6 py-3 text-right text-slate-300">{{ formatNumber(node.duckdb_queries ?? 0) }}</td>
                <td class="px-6 py-3 text-right" :class="(node.duckdb_errors ?? 0) > 0 ? 'text-rose-400' : 'text-slate-300'">
                  {{ formatNumber(node.duckdb_errors ?? 0) }}
                </td>
                <td class="px-6 py-3 text-right" :class="(node.duckdb_fallbacks ?? 0) > 0 ? 'text-amber-400' : 'text-slate-300'">
                  {{ formatNumber(node.duckdb_fallbacks ?? 0) }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <div class="border-t border-slate-700/40 px-6 py-3 text-xs text-slate-500">
          <span class="inline-flex items-center gap-1.5">
            <TrendingUp class="h-3 w-3" />
            Query counts include SQL issued through the sidecar and DuckDB-backed log, metric, and trace queries.
          </span>
          <span class="ml-3 inline-flex items-center gap-1.5">
            <Trash2 class="h-3 w-3" />
            Fallbacks indicate a DuckDB error that was served by the native scanner instead.
          </span>
        </div>
      </DataCard>
    </template>
  </div>
</template>
