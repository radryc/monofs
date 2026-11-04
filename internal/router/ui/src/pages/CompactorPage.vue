<script setup lang="ts">
import { computed, ref } from 'vue'
import { Archive, ChartColumn, Database, HardDrive, Layers, RefreshCw, Trash2, TrendingUp } from 'lucide-vue-next'
import { useAutoRefresh, formatBytes, formatNumber } from '../composables/useAutoRefresh'
import PageHeader from '../components/PageHeader.vue'
import StatCard from '../components/StatCard.vue'
import DataCard from '../components/DataCard.vue'
import type { LogEngineData, LogEngineNode } from '../types/api'

const logEngine = ref<LogEngineData | null>(null)

async function loadLogEngine() {
  logEngine.value = await fetch('/api/logengine').then(r => r.json())
}

const { loading } = useAutoRefresh(loadLogEngine, 10_000)

const nodes = computed<LogEngineNode[]>(() => logEngine.value?.nodes ?? [])

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
      title="Compactor & DuckDB"
      subtitle="Hourly bucket storage, background compaction, and the DuckDB query sidecar"
    >
      <template #actions>
        <button
          type="button"
          class="inline-flex items-center gap-2 rounded-lg border border-slate-700/50 bg-slate-900/60 px-3 py-2 text-xs font-semibold text-slate-300 hover:border-slate-500 hover:text-slate-100"
          @click="loadLogEngine"
        >
          <RefreshCw class="h-3.5 w-3.5" :class="{ 'animate-spin': loading }" />
          Refresh
        </button>
      </template>
    </PageHeader>

    <div v-if="!nodes.length && !loading" class="mb-6 rounded-2xl border border-slate-700/40 bg-slate-900/60 p-10 text-center">
      <Archive class="mx-auto h-8 w-8 text-slate-500" />
      <h2 class="mt-3 text-sm font-semibold text-slate-200">No log engine nodes reported yet</h2>
      <p class="mt-1 text-xs text-slate-400">
        The compactor and DuckDB statistics appear after a MonoFS node registers with telemetry enabled.
      </p>
    </div>

    <template v-else>
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
