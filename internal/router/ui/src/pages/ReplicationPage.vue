<script setup lang="ts">
import { ref, computed } from 'vue'
import { useAutoRefresh, formatNumber } from '../composables/useAutoRefresh'
import PageHeader from '../components/PageHeader.vue'
import StatCard from '../components/StatCard.vue'
import DataCard from '../components/DataCard.vue'
import NodeBadge from '../components/NodeBadge.vue'
import ProgressBar from '../components/ProgressBar.vue'
import type { RoutersData } from '../types/api'

const routers = ref<RoutersData | null>(null)

const { loading } = useAutoRefresh(async () => {
  routers.value = await fetch('/api/routers').then(r => r.json())
}, 15_000)

// Flatten + deduplicate nodes from all routers
const allNodes = computed(() => {
  if (!routers.value) return []
  const map = new Map<string, any>()
  for (const router of routers.value.routers ?? []) {
    for (const node of router.status?.nodes ?? []) {
      const key = node.id
      if (!map.has(key)) {
        map.set(key, { ...node, _routers: new Set([router.name || router.url || 'local']) })
      } else {
        const ex = map.get(key)!
        ex.healthy = ex.healthy || node.healthy
        ex.file_count = Math.max(ex.file_count || 0, node.file_count || 0)
        ex._routers.add(router.name || router.url || 'local')
        if (!ex.covered_by && node.covered_by) ex.covered_by = node.covered_by
        if (node.backing_up?.length) {
          ex.backing_up = [...new Set([...(ex.backing_up || []), ...node.backing_up])]
        }
        if (ex.status !== 'ACTIVE' && node.status) ex.status = node.status
      }
    }
  }
  return Array.from(map.values()).map(n => ({ ...n, _routers: Array.from(n._routers) }))
})

// All failovers from all routers: { routerLabel:failedNode -> backupNode }
const allFailovers = computed(() => {
  if (!routers.value) return {}
  const merged: Record<string, string> = {}
  for (const router of routers.value.routers ?? []) {
    const failovers = router.status?.failovers ?? {}
    for (const [failed, backup] of Object.entries(failovers)) {
      const rname = router.name || router.url || 'local'
      merged[`${rname}:${failed}`] = backup as string
    }
  }
  return merged
})

const failoverEntries = computed(() => Object.entries(allFailovers.value))
const syncingNodes = computed(() => allNodes.value.filter(n => n.sync_progress > 0))

// Drain mode from any router
const activeDrain = computed(() => {
  for (const r of routers.value?.routers ?? []) {
    if (r.status?.drain_mode?.active) return r.status.drain_mode
  }
  return null
})

// Replication health
const healthPercent = computed(() => {
  if (!allNodes.value.length) return 0
  return Math.round((allNodes.value.filter(n => n.healthy).length / allNodes.value.length) * 100)
})
const healthLabel = computed(() => {
  if (healthPercent.value >= 100) return { label: 'Excellent', class: 'text-emerald-400' }
  if (healthPercent.value >= 80) return { label: 'Good', class: 'text-emerald-400' }
  if (healthPercent.value >= 50) return { label: 'Degraded', class: 'text-amber-400' }
  return { label: 'Critical', class: 'text-rose-400' }
})
</script>

<template>
  <div>
    <PageHeader title="Replication Status" subtitle="Monitor data replication and failover across all routers" />

    <!-- Drain mode warning banner -->
    <div v-if="activeDrain"
      class="mb-4 flex items-center gap-3 px-5 py-4 rounded-xl bg-amber-500/10 border border-amber-500/20 text-amber-300">
      <span class="text-2xl">🚧</span>
      <div>
        <div class="font-semibold">CLUSTER IN DRAIN MODE</div>
        <div class="text-sm text-amber-400/80">
          {{ activeDrain.reason || 'planned maintenance' }} — Failover is disabled
        </div>
      </div>
    </div>

    <!-- Stat cards -->
    <div class="grid grid-cols-1 sm:grid-cols-3 gap-4 mb-6">
      <StatCard icon="🛡️" label="Active Failovers" :value="failoverEntries.length" />
      <StatCard icon="🔄" label="Syncing Nodes" :value="syncingNodes.length" />
      <StatCard icon="📊" label="Replication Factor" value="2x" />
    </div>

    <!-- Active failovers card (shown only when failovers exist) -->
    <div v-if="failoverEntries.length" class="mb-6">
      <DataCard>
        <template #header>
          <h2 class="text-sm font-semibold text-amber-300">⚠️ Active Failovers</h2>
        </template>
        <div class="divide-y divide-slate-700/20">
          <div v-for="[key, backup] in failoverEntries" :key="key"
            class="px-6 py-4 flex items-center justify-between gap-4">
            <div>
              <div class="flex items-center gap-2 text-sm font-semibold">
                <span class="text-rose-400">❌ {{ key.includes(':') ? key.split(':').slice(1).join(':') : key }}</span>
                <span class="text-slate-500">→</span>
                <span class="text-emerald-400">✅ {{ backup }}</span>
              </div>
              <div class="text-xs text-slate-400 mt-1">
                {{ backup }} is covering traffic for {{ key.includes(':') ? key.split(':').slice(1).join(':') : key }}
              </div>
              <div v-if="key.includes(':')" class="text-xs text-slate-500 mt-0.5">
                Router: {{ key.split(':')[0] }}
              </div>
            </div>
            <span class="text-xs px-2 py-0.5 rounded-full bg-amber-500/15 text-amber-300 border border-amber-500/20">Active</span>
          </div>
        </div>
      </DataCard>
    </div>

    <!-- Node replication table -->
    <DataCard :loading="loading" class="mb-6">
      <template #header>
        <h2 class="text-sm font-semibold text-slate-200">Node Replication Status</h2>
      </template>
      <div v-if="allNodes.length" class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-slate-700/40 text-xs text-slate-400 uppercase tracking-wider">
              <th class="text-left px-6 py-3 font-medium">Node</th>
              <th class="text-left px-6 py-3 font-medium">Router</th>
              <th class="text-left px-6 py-3 font-medium">Status</th>
              <th class="text-right px-6 py-3 font-medium">Files</th>
              <th class="text-left px-6 py-3 font-medium">Backup Status</th>
              <th class="text-left px-6 py-3 font-medium">Sync</th>
              <th class="text-left px-6 py-3 font-medium">Health</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-700/20">
            <tr v-for="node in allNodes" :key="node.id" class="hover:bg-slate-800/30 transition-colors">
              <td class="px-6 py-3 font-semibold text-slate-200">{{ node.id }}</td>
              <td class="px-6 py-3 text-xs text-slate-400">{{ node._routers?.join(', ') ?? '-' }}</td>
              <td class="px-6 py-3">
                <span v-if="node.status === 'STAGING'"
                  class="text-xs px-2 py-0.5 rounded-full bg-amber-500/15 text-amber-300 border border-amber-500/20">⏳ Staging</span>
                <span v-else-if="node.sync_progress > 0"
                  class="text-xs px-2 py-0.5 rounded-full bg-sky-500/15 text-sky-300 border border-sky-500/20">🔄 Syncing</span>
                <span v-else-if="node.healthy"
                  class="text-xs px-2 py-0.5 rounded-full bg-emerald-500/15 text-emerald-300 border border-emerald-500/20">✅ Active</span>
                <span v-else
                  class="text-xs px-2 py-0.5 rounded-full bg-rose-500/15 text-rose-300 border border-rose-500/20">❌ Failed</span>
              </td>
              <td class="px-6 py-3 text-right text-slate-300">{{ formatNumber(node.file_count || 0) }}</td>
              <td class="px-6 py-3 text-xs">
                <span v-if="node.covered_by" class="text-sky-400 font-medium">Covered by: {{ node.covered_by }}</span>
                <span v-else-if="node.backing_up?.length" class="text-amber-400 font-medium">Covering: {{ node.backing_up.join(', ') }}</span>
                <span v-else class="text-slate-500">-</span>
              </td>
              <td class="px-6 py-3">
                <div v-if="node.sync_progress > 0" class="min-w-[120px]">
                  <div class="text-xs text-sky-400 mb-1">{{ (node.sync_progress * 100).toFixed(0) }}%</div>
                  <ProgressBar :value="node.sync_progress * 100" />
                </div>
                <span v-else-if="node.status === 'STAGING'" class="text-xs text-slate-400">Pending</span>
                <span v-else-if="node.healthy" class="text-xs text-emerald-400">Complete</span>
                <span v-else class="text-xs text-rose-400">N/A</span>
              </td>
              <td class="px-6 py-3">
                <NodeBadge :healthy="node.healthy" />
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <div v-else class="py-12 text-center text-slate-400 text-sm">No nodes found</div>
    </DataCard>

    <!-- Bottom row: Replication Health + Data Distribution -->
    <div class="grid grid-cols-1 sm:grid-cols-2 gap-6">
      <!-- Replication Health -->
      <DataCard :loading="loading">
        <template #header>
          <h2 class="text-sm font-semibold text-slate-200">Replication Health</h2>
        </template>
        <div class="px-6 py-8 text-center">
          <div class="text-6xl font-bold mb-2" :class="healthLabel.class">
            {{ healthPercent }}%
          </div>
          <div class="text-sm font-semibold mb-2" :class="healthLabel.class">{{ healthLabel.label }}</div>
          <div class="w-full max-w-xs mx-auto mt-4">
            <ProgressBar :value="healthPercent" />
          </div>
          <div class="text-xs text-slate-400 mt-3">
            {{ allNodes.filter(n => n.healthy).length }}/{{ allNodes.length }} nodes healthy
          </div>
        </div>
      </DataCard>

      <!-- Data Distribution -->
      <DataCard :loading="loading">
        <template #header>
          <h2 class="text-sm font-semibold text-slate-200">Data Distribution</h2>
        </template>
        <div v-if="allNodes.length" class="px-6 py-4 space-y-3">
          <div v-for="node in allNodes" :key="node.id" class="space-y-1">
            <div class="flex justify-between text-xs">
              <span class="text-slate-300">{{ node.id }}</span>
              <span class="text-slate-400">{{ formatNumber(node.file_count || 0) }} files</span>
            </div>
            <ProgressBar
              :value="allNodes.reduce((s, n) => s + (n.file_count || 0), 0) > 0
                ? Math.round(((node.file_count || 0) / allNodes.reduce((s, n) => s + (n.file_count || 0), 0)) * 100)
                : 0"
            />
          </div>
        </div>
        <div v-else class="py-12 text-center text-slate-400 text-sm">No data</div>
      </DataCard>
    </div>
  </div>
</template>
