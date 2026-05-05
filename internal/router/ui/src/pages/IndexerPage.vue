<script setup lang="ts">
import { ref, computed } from 'vue'
import { useAppStore } from '../stores/app'
import { useAutoRefresh, formatBytes, formatNumber, formatDate } from '../composables/useAutoRefresh'
import PageHeader from '../components/PageHeader.vue'
import StatCard from '../components/StatCard.vue'
import DataCard from '../components/DataCard.vue'
import type { SearchStatsData, SearchIndexesResponse } from '../types/api'

const store = useAppStore()
const stats = ref<SearchStatsData | null>(null)
const indexes = ref<SearchIndexesResponse | null>(null)
const rebuilding = ref<Set<string>>(new Set())

const { loading: idxLoading } = useAutoRefresh(async () => {
  indexes.value = await fetch('/api/search/indexes').then(r => r.json())
}, 10_000)

useAutoRefresh(async () => {
  stats.value = await fetch('/api/search/stats').then(r => r.json())
}, 30_000)

const failedIndexes = computed(() => (indexes.value?.indexes ?? []).filter(i => i.status === 4))
const goodIndexes = computed(() => (indexes.value?.indexes ?? []).filter(i => i.status !== 4))
const safeStats = computed(() => ({
  total_indexes: stats.value?.total_indexes ?? 0,
  jobs_failed: stats.value?.jobs_failed ?? 0,
  jobs_rejected: stats.value?.jobs_rejected ?? 0,
  searches_total: stats.value?.searches_total ?? 0,
  total_files_indexed: stats.value?.total_files_indexed ?? 0,
  total_index_size_bytes: stats.value?.total_index_size_bytes ?? 0,
  queue_length: stats.value?.queue_length ?? 0,
  active_jobs: stats.value?.active_jobs ?? 0,
}))

async function rebuild(storageId?: string) {
  const key = storageId || '__all__'
  rebuilding.value = new Set([...rebuilding.value, key])
  try {
    const body = storageId ? { storage_id: storageId } : { all: true }
    await fetch('/api/search/rebuild', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })
    store.addToast('success', storageId ? `Rebuilding index for ${storageId.slice(0, 12)}...` : 'Rebuilding all indexes...')
  } catch {
    store.addToast('error', 'Rebuild failed')
  } finally {
    const s = new Set(rebuilding.value)
    s.delete(key)
    rebuilding.value = s
  }
}

async function rebuildAllFailed() {
  for (const idx of failedIndexes.value) {
    await rebuild(idx.storage_id)
  }
}

function statusLabel(s: string | number): { label: string; class: string } {
  const statusMap: Record<string | number, { label: string; class: string }> = {
    0: { label: 'Pending', class: 'text-slate-400' },
    1: { label: 'Queued', class: 'text-amber-400' },
    2: { label: 'Indexing', class: 'text-sky-400' },
    3: { label: 'Ready', class: 'text-emerald-400' },
    4: { label: 'Error', class: 'text-rose-400' },
    5: { label: 'Not Found', class: 'text-slate-500' },
    'ready': { label: 'Ready', class: 'text-emerald-400' },
    'indexing': { label: 'Indexing', class: 'text-sky-400' },
    'error': { label: 'Error', class: 'text-rose-400' },
  }
  return statusMap[s] ?? { label: String(s), class: 'text-slate-400' }
}
</script>

<template>
  <div>
    <PageHeader title="Search Indexer" subtitle="Manage Zoekt search indexes for repositories">
      <template #actions>
        <button
          @click="rebuild()"
          class="px-4 py-2 rounded-xl text-sm font-medium bg-violet-600/20 text-violet-300 border border-violet-500/30 hover:bg-violet-600/30 transition-all"
        >
          🔄 Rebuild All
        </button>
      </template>
    </PageHeader>

    <!-- Stat cards row 1 -->
    <div v-if="stats" class="grid grid-cols-2 sm:grid-cols-4 gap-4 mb-4">
      <StatCard icon="📚" label="Indexed Repos" :value="safeStats.total_indexes" />
      <StatCard icon="❌" label="Failed Jobs"
        :value="safeStats.jobs_failed"
        :class="safeStats.jobs_failed > 0 ? 'text-rose-400' : ''" />
      <StatCard icon="⚠️" label="Rejected (Queue Full)"
        :value="safeStats.jobs_rejected"
        :class="safeStats.jobs_rejected > 0 ? 'text-amber-400' : ''" />
      <StatCard icon="🔍" label="Searches" :value="formatNumber(safeStats.searches_total)" />
    </div>

    <!-- Stat cards row 2 -->
    <div v-if="stats" class="grid grid-cols-2 sm:grid-cols-4 gap-4 mb-6">
      <StatCard icon="📄" label="Total Files" :value="formatNumber(safeStats.total_files_indexed)" />
      <StatCard icon="💾" label="Index Size" :value="formatBytes(safeStats.total_index_size_bytes)" />
      <StatCard icon="⏳" label="Queue Length" :value="safeStats.queue_length" />
      <StatCard icon="⚙️" label="Active Jobs" :value="safeStats.active_jobs" />
    </div>

    <!-- Failed Indexes section (visible only when there are errors) -->
    <div v-if="failedIndexes.length" class="mb-6">
      <DataCard>
        <template #header>
          <div class="flex items-center justify-between">
            <div class="flex items-center gap-3">
              <h2 class="text-sm font-semibold text-rose-300">❌ Failed Repositories</h2>
              <span class="text-xs px-2 py-0.5 rounded-full bg-rose-500/15 text-rose-300 border border-rose-500/20">
                {{ failedIndexes.length }}
              </span>
            </div>
            <button
              @click="rebuildAllFailed()"
              class="text-xs px-3 py-1.5 rounded-lg bg-amber-500/15 text-amber-300 border border-amber-500/20 hover:bg-amber-500/25 transition-all"
            >🔄 Rebuild All Failed</button>
          </div>
        </template>
        <div class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="border-b border-slate-700/40 text-xs text-slate-400 uppercase tracking-wider">
                <th class="text-left px-6 py-3 font-medium">Repository</th>
                <th class="text-left px-6 py-3 font-medium">Error Message</th>
                <th class="text-left px-6 py-3 font-medium">Last Attempted</th>
                <th class="text-right px-6 py-3 font-medium">Actions</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-slate-700/20">
              <tr v-for="idx in failedIndexes" :key="idx.storage_id" class="hover:bg-slate-800/30 transition-colors">
                <td class="px-6 py-3">
                  <div class="text-xs text-slate-200 truncate max-w-[240px]">{{ idx.display_path || idx.storage_id }}</div>
                  <div class="font-mono text-xs text-slate-500 mt-0.5">{{ idx.storage_id?.slice(0, 12) }}...</div>
                </td>
                <td class="px-6 py-3 text-xs text-rose-400 max-w-[320px]">
                  <div class="truncate" :title="idx.error_message">{{ idx.error_message || 'Unknown error' }}</div>
                </td>
                <td class="px-6 py-3 text-xs text-slate-400">{{ formatDate(idx.last_indexed) }}</td>
                <td class="px-6 py-3 text-right">
                  <button
                    @click="rebuild(idx.storage_id)"
                    :disabled="rebuilding.has(idx.storage_id)"
                    class="text-xs text-violet-400 hover:text-violet-300 disabled:opacity-40 transition-colors px-2 py-1 rounded hover:bg-violet-500/10"
                  >
                    {{ rebuilding.has(idx.storage_id) ? 'Rebuilding...' : 'Rebuild' }}
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </DataCard>
    </div>

    <!-- Index list (non-failed) -->
    <DataCard :loading="idxLoading">
      <template #header>
        <h2 class="text-sm font-semibold text-slate-200">
          📊 Index Status
          <span v-if="indexes" class="text-xs font-normal text-slate-400 ml-2">{{ goodIndexes.length }} total</span>
        </h2>
      </template>
      <div v-if="goodIndexes.length" class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-slate-700/40 text-xs text-slate-400 uppercase tracking-wider">
              <th class="text-left px-6 py-3 font-medium">Repository</th>
              <th class="text-left px-6 py-3 font-medium">Status</th>
              <th class="text-right px-6 py-3 font-medium">Files</th>
              <th class="text-right px-6 py-3 font-medium">Index Size</th>
              <th class="text-right px-6 py-3 font-medium">Last Indexed</th>
              <th class="text-right px-6 py-3 font-medium">Actions</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-700/20">
            <tr v-for="idx in goodIndexes" :key="idx.storage_id" class="hover:bg-slate-800/30 transition-colors">
              <td class="px-6 py-3">
                <div class="text-xs text-slate-200 truncate max-w-[280px]">{{ idx.display_path || idx.storage_id }}</div>
                <div class="font-mono text-xs text-slate-500 mt-0.5">{{ idx.storage_id?.slice(0, 12) }}...</div>
              </td>
              <td class="px-6 py-3">
                <span :class="statusLabel(idx.status).class" class="text-xs font-medium">
                  {{ statusLabel(idx.status).label }}
                </span>
                <div v-if="idx.status === 2 && idx.progress" class="mt-1 w-20 h-1 bg-slate-700 rounded-full overflow-hidden">
                  <div class="h-full bg-amber-400 rounded-full" :style="{ width: `${idx.progress * 100}%` }"></div>
                </div>
              </td>
              <td class="px-6 py-3 text-right text-slate-300">{{ formatNumber(idx.files_count) }}</td>
              <td class="px-6 py-3 text-right text-slate-300">{{ formatBytes(idx.index_size_bytes) }}</td>
              <td class="px-6 py-3 text-right text-xs text-slate-400">{{ formatDate(idx.last_indexed) }}</td>
              <td class="px-6 py-3 text-right">
                <button
                  @click="rebuild(idx.storage_id)"
                  :disabled="rebuilding.has(idx.storage_id)"
                  class="text-xs text-violet-400 hover:text-violet-300 disabled:opacity-40 transition-colors px-2 py-1 rounded hover:bg-violet-500/10"
                >
                  {{ rebuilding.has(idx.storage_id) ? 'Rebuilding...' : 'Rebuild' }}
                </button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <div v-else-if="!idxLoading" class="py-12 text-center text-slate-400 text-sm">No search indexes</div>
    </DataCard>
  </div>
</template>
