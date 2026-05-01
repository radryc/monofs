<script setup lang="ts">
import { ref, computed } from 'vue'
import { useAutoRefresh, formatNumber } from '../composables/useAutoRefresh'
import PageHeader from '../components/PageHeader.vue'
import StatCard from '../components/StatCard.vue'
import DataCard from '../components/DataCard.vue'
import ProgressBar from '../components/ProgressBar.vue'
import type { DependenciesData } from '../types/api'

const data = ref<DependenciesData | null>(null)

const { loading } = useAutoRefresh(async () => {
  data.value = await fetch('/api/dependencies').then(r => r.json())
}, 30_000)

const toolMeta: Record<string, { icon: string; color: string; label: string }> = {
  go:    { icon: '🔵', color: 'text-sky-400',     label: 'Go' },
  npm:   { icon: '🟥', color: 'text-red-400',     label: 'npm' },
  pip:   { icon: '🐍', color: 'text-amber-400',   label: 'pip' },
  yarn:  { icon: '🔵', color: 'text-blue-400',    label: 'Yarn' },
  cargo: { icon: '🦀', color: 'text-orange-400',  label: 'Cargo' },
  gem:   { icon: '💎', color: 'text-rose-400',    label: 'Gems' },
  mvn:   { icon: '☕', color: 'text-amber-500',   label: 'Maven' },
  nuget: { icon: '💜', color: 'text-violet-400',  label: 'NuGet' },
  composer: { icon: '🐘', color: 'text-blue-300', label: 'Composer' },
}

function toolInfo(name: string) {
  return toolMeta[name] ?? { icon: '📦', color: 'text-slate-300', label: name }
}

function toolPct(files: number): string {
  const total = data.value?.total_files ?? 0
  if (!total) return '0.0'
  return ((files / total) * 100).toFixed(1)
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

const maxNodeFiles = computed(() => {
  const nodes = data.value?.nodes ?? []
  return Math.max(...nodes.map(n => n.files ?? 0), 1)
})
</script>

<template>
  <div>
    <PageHeader title="Dependencies" subtitle="Package dependency distribution across MetaStore nodes — refreshes every 30s" />

    <div v-if="data" class="grid grid-cols-2 sm:grid-cols-4 gap-4 mb-6">
      <StatCard icon="📦" label="Total Files" :value="formatNumber(data.total_files ?? 0)" />
      <StatCard icon="🌍" label="Ecosystems" :value="data.ecosystems ?? 0" />
      <StatCard icon="🖥️" label="Nodes w/ Data" :value="data.nodes_with_data ?? data.nodes?.length ?? 0" />
      <StatCard icon="🕒" label="Last Ingested" :value="timeAgoUnix(data.ingested_at)" />
    </div>

    <!-- Tool overview with percentages + progress bars -->
    <DataCard :loading="loading" class="mb-6">
      <template #header>
        <h2 class="text-sm font-semibold text-slate-200">Package Manager Overview</h2>
      </template>
      <div v-if="data?.tools?.length" class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4 p-5">
        <div
          v-for="tool in data.tools"
          :key="tool.tool"
          class="bg-slate-800/40 border border-slate-700/30 rounded-xl p-4"
        >
          <div class="flex items-center gap-3 mb-3">
            <span class="text-2xl">{{ toolInfo(tool.tool).icon }}</span>
            <div class="flex-1 min-w-0">
              <div class="text-sm font-semibold text-slate-200">{{ toolInfo(tool.tool).label }}</div>
              <div :class="toolInfo(tool.tool).color" class="text-xs font-medium">{{ toolPct(tool.files) }}%</div>
            </div>
            <div class="text-right">
              <div class="text-lg font-bold text-slate-200">{{ formatNumber(tool.files) }}</div>
              <div class="text-xs text-slate-500">files</div>
            </div>
          </div>
          <ProgressBar :value="parseFloat(toolPct(tool.files))" />
        </div>
      </div>
      <div v-else class="py-12 text-center text-slate-400 text-sm">No dependency data available</div>
    </DataCard>

    <!-- Node distribution -->
    <DataCard :loading="loading">
      <template #header>
        <h2 class="text-sm font-semibold text-slate-200">Node Distribution</h2>
      </template>
      <div v-if="data?.nodes?.length">
        <div class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="border-b border-slate-700/40 text-xs text-slate-400 uppercase tracking-wider">
                <th class="text-left px-6 py-3 font-medium">Node</th>
                <th class="text-right px-6 py-3 font-medium">Files</th>
                <th class="text-left px-6 py-3 font-medium w-48">Distribution</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-slate-700/20">
              <tr v-for="node in data.nodes" :key="node.node_id" class="hover:bg-slate-800/30 transition-colors">
                <td class="px-6 py-3 font-mono text-xs text-slate-300 max-w-[200px] truncate">{{ node.node_id }}</td>
                <td class="px-6 py-3 text-right text-slate-300 font-medium">{{ formatNumber(node.files ?? 0) }}</td>
                <td class="px-6 py-3 w-48">
                  <ProgressBar :value="maxNodeFiles > 0 ? Math.round(((node.files ?? 0) / maxNodeFiles) * 100) : 0" />
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
      <div v-else class="py-12 text-center text-slate-400 text-sm">No node distribution data</div>
    </DataCard>
  </div>
</template>
