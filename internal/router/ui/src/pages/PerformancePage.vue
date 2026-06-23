<script setup lang="ts">
import { ref, computed } from 'vue'
import { useAutoRefresh, formatBytes, formatNumber } from '../composables/useAutoRefresh'
import PageHeader from '../components/PageHeader.vue'
import StatCard from '../components/StatCard.vue'
import DataCard from '../components/DataCard.vue'
import type { ListClientsResponse, PredictorStats, PprofCollectRequest, PprofProfile } from '../types/api'

const clients = ref<ListClientsResponse | null>(null)
const predictor = ref<PredictorStats | null>(null)
const collectingPprof = ref(false)
const pprofError = ref('')
const pprofSuccess = ref('')
const cpuDurationSeconds = ref(30)

const profileOptions: Array<{ key: PprofProfile; label: string; advanced?: boolean }> = [
  { key: 'cpu', label: 'CPU' },
  { key: 'heap', label: 'Heap' },
  { key: 'goroutine', label: 'Goroutine' },
  { key: 'allocs', label: 'Allocs', advanced: true },
  { key: 'mutex', label: 'Mutex', advanced: true },
  { key: 'block', label: 'Block', advanced: true },
  { key: 'threadcreate', label: 'Thread Create', advanced: true },
  { key: 'trace', label: 'Trace', advanced: true }
]

const selectedProfiles = ref<PprofProfile[]>(['cpu', 'heap', 'goroutine'])

const totalOps = computed(() => clients.value?.clients?.reduce((s, c) => s + (c.operations_count ?? 0), 0) ?? 0)
const totalBytes = computed(() => clients.value?.clients?.reduce((s, c) => s + (c.bytes_read ?? 0), 0) ?? 0)
const totalErrors = computed(() => clients.value?.clients?.reduce((s, c) => s + (c.errors_count ?? 0), 0) ?? 0)

async function loadClients() {
  clients.value = await fetch('/api/clients').then(r => r.json())
}

async function loadPredictor() {
  predictor.value = await fetch('/api/predictor').then(r => r.json())
}

const { loading: clientsLoading } = useAutoRefresh(loadClients, 15_000)
const { loading: predLoading } = useAutoRefresh(loadPredictor, 10_000)

const top5Ops = computed(() =>
  [...(clients.value?.clients ?? [])].sort((a, b) => (b.operations_count ?? 0) - (a.operations_count ?? 0)).slice(0, 5)
)
const top5Bytes = computed(() =>
  [...(clients.value?.clients ?? [])].sort((a, b) => (b.bytes_read ?? 0) - (a.bytes_read ?? 0)).slice(0, 5)
)

async function collectClusterPprof() {
  if (selectedProfiles.value.length === 0) {
    pprofError.value = 'Select at least one profile'
    return
  }

  collectingPprof.value = true
  pprofError.value = ''
  pprofSuccess.value = ''

  const requestBody: PprofCollectRequest = {
    profiles: selectedProfiles.value,
    cpu_duration_seconds: Math.max(1, Math.min(120, cpuDurationSeconds.value || 30))
  }

  try {
    const response = await fetch('/api/pprof/collect', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(requestBody)
    })

    if (!response.ok) {
      const message = await response.text()
      throw new Error(message || `Collect failed (${response.status})`)
    }

    const blob = await response.blob()
    const contentDisposition = response.headers.get('Content-Disposition') || ''
    const match = contentDisposition.match(/filename="?([^";]+)"?/i)
    const fileName = match?.[1] ?? `monofs-pprof-${Date.now()}.zip`

    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = fileName
    document.body.appendChild(a)
    a.click()
    a.remove()
    URL.revokeObjectURL(url)

    const targetCount = parseInt(response.headers.get('X-Pprof-Target-Count') ?? '0', 10)
    const successTargets = parseInt(response.headers.get('X-Pprof-Success-Targets') ?? '0', 10)
    const failedTargets = parseInt(response.headers.get('X-Pprof-Failed-Targets') ?? '0', 10)

    let msg = `Collected ${selectedProfiles.value.length} profile type(s) from ${targetCount} target(s)`
    if (failedTargets > 0) {
      msg += ` (${successTargets} ok, ${failedTargets} failed)`
    }
    msg += ` into ${fileName}`
    pprofSuccess.value = msg
  } catch (error) {
    pprofError.value = error instanceof Error ? error.message : 'Failed to collect cluster pprof'
  } finally {
    collectingPprof.value = false
  }
}
</script>

<template>
  <div>
    <PageHeader title="Performance" subtitle="Client operation metrics and prefetch predictor stats" />

    <div class="grid grid-cols-2 sm:grid-cols-4 gap-4 mb-6">
      <StatCard icon="⚡" label="Total Operations" :value="formatNumber(totalOps)" />
      <StatCard icon="💾" label="Data Transferred" :value="formatBytes(totalBytes)" />
      <StatCard icon="❌" label="Total Errors" :value="totalErrors" />
      <StatCard icon="💻" label="Active Clients" :value="clients?.clients?.length ?? '-'" />
    </div>

    <div class="grid grid-cols-1 xl:grid-cols-2 gap-6 mb-6">
      <!-- Top by ops -->
      <DataCard :loading="clientsLoading">
        <template #header>
          <h2 class="text-sm font-semibold text-slate-200">Top Clients by Operations</h2>
        </template>
        <div v-if="top5Ops.length" class="divide-y divide-slate-700/20">
          <div v-for="(c, i) in top5Ops" :key="c.client_id" class="flex items-center gap-4 px-6 py-3">
            <span class="w-6 text-center text-xs font-bold text-slate-500">#{{ i + 1 }}</span>
            <div class="flex-1 min-w-0">
              <div class="font-mono text-xs text-violet-300 truncate">{{ c.client_id }}</div>
              <div class="text-xs text-slate-500">{{ c.hostname || c.mount_point }}</div>
            </div>
            <div class="text-right">
              <div class="text-sm font-semibold text-slate-200">{{ formatNumber(c.operations_count ?? 0) }}</div>
              <div class="text-xs text-slate-400">ops</div>
            </div>
          </div>
        </div>
        <div v-else class="py-10 text-center text-sm text-slate-400">No clients</div>
      </DataCard>

      <!-- Top by bytes -->
      <DataCard :loading="clientsLoading">
        <template #header>
          <h2 class="text-sm font-semibold text-slate-200">Top Clients by Data Read</h2>
        </template>
        <div v-if="top5Bytes.length" class="divide-y divide-slate-700/20">
          <div v-for="(c, i) in top5Bytes" :key="c.client_id" class="flex items-center gap-4 px-6 py-3">
            <span class="w-6 text-center text-xs font-bold text-slate-500">#{{ i + 1 }}</span>
            <div class="flex-1 min-w-0">
              <div class="font-mono text-xs text-violet-300 truncate">{{ c.client_id }}</div>
              <div class="text-xs text-slate-500">{{ c.hostname || c.mount_point }}</div>
            </div>
            <div class="text-right">
              <div class="text-sm font-semibold text-slate-200">{{ formatBytes(c.bytes_read ?? 0) }}</div>
              <div class="text-xs text-slate-400">read</div>
            </div>
          </div>
        </div>
        <div v-else class="py-10 text-center text-sm text-slate-400">No clients</div>
      </DataCard>
    </div>

    <!-- All clients table -->
    <DataCard :loading="clientsLoading" class="mb-6">
      <template #header>
        <h2 class="text-sm font-semibold text-slate-200">All Clients</h2>
      </template>
      <div v-if="clients?.clients?.length" class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-slate-700/40 text-xs text-slate-400 uppercase tracking-wider">
              <th class="text-left px-6 py-3 font-medium">Client ID</th>
              <th class="text-left px-6 py-3 font-medium">Hostname</th>
              <th class="text-right px-6 py-3 font-medium">Ops</th>
              <th class="text-right px-6 py-3 font-medium">Bytes</th>
              <th class="text-right px-6 py-3 font-medium">Errors</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-700/20">
            <tr v-for="c in clients.clients" :key="c.client_id" class="hover:bg-slate-800/30 transition-colors">
              <td class="px-6 py-3 font-mono text-xs text-violet-300">{{ c.client_id }}</td>
              <td class="px-6 py-3 text-xs text-slate-400">{{ c.hostname || '-' }}</td>
              <td class="px-6 py-3 text-right text-slate-300">{{ formatNumber(c.operations_count ?? 0) }}</td>
              <td class="px-6 py-3 text-right text-slate-300">{{ formatBytes(c.bytes_read ?? 0) }}</td>
              <td class="px-6 py-3 text-right" :class="(c.errors_count ?? 0) > 0 ? 'text-rose-400' : 'text-slate-400'">{{ c.errors_count ?? 0 }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <div v-else class="py-12 text-center text-slate-400 text-sm">No clients connected</div>
    </DataCard>

    <DataCard class="mb-6">
      <template #header>
        <div>
          <h2 class="text-sm font-semibold text-slate-200">Cluster pprof</h2>
          <p class="text-xs text-slate-400 mt-0.5">Collect pprof from routers, servers, fetchers, and search as one zip.</p>
        </div>
      </template>
      <div class="px-6 py-5">
        <div class="grid grid-cols-1 md:grid-cols-3 gap-3 mb-4">
          <label v-for="option in profileOptions" :key="option.key" class="flex items-center gap-2 text-sm text-slate-300">
            <input
              :value="option.key"
              v-model="selectedProfiles"
              type="checkbox"
              class="h-4 w-4 rounded border-slate-600 bg-slate-900 text-violet-500 focus:ring-violet-500"
            />
            <span>{{ option.label }} <span v-if="option.advanced" class="text-xs text-slate-500">(advanced)</span></span>
          </label>
        </div>

        <div class="flex flex-col sm:flex-row sm:items-end gap-3 mb-4">
          <label class="text-xs text-slate-400">
            CPU duration (seconds)
            <input
              v-model.number="cpuDurationSeconds"
              type="number"
              min="1"
              max="120"
              class="mt-1 w-44 rounded-md border border-slate-700 bg-slate-900 px-3 py-2 text-sm text-slate-200"
            />
          </label>
          <button
            type="button"
            class="rounded-md bg-violet-600 hover:bg-violet-500 disabled:opacity-60 disabled:cursor-not-allowed px-4 py-2 text-sm font-medium text-white"
            :disabled="collectingPprof"
            @click="collectClusterPprof"
          >
            {{ collectingPprof ? 'Collecting...' : 'Collect From All Servers' }}
          </button>
        </div>

        <p v-if="pprofSuccess" class="text-xs text-emerald-400">{{ pprofSuccess }}</p>
        <p v-if="pprofError" class="text-xs text-rose-400">{{ pprofError }}</p>
      </div>
    </DataCard>

    <!-- Predictor -->
    <DataCard :loading="predLoading">
      <template #header>
        <div>
          <h2 class="text-sm font-semibold text-slate-200">Prefetch Predictor (Markov)</h2>
          <p class="text-xs text-slate-400 mt-0.5">Refreshes every 10s</p>
        </div>
      </template>
      <div v-if="predictor">
        <div class="grid grid-cols-2 sm:grid-cols-4 gap-4 px-6 py-5 border-b border-slate-700/30">
          <div class="text-center">
            <div class="text-xs text-slate-400 mb-1">Predictions</div>
            <div class="text-xl font-bold text-slate-200">{{ formatNumber(predictor.total_predictions) }}</div>
          </div>
          <div class="text-center">
            <div class="text-xs text-slate-400 mb-1">Prefetches</div>
            <div class="text-xl font-bold text-slate-200">{{ formatNumber(predictor.total_prefetches) }}</div>
          </div>
          <div class="text-center">
            <div class="text-xs text-slate-400 mb-1">Hits</div>
            <div class="text-xl font-bold text-emerald-400">{{ formatNumber(predictor.total_hits) }}</div>
          </div>
          <div class="text-center">
            <div class="text-xs text-slate-400 mb-1">Hit Rate</div>
            <div class="text-xl font-bold text-violet-400">{{ ((predictor.cluster_hit_rate || 0) * 100).toFixed(1) }}%</div>
          </div>
        </div>
        <div class="divide-y divide-slate-700/20">
          <div v-for="node in predictor.nodes" :key="node.address" class="px-6 py-4">
            <div class="flex items-center justify-between mb-2">
              <span class="text-sm font-medium text-slate-300">{{ node.address }}</span>
              <span class="text-xs text-violet-400">{{ ((node.hit_rate || 0) * 100).toFixed(1) }}% hit rate</span>
            </div>
            <div class="grid grid-cols-3 gap-4 text-xs text-slate-400 mb-2">
              <span>Predictions: {{ formatNumber(node.predictions) }}</span>
              <span>Prefetches: {{ formatNumber(node.prefetches) }}</span>
              <span class="text-emerald-400">Hits: {{ formatNumber(node.prefetch_hits ?? 0) }}</span>
            </div>
            <div v-if="node.error" class="text-xs text-rose-400 mt-1">{{ node.error }}</div>
          </div>
        </div>
      </div>
      <div v-else class="py-12 text-center text-slate-400 text-sm">No predictor data</div>
    </DataCard>
  </div>
</template>
