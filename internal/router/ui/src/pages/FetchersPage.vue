<script setup lang="ts">
import { ref, computed } from 'vue'
import { useAutoRefresh, formatBytes, formatNumber } from '../composables/useAutoRefresh'
import PageHeader from '../components/PageHeader.vue'
import StatCard from '../components/StatCard.vue'
import DataCard from '../components/DataCard.vue'
import NodeBadge from '../components/NodeBadge.vue'
import type { FetcherStats, LogEngineData } from '../types/api'

const fetchers = ref<FetcherStats | null>(null)
const logEngine = ref<LogEngineData | null>(null)
const detailed = ref(false)

async function loadFetchers() {
  const url = detailed.value ? '/api/fetchers?detailed=true' : '/api/fetchers'
  fetchers.value = await fetch(url).then(r => r.json())
}

async function loadLogEngine() {
  logEngine.value = await fetch('/api/logengine').then(r => r.json())
}

const { loading: fetchersLoading } = useAutoRefresh(async () => {
  await Promise.allSettled([loadFetchers(), loadLogEngine()])
}, 10_000)

function hitColor(rate: number): string {
  if (rate >= 0.9) return 'text-emerald-400'
  if (rate >= 0.6) return 'text-amber-400'
  return 'text-rose-400'
}

// Backend type metadata
const backendMeta: Record<string, { icon: string; label: string; desc: string }> = {
  git:  { icon: '📁', label: 'Git',                desc: 'Cloned repository objects' },
  blob: { icon: '📦', label: 'Packager Archives',  desc: 'Compacted archive objects' },
  s3:   { icon: '☁️', label: 'S3',                 desc: 'S3-compatible object storage' },
  http: { icon: '🌐', label: 'HTTP',               desc: 'Generic HTTP sources' },
  oci:  { icon: '🐳', label: 'OCI',                desc: 'OCI registry images' },
}

const blobStatsEntries = computed(() => {
  const bs = fetchers.value?.blob_stats
  if (!bs) return []
  const total = Object.values(bs).reduce((s, v) => s + v.blob_bytes, 0)
  return Object.entries(bs).map(([key, val]) => ({
    key,
    ...val,
    meta: backendMeta[key] ?? { icon: '📦', label: key, desc: key },
    pct: total > 0 ? ((val.blob_bytes / total) * 100) : 0,
  }))
})

const storageBlobsEntries = computed(() => {
  const sb = fetchers.value?.storage_blobs
  if (!sb) return []
  return Object.entries(sb).map(([key, val]) => ({ key, ...val }))
})
</script>

<template>
  <div>
    <PageHeader title="Fetchers" subtitle="Blob fetcher services for external data access (DMZ layer)" />

    <!-- Overview stat cards -->
    <div v-if="fetchers" class="grid grid-cols-2 sm:grid-cols-4 gap-4 mb-6">
      <StatCard
        icon="🔄"
        label="Fetchers"
        :value="`${fetchers.healthy_fetchers ?? 0}/${fetchers.total_fetchers ?? 0}`"
        :sub="`${fetchers.total_fetchers ? (((fetchers.healthy_fetchers ?? 0) / fetchers.total_fetchers) * 100).toFixed(0) : 0}% healthy`"
      />
      <StatCard icon="📥" label="Total Requests" :value="formatNumber(fetchers.total_requests ?? 0)" />
      <StatCard icon="💾" label="Cache Hit Rate"
        :value="`${((fetchers.aggregated_hit_rate ?? 0) * 100).toFixed(1)}%`" />
      <StatCard icon="📦" label="Data Served" :value="formatBytes(fetchers.total_bytes_served ?? 0)" />
      <StatCard icon="🧰" label="Sync Jobs"
        :value="`${fetchers.sync_worker?.active_jobs ?? 0}/${fetchers.sync_worker?.total_jobs ?? 0}`"
        :sub="`${formatNumber(fetchers.sync_worker?.publish_jobs ?? 0)} publish jobs`"
      />
      <StatCard icon="🚀" label="Published Repos" :value="formatNumber(fetchers.sync_worker?.published_repositories ?? 0)" />
      <StatCard icon="📦" label="Staged Bundles" :value="formatNumber(fetchers.sync_worker?.staged_bundles ?? 0)" />
      <StatCard icon="🗂️" label="Worktree Bytes" :value="formatBytes(fetchers.sync_worker?.worktree_bytes ?? 0)" />
    </div>

    <DataCard v-if="fetchers" class="mb-6" :loading="fetchersLoading">
      <template #header>
        <h2 class="text-sm font-semibold text-slate-200">Sync Worker</h2>
      </template>
      <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4 p-5 text-xs">
        <div class="bg-slate-800/40 rounded-xl border border-slate-700/30 p-4 space-y-1.5">
          <div class="text-slate-400">Active Jobs</div>
          <div class="text-lg font-semibold text-slate-200">{{ formatNumber(fetchers.sync_worker?.active_jobs ?? 0) }}</div>
          <div class="text-slate-500">{{ formatNumber(fetchers.sync_worker?.completed_jobs ?? 0) }} completed</div>
        </div>
        <div class="bg-slate-800/40 rounded-xl border border-slate-700/30 p-4 space-y-1.5">
          <div class="text-slate-400">Publishes</div>
          <div class="text-lg font-semibold text-slate-200">{{ formatNumber(fetchers.sync_worker?.publish_jobs ?? 0) }}</div>
          <div class="text-slate-500">{{ formatNumber(fetchers.sync_worker?.published_repositories ?? 0) }} repos pushed</div>
        </div>
        <div class="bg-slate-800/40 rounded-xl border border-slate-700/30 p-4 space-y-1.5">
          <div class="text-slate-400">Bundle Cache</div>
          <div class="text-lg font-semibold text-slate-200">{{ formatNumber(fetchers.sync_worker?.staged_bundles ?? 0) }}</div>
          <div class="text-slate-500">{{ formatBytes(fetchers.sync_worker?.staged_bundle_bytes ?? 0) }}</div>
        </div>
        <div class="bg-slate-800/40 rounded-xl border border-slate-700/30 p-4 space-y-1.5">
          <div class="text-slate-400">Git Cache</div>
          <div class="text-lg font-semibold text-slate-200">{{ formatNumber(fetchers.sync_worker?.git_cache_entries ?? 0) }}</div>
          <div class="text-slate-500">{{ formatNumber(fetchers.sync_worker?.bundle_stage_failures ?? 0) }} stage failures</div>
        </div>
      </div>
    </DataCard>

    <!-- Archive Storage by backend type -->
    <DataCard v-if="blobStatsEntries.length" class="mb-6" :loading="fetchersLoading">
      <template #header>
        <h2 class="text-sm font-semibold text-slate-200">Archive Storage</h2>
      </template>
      <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4 p-5">
        <div
          v-for="entry in blobStatsEntries"
          :key="entry.key"
          class="bg-slate-800/40 rounded-xl border border-slate-700/30 p-4"
        >
          <div class="flex items-center gap-2 mb-2">
            <span class="text-xl">{{ entry.meta.icon }}</span>
            <div>
              <div class="text-sm font-semibold text-slate-200">{{ entry.meta.label }}</div>
              <div class="text-xs text-slate-500">{{ entry.meta.desc }}</div>
            </div>
          </div>
          <div class="text-xs space-y-1 mt-3">
            <div class="flex justify-between">
              <span class="text-slate-400">Archives</span>
              <span class="text-slate-200 font-medium">{{ formatNumber(entry.blob_count) }}</span>
            </div>
            <div class="flex justify-between">
              <span class="text-slate-400">Size</span>
              <span class="text-slate-200 font-medium">{{ formatBytes(entry.blob_bytes) }}</span>
            </div>
            <div class="flex justify-between">
              <span class="text-slate-400">Share</span>
              <span class="text-slate-200 font-medium">{{ entry.pct.toFixed(1) }}%</span>
            </div>
          </div>
          <div class="mt-2 h-1 bg-slate-700 rounded-full overflow-hidden">
            <div class="h-full bg-violet-500 rounded-full" :style="{ width: `${entry.pct}%` }"></div>
          </div>
        </div>
      </div>

      <!-- Per-dependency file breakdown -->
      <div v-if="storageBlobsEntries.length" class="border-t border-slate-700/40 px-6 py-4">
        <h3 class="text-sm font-semibold text-slate-300 mb-3">Per-Dependency Files</h3>
        <table class="w-full text-sm">
          <thead>
            <tr class="text-xs text-slate-400 uppercase tracking-wider border-b border-slate-700/40">
              <th class="text-left py-2 font-medium">Key</th>
              <th class="text-right py-2 font-medium">Blobs</th>
              <th class="text-right py-2 font-medium">Size</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-700/20">
            <tr v-for="entry in storageBlobsEntries" :key="entry.key" class="hover:bg-slate-800/30">
              <td class="py-2 font-mono text-xs text-slate-400">{{ entry.key }}</td>
              <td class="py-2 text-right text-slate-300">{{ formatNumber(entry.blob_count) }}</td>
              <td class="py-2 text-right text-slate-300">{{ formatBytes(entry.blob_bytes) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </DataCard>

    <!-- Log Store / Doctor telemetry -->
    <DataCard v-if="logEngine?.nodes?.length" class="mb-6">
      <template #header>
        <div>
          <h2 class="text-sm font-semibold text-slate-200">Log Store</h2>
          <p class="text-xs text-slate-400 mt-0.5">Doctor telemetry engine — per-node chunk counts</p>
        </div>
      </template>
      <div class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-slate-700/40 text-xs text-slate-400 uppercase tracking-wider">
              <th class="text-left px-6 py-3 font-medium">Node</th>
              <th class="text-right px-6 py-3 font-medium">Logs</th>
              <th class="text-right px-6 py-3 font-medium">Metrics</th>
              <th class="text-right px-6 py-3 font-medium">Traces</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-700/20">
            <tr v-for="n in logEngine.nodes" :key="n.address" class="hover:bg-slate-800/30 transition-colors">
              <td class="px-6 py-3 font-mono text-xs text-slate-300">{{ n.node_id || n.address }}</td>
              <td class="px-6 py-3 text-right text-slate-300">{{ formatNumber(n.log_chunks) }}</td>
              <td class="px-6 py-3 text-right text-slate-300">{{ formatNumber(n.metric_chunks) }}</td>
              <td class="px-6 py-3 text-right text-slate-300">{{ formatNumber(n.trace_chunks) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </DataCard>

    <!-- Fetcher instances -->
    <DataCard :loading="fetchersLoading" class="mb-6">
      <template #header>
        <div class="flex items-center justify-between">
          <h2 class="text-sm font-semibold text-slate-200">Fetcher Instances</h2>
          <button
            @click="loadFetchers"
            class="text-xs text-violet-400 hover:text-violet-300 transition-colors px-3 py-1 rounded hover:bg-violet-500/10"
          >🔄 Refresh</button>
        </div>
      </template>
      <div v-if="fetchers?.fetchers?.length" class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4 p-5">
        <div
          v-for="f in fetchers.fetchers"
          :key="f.address"
          class="bg-slate-800/40 rounded-xl border p-4"
          :class="f.healthy ? 'border-slate-700/30' : 'border-rose-500/20'"
        >
          <div class="flex items-center justify-between mb-3">
            <div class="font-mono text-xs text-slate-300 truncate flex-1 mr-2">{{ f.address }}</div>
            <NodeBadge :healthy="f.healthy" />
          </div>
          <div class="grid grid-cols-2 gap-2 text-xs">
            <div class="bg-slate-900/40 rounded-lg p-2 text-center">
              <div class="text-slate-400 mb-1">Requests</div>
              <div class="font-semibold text-slate-200">{{ formatNumber(f.total_requests) }}</div>
            </div>
            <div class="bg-slate-900/40 rounded-lg p-2 text-center">
              <div class="text-slate-400 mb-1">Cache Rate</div>
              <div class="font-semibold" :class="hitColor(f.cache_hit_rate)">
                {{ ((f.cache_hit_rate || 0) * 100).toFixed(1) }}%
              </div>
            </div>
            <div class="bg-slate-900/40 rounded-lg p-2 text-center col-span-2">
              <div class="text-slate-400 mb-1">Bytes Served</div>
              <div class="font-semibold text-slate-200">{{ formatBytes(f.bytes_served) }}</div>
            </div>
            <div class="bg-slate-900/40 rounded-lg p-2 text-center">
              <div class="text-slate-400 mb-1">Sync Jobs</div>
              <div class="font-semibold text-slate-200">{{ formatNumber(f.sync_worker?.active_jobs ?? 0) }}</div>
            </div>
            <div class="bg-slate-900/40 rounded-lg p-2 text-center">
              <div class="text-slate-400 mb-1">Published</div>
              <div class="font-semibold text-slate-200">{{ formatNumber(f.sync_worker?.published_repositories ?? 0) }}</div>
            </div>
          </div>
        </div>
      </div>
      <div v-else class="py-12 text-center text-slate-400 text-sm">No fetcher instances available</div>
    </DataCard>

    <!-- Stats table -->
    <DataCard :loading="fetchersLoading">
      <template #header>
        <div class="flex items-center justify-between">
          <h2 class="text-sm font-semibold text-slate-200">Statistics</h2>
          <label class="flex items-center gap-2 text-sm text-slate-400 cursor-pointer select-none">
            <input
              type="checkbox"
              v-model="detailed"
              @change="loadFetchers"
              class="rounded border-slate-600 bg-slate-800 text-violet-500"
            />
            Per-source stats
          </label>
        </div>
      </template>
      <div v-if="fetchers?.fetchers?.length" class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-slate-700/40 text-xs text-slate-400 uppercase tracking-wider">
              <th class="text-left px-6 py-3 font-medium">Fetcher</th>
              <th class="text-right px-6 py-3 font-medium">Requests</th>
              <th class="text-right px-6 py-3 font-medium">Hits</th>
              <th class="text-right px-6 py-3 font-medium">Misses</th>
              <th class="text-right px-6 py-3 font-medium">Cache Rate</th>
              <th class="text-right px-6 py-3 font-medium">Bytes</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-700/20">
            <tr v-for="f in fetchers.fetchers" :key="f.address" class="hover:bg-slate-800/30 transition-colors">
              <td class="px-6 py-3 font-mono text-xs text-slate-300">{{ f.address }}</td>
              <td class="px-6 py-3 text-right text-slate-300">{{ formatNumber(f.total_requests) }}</td>
              <td class="px-6 py-3 text-right text-emerald-400">{{ formatNumber(f.cache_hits) }}</td>
              <td class="px-6 py-3 text-right text-slate-400">{{ formatNumber(f.cache_misses) }}</td>
              <td class="px-6 py-3 text-right" :class="hitColor(f.cache_hit_rate)">
                {{ ((f.cache_hit_rate || 0) * 100).toFixed(1) }}%
              </td>
              <td class="px-6 py-3 text-right text-slate-300">{{ formatBytes(f.bytes_served) }}</td>
            </tr>
            <!-- Totals row -->
            <tr class="border-t border-slate-600/40 bg-slate-800/20 font-semibold">
              <td class="px-6 py-3 text-slate-300">Total</td>
              <td class="px-6 py-3 text-right text-slate-200">{{ formatNumber(fetchers.total_requests ?? 0) }}</td>
              <td class="px-6 py-3 text-right text-emerald-400">{{ formatNumber(fetchers.total_cache_hits ?? 0) }}</td>
              <td class="px-6 py-3 text-right text-slate-400">{{ formatNumber(fetchers.total_cache_misses ?? 0) }}</td>
              <td class="px-6 py-3 text-right" :class="hitColor(fetchers.aggregated_hit_rate ?? 0)">
                {{ ((fetchers.aggregated_hit_rate ?? 0) * 100).toFixed(1) }}%
              </td>
              <td class="px-6 py-3 text-right text-slate-200">{{ formatBytes(fetchers.total_bytes_served ?? 0) }}</td>
            </tr>
          </tbody>
        </table>

        <!-- Per-source breakdown (only shown when detailed mode + fetcher has source_stats) -->
        <template v-if="detailed">
          <div
            v-for="f in fetchers.fetchers?.filter(f => f.source_stats && Object.keys(f.source_stats).length)"
            :key="`src-${f.address}`"
            class="border-t border-slate-700/40 px-6 py-5"
          >
            <h3 class="text-sm font-semibold text-slate-300 mb-3">Per-Source: {{ f.address }}</h3>
            <table class="w-full text-sm">
              <thead>
                <tr class="text-xs text-slate-400 uppercase tracking-wider">
                  <th class="text-left py-2 font-medium">Source</th>
                  <th class="text-right py-2 font-medium">Requests</th>
                  <th class="text-right py-2 font-medium">Errors</th>
                  <th class="text-right py-2 font-medium">Bytes Fetched</th>
                  <th class="text-right py-2 font-medium">Avg Latency</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-slate-700/20">
                <tr v-for="(s, srcKey) in f.source_stats" :key="srcKey" class="hover:bg-slate-800/30">
                  <td class="py-2 font-mono text-xs text-slate-400 truncate max-w-[200px]">{{ srcKey }}</td>
                  <td class="py-2 text-right text-slate-300">{{ formatNumber(s.requests) }}</td>
                  <td class="py-2 text-right" :class="s.errors > 0 ? 'text-rose-400' : 'text-slate-400'">{{ formatNumber(s.errors) }}</td>
                  <td class="py-2 text-right text-slate-300">{{ formatBytes(s.bytes_fetched) }}</td>
                  <td class="py-2 text-right text-slate-300">{{ s.avg_latency_ms?.toFixed(1) ?? '-' }}ms</td>
                </tr>
              </tbody>
            </table>
          </div>
        </template>
      </div>
      <div v-else class="py-12 text-center text-slate-400 text-sm">No stats available</div>
    </DataCard>
  </div>
</template>
