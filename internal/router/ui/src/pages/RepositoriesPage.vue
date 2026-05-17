<script setup lang="ts">
import { ref, computed } from 'vue'
import { useAppStore } from '../stores/app'
import { useAutoRefresh, formatNumber } from '../composables/useAutoRefresh'
import PageHeader from '../components/PageHeader.vue'
import StatCard from '../components/StatCard.vue'
import DataCard from '../components/DataCard.vue'
import type { RepositoriesData, Repository } from '../types/api'

const store = useAppStore()
const data = ref<RepositoriesData | null>(null)
const searchQuery = ref('')
const expandedRows = ref<Set<string>>(new Set())
const rebalancing = ref<Set<string>>(new Set())

const { loading } = useAutoRefresh(async () => {
  const controller = new AbortController()
  const timeout = setTimeout(() => controller.abort(), 30_000)
  try {
    const res = await fetch('/api/repositories', { signal: controller.signal })
    const d = await res.json()
    data.value = d
  } finally {
    clearTimeout(timeout)
  }
}, 15_000)

function timeAgoUnix(ts: number): string {
  if (!ts) return '-'
  const diff = Math.floor(Date.now() / 1000) - ts
  if (diff < 60) return 'just now'
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`
  if (diff < 604800) return `${Math.floor(diff / 86400)}d ago`
  return new Date(ts * 1000).toLocaleDateString()
}

function filteredRepos(): Repository[] {
  const repos = data.value?.repositories ?? []
  if (!searchQuery.value) return repos
  const q = searchQuery.value.toLowerCase()
  return repos.filter(r =>
    r.repo_url?.toLowerCase().includes(q) ||
    r.repo_id?.toLowerCase().includes(q) ||
    r.branch?.toLowerCase().includes(q) ||
    r.storage_id?.toLowerCase().includes(q)
  )
}

function toggleRow(storageId: string) {
  const next = new Set(expandedRows.value)
  if (next.has(storageId)) next.delete(storageId)
  else next.add(storageId)
  expandedRows.value = next
}

async function triggerRebalance(storageId: string) {
  if (!confirm('Trigger rebalancing for this repository?')) return
  rebalancing.value = new Set([...rebalancing.value, storageId])
  try {
    const body = new URLSearchParams({ storage_id: storageId })
    const res = await fetch('/api/rebalance', {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body,
    })
    const result = await res.json()
    if (result.success) {
      store.addToast('success', 'Rebalancing started!')
    } else {
      store.addToast('error', `Failed to start rebalancing: ${result.message}`)
    }
  } catch (err: any) {
    store.addToast('error', `Error: ${err.message}`)
  } finally {
    const s = new Set(rebalancing.value)
    s.delete(storageId)
    rebalancing.value = s
  }
}

function statusBadgeClass(repo: Repository): string {
  if (repo.in_progress && repo.stage === 'FAILED') return 'bg-rose-500/15 text-rose-300 border-rose-500/20'
  if (repo.in_progress) return 'bg-amber-500/15 text-amber-300 border-amber-500/20'
  if (repo.rebalance_state === 'Stable') return 'bg-emerald-500/15 text-emerald-300 border-emerald-500/20'
  if (repo.rebalance_state === 'Rebalancing') return 'bg-amber-500/15 text-amber-300 border-amber-500/20'
  if (repo.rebalance_state === 'DualActive') return 'bg-sky-500/15 text-sky-300 border-sky-500/20'
  if (repo.rebalance_state === 'Ingesting') return 'bg-amber-500/15 text-amber-300 border-amber-500/20'
  return 'bg-slate-700/30 text-slate-400 border-slate-600/20'
}

function statusLabel(repo: Repository): string {
  if (repo.in_progress) {
    const stage = repo.stage || 'CLONING'
    const pct = repo.rebalance_progress ? `${Math.round(repo.rebalance_progress * 100)}%` : ''
    return `${stage}${pct ? ' ' + pct : ''}`
  }
  if (repo.rebalance_state === 'Stable') return '✓ Stable'
  if (repo.rebalance_state === 'Rebalancing') return `⟳ ${Math.round((repo.rebalance_progress || 0) * 100)}%`
  if (repo.rebalance_state === 'DualActive') return `↔ ${Math.round((repo.rebalance_progress || 0) * 100)}%`
  if (repo.rebalance_state === 'Ingesting') return '⟳ Ingesting'
  return repo.rebalance_state || 'Unknown'
}

const inProgressRepos = computed(() => (data.value?.repositories ?? []).filter(r => r.in_progress))
</script>

<template>
  <div>
    <PageHeader title="Repositories" subtitle="Browse ingested repositories across all routers" />

    <div class="grid grid-cols-2 sm:grid-cols-4 gap-4 mb-6">
      <StatCard icon="📦" label="Total Repos" :value="data?.repositories?.filter(r => !r.in_progress)?.length ?? '-'" />
      <StatCard icon="🔄" label="In Progress" :value="inProgressRepos.length" />
      <StatCard icon="📄" label="Total Files"
        :value="formatNumber(data?.repositories?.reduce((s, r) => s + (r.files_count || 0), 0) ?? 0)" />
      <StatCard icon="🗂️" label="Topology Version" :value="data?.current_topology_version ?? '-'" />
    </div>

    <!-- In-progress ingestions -->
    <div v-if="inProgressRepos.length" class="mb-6">
      <DataCard>
        <template #header>
          <h2 class="text-sm font-semibold text-slate-200">Ingesting Now</h2>
        </template>
        <div class="divide-y divide-slate-700/20">
          <div v-for="ip in inProgressRepos" :key="ip.storage_id" class="flex items-center gap-4 px-6 py-4">
            <div class="flex-1 min-w-0">
              <div class="text-sm font-medium text-slate-200 truncate">{{ ip.repo_url || ip.repo_id }}</div>
              <div class="text-xs text-slate-400 mt-0.5">ref: {{ ip.branch || 'main' }}</div>
            </div>
            <div v-if="ip.message" class="text-xs text-slate-500 truncate max-w-[200px]">{{ ip.message }}</div>
            <div class="text-xs text-slate-500">Started {{ timeAgoUnix(ip.ingested_at) }}</div>
            <div class="flex items-center gap-2 text-xs text-amber-400">
              <svg class="animate-spin w-3.5 h-3.5" viewBox="0 0 24 24" fill="none">
                <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"/>
                <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z"/>
              </svg>
              {{ ip.stage || 'In progress...' }}
            </div>
          </div>
        </div>
      </DataCard>
    </div>

    <!-- Repositories table -->
    <DataCard :loading="loading">
      <template #header>
        <div class="flex items-center justify-between gap-4 flex-wrap">
          <h2 class="text-sm font-semibold text-slate-200">
            All Repositories
            <span v-if="data" class="font-normal text-slate-400 ml-1">
              ({{ filteredRepos().length }} of {{ data.repositories?.length ?? 0 }})
            </span>
          </h2>
          <input
            v-model="searchQuery"
            type="text"
            placeholder="Filter by repo, URL, branch, or storage ID..."
            class="bg-slate-800/60 border border-slate-700/40 rounded-lg px-3 py-2 text-sm text-slate-200 placeholder-slate-500 focus:outline-none focus:border-violet-500/60 w-72"
          />
        </div>
      </template>
      <div v-if="filteredRepos().length" class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-slate-700/40 text-xs text-slate-400 uppercase tracking-wider">
              <th class="text-left px-6 py-3 font-medium">Repository ID</th>
              <th class="text-left px-6 py-3 font-medium">Source</th>
              <th class="text-left px-6 py-3 font-medium">Status</th>
              <th class="text-right px-6 py-3 font-medium">Files</th>
              <th class="text-left px-6 py-3 font-medium">Ingested At</th>
            </tr>
          </thead>
          <tbody>
            <template v-for="repo in filteredRepos()" :key="repo.storage_id">
              <!-- Main row - clickable to expand -->
              <tr
                class="border-b border-slate-700/20 hover:bg-slate-800/30 transition-colors cursor-pointer"
                :class="expandedRows.has(repo.storage_id) ? 'bg-slate-800/20' : ''"
                @click="toggleRow(repo.storage_id)"
                title="Click for details"
              >
                <td class="px-6 py-3">
                  <div class="flex items-center gap-2">
                    <span class="text-slate-400 text-xs">{{ expandedRows.has(repo.storage_id) ? '▼' : '▶' }}</span>
                    <div>
                      <div class="text-slate-200 text-xs font-medium truncate max-w-[220px]">{{ repo.repo_id }}</div>
                      <span v-if="repo.product_kind" class="text-xs px-1 py-0.5 rounded bg-sky-500/15 text-sky-300 border border-sky-500/20">
                        {{ repo.product_kind }}
                      </span>
                    </div>
                  </div>
                </td>
                <td class="px-6 py-3 text-xs text-slate-400">
                  <a
                    v-if="repo.product_ui_url"
                    :href="repo.product_ui_url"
                    target="_blank"
                    class="text-violet-400 hover:text-violet-300"
                    @click.stop
                  >
                    {{ repo.product_ui_label || repo.product_ui_url }}
                  </a>
                  <a
                    v-else-if="repo.repo_url && repo.repo_url !== 'dir-hint' && (repo.repo_url.startsWith('http://') || repo.repo_url.startsWith('https://'))"
                    :href="repo.repo_url"
                    target="_blank"
                    class="text-violet-400 hover:text-violet-300 truncate max-w-[220px] block"
                    @click.stop
                  >
                    {{ repo.repo_url.length > 50 ? repo.repo_url.slice(0, 47) + '...' : repo.repo_url }}
                  </a>
                  <a v-else-if="repo.repo_url === 'dir-hint'" href="/dependencies" class="text-violet-400 hover:text-violet-300" @click.stop>
                    📦 Dependencies
                  </a>
                  <span v-else class="truncate max-w-[220px] block">{{ repo.repo_url || repo.repo_id }}</span>
                </td>
                <td class="px-6 py-3">
                  <span class="text-xs px-2 py-0.5 rounded-full border" :class="statusBadgeClass(repo)">
                    {{ statusLabel(repo) }}
                  </span>
                </td>
                <td class="px-6 py-3 text-right text-slate-300">
                  {{ repo.in_progress && repo.total_files
                    ? `${formatNumber(repo.files_count)} / ${formatNumber(repo.total_files)}`
                    : formatNumber(repo.files_count) }}
                </td>
                <td class="px-6 py-3 text-xs text-slate-500">
                  {{ repo.in_progress ? 'In Progress...' : timeAgoUnix(repo.ingested_at) }}
                </td>
              </tr>

              <!-- Expanded details row -->
              <tr v-if="expandedRows.has(repo.storage_id)" :key="`${repo.storage_id}-details`"
                class="border-b border-slate-700/20">
                <td colspan="5" class="px-8 py-4 bg-slate-800/30 border-l-2 border-violet-500/40">
                  <div class="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-5 gap-4 text-xs mb-4">
                    <div>
                      <div class="text-slate-500 uppercase tracking-wider mb-1">REF / VERSION</div>
                      <span class="px-2 py-0.5 rounded bg-sky-500/15 text-sky-300 border border-sky-500/20 text-xs">
                        {{ repo.branch || 'N/A' }}
                      </span>
                    </div>
                    <div v-if="repo.commit_hash">
                      <div class="text-slate-500 uppercase tracking-wider mb-1">COMMIT</div>
                      <div class="font-mono text-slate-300" :title="repo.commit_message || ''">
                        {{ repo.commit_hash.slice(0, 7) }}
                      </div>
                      <div v-if="repo.commit_time" class="text-slate-500 mt-0.5">
                        {{ timeAgoUnix(repo.commit_time) }}
                      </div>
                    </div>
                    <div>
                      <div class="text-slate-500 uppercase tracking-wider mb-1">TOPOLOGY</div>
                      <div class="text-slate-300">V{{ repo.topology_version ?? '-' }}</div>
                    </div>
                    <div>
                      <div class="text-slate-500 uppercase tracking-wider mb-1">STORAGE ID</div>
                      <div class="font-mono text-slate-400 break-all" :title="repo.storage_id">
                        {{ repo.storage_id ? repo.storage_id.slice(0, 12) + '...' : 'N/A' }}
                      </div>
                    </div>
                    <div v-if="repo.product_ui_url">
                      <div class="text-slate-500 uppercase tracking-wider mb-1">PRODUCT UI</div>
                      <a :href="repo.product_ui_url" target="_blank"
                        class="text-violet-400 hover:text-violet-300">
                        {{ repo.product_ui_label || 'Open →' }}
                      </a>
                    </div>
                  </div>
                  <div class="flex items-center gap-3">
                    <button
                      v-if="!repo.in_progress && repo.rebalance_state !== 'Rebalancing' && repo.rebalance_state !== 'DualActive'"
                      :disabled="rebalancing.has(repo.storage_id)"
                      @click.stop="triggerRebalance(repo.storage_id)"
                      class="text-xs px-3 py-1.5 rounded-lg bg-violet-600/20 text-violet-300 border border-violet-500/30 hover:bg-violet-600/30 disabled:opacity-40 transition-all"
                    >
                      {{ rebalancing.has(repo.storage_id) ? '⟳ Starting...' : '🔄 Trigger Rebalance' }}
                    </button>
                    <span v-else-if="repo.rebalance_state === 'Rebalancing' || repo.rebalance_state === 'DualActive'"
                      class="text-xs text-slate-400">⏳ Rebalancing in progress</span>
                    <button
                      @click.stop="toggleRow(repo.storage_id)"
                      class="text-xs px-3 py-1.5 rounded-lg bg-slate-700/40 text-slate-400 hover:bg-slate-700/60 transition-all"
                    >✕ Close</button>
                  </div>
                </td>
              </tr>
            </template>
          </tbody>
        </table>
      </div>
      <div v-else class="py-12 text-center text-slate-400 text-sm">
        {{ searchQuery ? 'No repositories match your search' : 'No repositories ingested yet' }}
      </div>
    </DataCard>
  </div>
</template>
