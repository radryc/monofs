<script setup lang="ts">
import { ref, computed } from 'vue'
import { useAppStore } from '../stores/app'
import { useAutoRefresh, formatBytes, formatNumber } from '../composables/useAutoRefresh'
import PageHeader from '../components/PageHeader.vue'
import DataCard from '../components/DataCard.vue'
import StatCard from '../components/StatCard.vue'
import NodeBadge from '../components/NodeBadge.vue'
import ProgressBar from '../components/ProgressBar.vue'
import type { RoutersData, FetcherClusterStats, SearchStatsData, ListClientsResponse, RepositoriesData } from '../types/api'

const store = useAppStore()

const routers = ref<RoutersData | null>(null)
const fetcherStats = ref<FetcherClusterStats | null>(null)
const searchStats = ref<SearchStatsData | null>(null)
const clients = ref<ListClientsResponse | null>(null)
const recentRepos = ref<RepositoriesData | null>(null)

// Deduplicate nodes across multiple routers by node.id
const dedupedNodes = computed(() => {
  const map = new Map<string, any>()
  for (const router of routers.value?.routers ?? []) {
    const rname = router.name || router.url || 'local'
    for (const node of router.status?.nodes ?? []) {
      const key = node.id || node.address
      if (!map.has(key)) {
        map.set(key, { ...node, _routers: [rname] })
      } else {
        const ex = map.get(key)!
        if (!ex._routers.includes(rname)) ex._routers.push(rname)
        // Prefer healthy node data
        if (node.healthy && !ex.healthy) {
          const savedRouters = ex._routers
          Object.assign(ex, node)
          ex._routers = savedRouters
        }
        // Take max file count
        if ((node.file_count || 0) > (ex.file_count || 0)) ex.file_count = node.file_count
        // Merge backing_up arrays
        if (node.backing_up?.length) {
          ex.backing_up = [...new Set([...(ex.backing_up || []), ...node.backing_up])]
        }
      }
    }
  }
  return Array.from(map.values())
})

const totalNodes = computed(() => dedupedNodes.value.length)
const healthyNodes = computed(() => dedupedNodes.value.filter((n) => n.healthy).length)
const totalFiles = computed(() => dedupedNodes.value.reduce((s: number, n: any) => s + (n.file_count || 0), 0))
const totalRepos = ref(0)

function diskFree(node: { disk_free?: number; disk_total?: number; disk_used?: number }) {
  if (typeof node.disk_free === 'number') return node.disk_free
  return Math.max((node.disk_total || 0) - (node.disk_used || 0), 0)
}

async function load() {
  const [routersRes, fetchersRes, searchRes, clientsRes, reposRes] = await Promise.allSettled([
    fetch('/api/routers').then((r) => r.json()),
    fetch('/api/fetchers').then((r) => r.json()),
    fetch('/api/search/stats').then((r) => r.json()),
    fetch('/api/clients').then((r) => r.json()),
    fetch('/api/repositories').then((r) => r.json()),
  ])

  if (routersRes.status === 'fulfilled') {
    routers.value = routersRes.value as RoutersData
    const r = routersRes.value as RoutersData
    totalRepos.value = 0
    // Deduplicate repos by storage_id across all routers
    const seenRepos = new Set<string>()
    for (const router of r.routers ?? []) {
      for (const repo of router.repositories?.repositories ?? []) {
        seenRepos.add(repo.storage_id)
      }
      if (!store.version && router.status?.version?.version) store.setVersion(router.status.version.version)
    }
    totalRepos.value = seenRepos.size
  }

  if (fetchersRes.status === 'fulfilled') fetcherStats.value = fetchersRes.value
  if (searchRes.status === 'fulfilled') searchStats.value = searchRes.value
  if (clientsRes.status === 'fulfilled') clients.value = clientsRes.value
  if (reposRes.status === 'fulfilled') recentRepos.value = reposRes.value
}

const { loading } = useAutoRefresh(load, 15_000)

function recentActivity() {
  const repos = (recentRepos.value?.repositories ?? [])
    .filter(r => !r.in_progress)
  // Sort by ingested_at descending, take last 5
  return [...repos]
    .sort((a, b) => (b.ingested_at || 0) - (a.ingested_at || 0))
    .slice(0, 5)
}

function timeAgoUnix(ts: number): string {
  if (!ts) return '-'
  const diff = Math.floor(Date.now() / 1000) - ts
  if (diff < 60) return 'just now'
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`
  if (diff < 604800) return `${Math.floor(diff / 86400)}d ago`
  return new Date(ts * 1000).toLocaleDateString()
}
</script>

<template>
  <div>
    <PageHeader title="Dashboard" subtitle="Overview across all routers in your MonoFS deployment" />

    <!-- Stat cards -->
    <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4 gap-4 mb-6">
      <StatCard v-for="stat in [
        { icon: '🧭', label: 'Routers',          value: routers?.routers?.length ?? '-' },
        { icon: '🌐', label: 'MetaStore Nodes',   value: loading ? '…' : totalNodes },
        { icon: '✅', label: 'Healthy Nodes',     value: loading ? '…' : healthyNodes },
        { icon: '📦', label: 'Repositories',      value: loading ? '…' : totalRepos },
        { icon: '💻', label: 'Clients',           value: clients?.clients?.filter(c => c.state === 1)?.length ?? '-' },
        { icon: '📄', label: 'Total Files',       value: formatNumber(totalFiles) },
        { icon: '🔄', label: 'Fetchers',          value: fetcherStats ? `${fetcherStats.healthy_fetchers}/${fetcherStats.total_fetchers}` : '-' },
        { icon: '💾', label: 'Cache Hit Rate',    value: fetcherStats ? `${((fetcherStats.aggregated_hit_rate || 0) * 100).toFixed(1)}%` : '-' },
        { icon: '🔍', label: 'Search Indexes',    value: searchStats?.total_indexes ?? '-' },
        { icon: '📊', label: 'Total Searches',    value: searchStats ? formatNumber(searchStats.searches_total) : '-' },
        { icon: '⚡', label: 'Avg Search',        value: searchStats ? `${searchStats.avg_search_duration_ms?.toFixed(0) ?? 0}ms` : '-' },
        { icon: '📥', label: 'Data Fetched',      value: fetcherStats ? formatBytes(fetcherStats.total_bytes_fetched || 0) : '-' },
      ]" :key="stat.label"
        :icon="stat.icon"
        :label="stat.label"
        :value="stat.value" />
    </div>

    <!-- Routers overview -->
    <DataCard :loading="loading" class="mb-6">
      <template #header>
        <div class="flex items-center justify-between">
          <div>
            <h2 class="text-base font-semibold text-slate-200">Routers Overview</h2>
            <p class="text-xs text-slate-400 mt-0.5">Status and health by router — refreshes every 15s</p>
          </div>
        </div>
      </template>

      <div v-if="routers" class="divide-y divide-slate-700/30">
        <div v-for="r in routers.routers" :key="r.url || r.name"
          class="px-6 py-4 flex items-center justify-between gap-4 flex-wrap">
          <div class="flex items-center gap-3">
            <NodeBadge :healthy="(r.status?.nodes?.filter(n => n.healthy).length ?? 0) === (r.status?.nodes?.length ?? 0) && !r.error" />
            <div>
              <div class="text-sm font-semibold text-slate-200">{{ r.name || r.url || 'local' }}</div>
              <div class="text-xs text-slate-500">v{{ r.status?.version?.version || '?' }}</div>
            </div>
            <span v-if="r.status?.drain_mode?.active"
              class="text-xs px-2 py-0.5 rounded-full bg-amber-500/15 text-amber-300 border border-amber-500/20">🚧 Drain</span>
            <span v-if="r.error"
              class="text-xs px-2 py-0.5 rounded-full bg-rose-500/15 text-rose-300 border border-rose-500/20">{{ r.error }}</span>
          </div>
          <div class="flex items-center gap-4 text-xs text-slate-400">
            <span>{{ r.status?.nodes?.length ?? 0 }} nodes</span>
            <span>{{ r.repositories?.repositories?.length ?? 0 }} repos</span>
            <span v-if="r.repositories?.repositories?.filter(rp => rp.in_progress)?.length">
              {{ r.repositories.repositories.filter(rp => rp.in_progress).length }} ingesting
            </span>
          </div>
        </div>
      </div>
    </DataCard>

    <!-- MetaStore Nodes — deduplicated across all routers -->
    <DataCard :loading="loading" class="mb-6">
      <template #header>
        <div class="flex items-center justify-between">
          <div>
            <h2 class="text-base font-semibold text-slate-200">MetaStore Nodes</h2>
            <p class="text-xs text-slate-400 mt-0.5">Deduplicated — {{ dedupedNodes.length }} unique node{{ dedupedNodes.length !== 1 ? 's' : '' }} ({{ healthyNodes }} healthy)</p>
          </div>
        </div>
      </template>
      <div v-if="dedupedNodes.length" class="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-4 xl:grid-cols-5 2xl:grid-cols-6 gap-2 p-3">
        <div v-for="node in dedupedNodes" :key="node.id"
          class="rounded-lg border px-3 py-2.5 flex flex-col gap-1.5 transition-colors text-xs"
          :class="node.healthy ? 'bg-slate-800/40 border-slate-700/40' : 'bg-rose-950/20 border-rose-700/30'">

          <!-- Top: status + node name -->
          <div class="flex items-center gap-1.5">
            <NodeBadge :healthy="node.healthy" />
            <span class="font-semibold text-slate-200 truncate leading-tight">{{ node.id }}</span>
          </div>

          <!-- Metrics: single row per field -->
          <div class="flex items-center justify-between gap-1">
            <span class="text-slate-500">Files</span>
            <span class="text-slate-300 font-medium">{{ formatNumber(node.file_count) }}</span>
          </div>

          <div>
            <div class="flex items-center justify-between gap-1 mb-0.5">
              <span class="text-slate-500">Disk</span>
              <span v-if="node.disk_total" class="text-slate-300">
                {{ formatBytes(node.disk_used) }} / {{ formatBytes(diskFree(node)) }}
                <span class="text-slate-500">({{ Math.round((node.disk_used / node.disk_total) * 100) }}%)</span>
              </span>
              <span v-else class="text-slate-500">-</span>
            </div>
            <ProgressBar v-if="node.disk_total" :value="Math.round((node.disk_used / node.disk_total) * 100)" />
          </div>

          <div class="flex items-center justify-between gap-1">
            <span class="text-slate-500">KVS</span>
            <span v-if="node.kvs?.healthy" class="text-emerald-400">✓ {{ node.kvs?.mode || 'OK' }}</span>
            <span v-else-if="node.kvs?.enabled === false" class="text-slate-500">disabled</span>
            <span v-else-if="node.kvs" class="text-rose-400">⚠ err</span>
            <span v-else class="text-slate-500">-</span>
          </div>

          <div v-if="node._routers?.length > 1" class="flex items-center justify-between gap-1">
            <span class="text-slate-500">Via</span>
            <span class="text-slate-400 truncate">{{ node._routers.join(', ') }}</span>
          </div>

          <!-- State badges -->
          <div v-if="node.backing_up?.length || node.sync_progress > 0 || node.covered_by" class="flex flex-wrap gap-1 pt-0.5">
            <span v-if="node.backing_up?.length" class="px-1 py-0.5 rounded bg-amber-500/10 text-amber-400 border border-amber-500/20">⟳ backup</span>
            <span v-if="node.sync_progress > 0" class="px-1 py-0.5 rounded bg-sky-500/10 text-sky-400 border border-sky-500/20">⟳ {{ (node.sync_progress * 100).toFixed(0) }}%</span>
            <span v-if="node.covered_by" class="px-1 py-0.5 rounded bg-slate-700/40 text-slate-400 border border-slate-600/30">covered</span>
          </div>
        </div>
      </div>
      <div v-else class="py-8 text-center text-slate-400 text-sm">No nodes found</div>
    </DataCard>

    <!-- Bottom row: Cluster health + Fetcher cluster + Search + Recent Activity -->
    <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-6">
      <!-- Cluster Health -->
      <DataCard :loading="loading">
        <template #header>
          <h2 class="text-sm font-semibold text-slate-200">Cluster Health</h2>
        </template>
        <div class="px-6 py-5 text-center">
          <div class="text-5xl font-bold mb-1"
            :class="totalNodes > 0 && healthyNodes === totalNodes ? 'text-emerald-400' : 'text-amber-400'">
            {{ totalNodes > 0 ? Math.round((healthyNodes / totalNodes) * 100) : 0 }}%
          </div>
          <div class="text-xs text-slate-400">{{ healthyNodes }}/{{ totalNodes }} nodes healthy</div>
        </div>
      </DataCard>

      <!-- Fetcher Cluster -->
      <DataCard :loading="loading">
        <template #header>
          <h2 class="text-sm font-semibold text-slate-200">Fetcher Cluster</h2>
        </template>
        <div class="px-6 py-5 space-y-2.5 text-xs">
          <div class="flex justify-between">
            <span class="text-slate-500">Healthy</span>
            <span class="text-slate-300 font-medium tabular-nums">{{ fetcherStats?.healthy_fetchers ?? '-' }}/{{ fetcherStats?.total_fetchers ?? '-' }}</span>
          </div>
          <div class="flex justify-between">
            <span class="text-slate-500">Cache Hit</span>
            <span class="text-slate-300 font-medium tabular-nums">{{ fetcherStats ? `${((fetcherStats.aggregated_hit_rate || 0) * 100).toFixed(1)}%` : '-' }}</span>
          </div>
          <div class="flex justify-between">
            <span class="text-slate-500">Data Served</span>
            <span class="text-slate-300 font-medium tabular-nums">{{ fetcherStats ? formatBytes(fetcherStats.total_bytes_served) : '-' }}</span>
          </div>
        </div>
      </DataCard>

      <!-- Search Engine -->
      <DataCard :loading="loading">
        <template #header>
          <h2 class="text-sm font-semibold text-slate-200">Search Engine</h2>
        </template>
        <div class="px-6 py-5 space-y-2.5 text-xs">
          <div class="flex justify-between">
            <span class="text-slate-500">Indexes</span>
            <span class="text-slate-300 font-medium tabular-nums">{{ searchStats?.total_indexes ?? '-' }}</span>
          </div>
          <div class="flex justify-between">
            <span class="text-slate-500">Searches</span>
            <span class="text-slate-300 font-medium tabular-nums">{{ searchStats ? formatNumber(searchStats.searches_total) : '-' }}</span>
          </div>
          <div class="flex justify-between">
            <span class="text-slate-500">Avg Time</span>
            <span class="text-slate-300 font-medium tabular-nums">{{ searchStats ? `${searchStats.avg_search_duration_ms?.toFixed(0) ?? 0}ms` : '-' }}</span>
          </div>
        </div>
      </DataCard>

      <!-- Recent Activity -->
      <DataCard :loading="loading">
        <template #header>
          <h2 class="text-sm font-semibold text-slate-200">Recent Activity</h2>
        </template>
        <div class="divide-y divide-slate-700/20">
          <div v-if="recentActivity().length" v-for="repo in recentActivity()" :key="repo.storage_id"
            class="px-4 py-3 text-xs">
            <div class="text-slate-300 truncate">{{ repo.repo_url || repo.repo_id }}</div>
            <div class="flex justify-between mt-1">
              <span class="text-slate-500">{{ formatNumber(repo.files_count) }} files</span>
              <span class="text-slate-500">{{ timeAgoUnix(repo.ingested_at) }}</span>
            </div>
          </div>
          <div v-else class="px-4 py-6 text-center text-xs text-slate-500">No recent activity</div>
        </div>
      </DataCard>
    </div>
  </div>
</template>
