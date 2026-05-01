<script setup lang="ts">
import { ref, computed } from 'vue'
import { useAppStore } from '../stores/app'
import { useAutoRefresh, formatBytes, formatNumber } from '../composables/useAutoRefresh'
import PageHeader from '../components/PageHeader.vue'
import StatCard from '../components/StatCard.vue'
import DataCard from '../components/DataCard.vue'
import NodeBadge from '../components/NodeBadge.vue'
import ProgressBar from '../components/ProgressBar.vue'
import type { StatusData, NodeStatus } from '../types/api'

const store = useAppStore()
const status = ref<StatusData | null>(null)

async function load() {
  const data: StatusData = await fetch('/api/status').then((r) => r.json())
  status.value = data
  if (data.version?.version) store.setVersion(data.version.version)
}

const { loading } = useAutoRefresh(load, 5_000)

const healthy = (s: StatusData) => s.nodes.filter((n) => n.healthy).length

// KVS helpers
function kvsStatusLabel(kvs?: NodeStatus['kvs']): { label: string; class: string } {
  if (!kvs || !kvs.enabled) return { label: 'Disabled', class: 'text-slate-500' }
  if (kvs.mode === 'local') return { label: 'Local', class: 'text-emerald-400' }
  if (!kvs.healthy) return { label: 'Raft starting', class: 'text-amber-400' }
  if (kvs.role === 'leader') return { label: 'Raft leader', class: 'text-emerald-400' }
  if (kvs.role === 'follower') return { label: 'Raft follower', class: 'text-sky-400' }
  return { label: `Raft ${kvs.role || 'unknown'}`, class: 'text-slate-400' }
}

function kvsStatusDetails(kvs?: NodeStatus['kvs']): string {
  if (!kvs || !kvs.enabled) return 'Embedded KVS disabled'
  const keyCount = (kvs.key_count || 0).toLocaleString()
  if (kvs.mode === 'local') return `Single-node local store · ${keyCount} keys`
  const parts: string[] = []
  if (kvs.leader_id) parts.push(`Leader ${kvs.leader_id}`)
  if (kvs.peer_count) parts.push(`${kvs.peer_count} peers`)
  parts.push(`${keyCount} keys`)
  return parts.join(' · ')
}

function diskPercent(node: NodeStatus): number {
  if (!node.disk_total) return 0
  return Math.round((node.disk_used / node.disk_total) * 100)
}

function diskFree(node: NodeStatus): number {
  if (typeof node.disk_free === 'number') return node.disk_free
  return Math.max(node.disk_total - node.disk_used, 0)
}

// Failovers: Record<string, string> -> array for iteration
const failoverEntries = computed(() => {
  if (!status.value?.failovers) return []
  return Object.entries(status.value.failovers)
})
</script>

<template>
  <div>
    <PageHeader title="MetaStores" subtitle="Backend storage node health and disk usage — refreshes every 5s" />

    <!-- Drain mode banner -->
    <div v-if="status?.drain_mode?.active"
      class="mb-4 flex items-center gap-3 px-5 py-4 rounded-xl bg-amber-500/10 border border-amber-500/20 text-amber-300">
      <span class="text-2xl">🚧</span>
      <div>
        <div class="font-semibold text-amber-300">CLUSTER IN DRAIN MODE</div>
        <div class="text-sm text-amber-400/80">
          {{ status.drain_mode.reason || 'planned maintenance' }} — Failover is disabled
        </div>
      </div>
    </div>

    <div v-if="status" class="grid grid-cols-2 sm:grid-cols-4 gap-4 mb-6">
      <StatCard icon="🌐" label="Total Nodes" :value="status.nodes.length" />
      <StatCard icon="✅" label="Healthy" :value="healthy(status)" />
      <StatCard icon="⚠️" label="Unhealthy" :value="status.nodes.length - healthy(status)" />
      <StatCard icon="📁" label="Total Files" :value="formatNumber(status.nodes.reduce((s, n) => s + n.file_count, 0))" />
    </div>

    <DataCard :loading="loading">
      <template #header>
        <h2 class="text-base font-semibold text-slate-200">Storage Nodes</h2>
      </template>

      <div v-if="status" class="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-4 xl:grid-cols-5 gap-2 p-3">
        <div v-for="node in status.nodes" :key="node.id"
          class="rounded-lg border px-3 py-2.5 flex flex-col gap-1.5 text-xs transition-colors"
          :class="node.healthy ? 'bg-slate-800/40 border-slate-700/40' : 'bg-rose-950/20 border-rose-700/30'">

          <!-- Top: status + name -->
          <div class="flex items-center gap-1.5">
            <NodeBadge :healthy="node.healthy" />
            <span class="font-semibold text-slate-200 truncate leading-tight">{{ node.id }}</span>
          </div>

          <!-- Address + status -->
          <div class="flex items-center justify-between gap-1">
            <span class="text-slate-500 font-mono truncate">{{ node.address }}</span>
            <span class="shrink-0 text-slate-400">{{ node.status }}</span>
          </div>

          <!-- Files / Weight / Keys -->
          <div class="flex items-center justify-between gap-1">
            <span class="text-slate-500">Files</span>
            <span class="text-slate-300 font-medium">{{ formatNumber(node.file_count) }}</span>
          </div>
          <div class="flex items-center justify-between gap-1">
            <span class="text-slate-500">Weight</span>
            <span class="text-slate-300">{{ node.weight }}</span>
          </div>
          <div class="flex items-center justify-between gap-1">
            <span class="text-slate-500">KV Keys</span>
            <span class="text-slate-300">{{ formatNumber(node.kvs?.key_count ?? 0) }}</span>
          </div>

          <!-- KVS role -->
          <div class="flex items-center justify-between gap-1">
            <span class="text-slate-500">KVS</span>
            <span :class="kvsStatusLabel(node.kvs).class" class="font-medium truncate">{{ kvsStatusLabel(node.kvs).label }}</span>
          </div>
          <div class="text-slate-500 truncate leading-tight">{{ kvsStatusDetails(node.kvs) }}</div>

          <!-- Disk -->
          <div v-if="node.disk_total">
            <div class="flex items-center justify-between gap-1 mb-0.5">
              <span class="text-slate-500">Disk</span>
              <span class="text-slate-300">{{ formatBytes(node.disk_used) }} / {{ formatBytes(diskFree(node)) }}
                <span class="text-slate-500">({{ diskPercent(node) }}%)</span>
              </span>
            </div>
            <ProgressBar :value="diskPercent(node)" />
          </div>

          <!-- Sync progress -->
          <div v-if="node.sync_progress > 0">
            <div class="flex items-center justify-between gap-1 mb-0.5">
              <span class="text-sky-400">Syncing</span>
              <span class="text-sky-400">{{ (node.sync_progress * 100).toFixed(0) }}%</span>
            </div>
            <ProgressBar :value="node.sync_progress * 100" />
          </div>

          <!-- Badges -->
          <div v-if="node.backing_up?.length || node.covered_by" class="flex flex-wrap gap-1 pt-0.5">
            <span v-if="node.backing_up?.length" class="px-1 py-0.5 rounded bg-amber-500/10 text-amber-400 border border-amber-500/20">🛡️ {{ node.backing_up.join(', ') }}</span>
            <span v-if="node.covered_by" class="px-1 py-0.5 rounded bg-sky-500/10 text-sky-400 border border-sky-500/20">covered: {{ node.covered_by }}</span>
          </div>
        </div>
      </div>

      <!-- Failovers section -->
      <div v-if="failoverEntries.length" class="border-t border-amber-500/20 bg-amber-500/5 px-6 py-5">
        <h3 class="text-sm font-semibold text-amber-300 mb-3">⚠️ Active Failovers</h3>
        <div class="space-y-2">
          <div v-for="[failed, backup] in failoverEntries" :key="failed"
            class="flex items-center gap-3 text-sm">
            <span class="text-rose-400 font-medium">❌ {{ failed }}</span>
            <span class="text-slate-500">→</span>
            <span class="text-emerald-400 font-medium">✅ {{ backup }}</span>
            <span class="text-xs text-slate-500 ml-2">{{ backup }} is covering traffic for {{ failed }}</span>
          </div>
        </div>
      </div>
    </DataCard>

  </div>
</template>
