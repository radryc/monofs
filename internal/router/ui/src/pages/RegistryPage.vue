<script setup lang="ts">
import { ref } from 'vue'
import { useAutoRefresh, formatBytes, formatNumber } from '../composables/useAutoRefresh'
import PageHeader from '../components/PageHeader.vue'
import DataCard from '../components/DataCard.vue'
import type { RegistryStats } from '../types/api'

const stats = ref<RegistryStats | null>(null)
const repos = ref<string[]>([])

const dedupRatio = ref(0)

useAutoRefresh(async () => {
  try {
    const [statsRes, reposRes] = await Promise.all([
      fetch('/api/registry/stats').then(r => r.json()),
      fetch('/api/registry/repos').then(r => r.json()),
    ])
    stats.value = statsRes as RegistryStats
    repos.value = (reposRes as any)?.repositories || []
    if (stats.value && stats.value.bytes_fetched > 0) {
      dedupRatio.value = Math.round((1 - stats.value.bytes_fetched / (stats.value.bytes_served + stats.value.bytes_fetched)) * 100)
    }
  } catch {}
}, 5000)
</script>

<template>
  <PageHeader title="Container Registry" subtitle="OCI image storage" />

  <div class="grid grid-cols-1 md:grid-cols-3 gap-4 mb-6">
    <DataCard>
      <template #title>Repositories</template>
      <template #value>{{ repos.length }}</template>
    </DataCard>
    <DataCard>
      <template #title>Cache Eff.</template>
      <template #value>{{ dedupRatio }}%</template>
    </DataCard>
    <DataCard>
      <template #title>Blob Count</template>
      <template #value>{{ formatNumber(stats?.blob_count || 0) }}</template>
    </DataCard>
  </div>

  <div class="grid grid-cols-1 md:grid-cols-4 gap-4 mb-6">
    <DataCard>
      <template #title>Pulls</template>
      <template #value>{{ formatNumber(stats?.pulls || 0) }}</template>
    </DataCard>
    <DataCard>
      <template #title>Pushes</template>
      <template #value>{{ formatNumber(stats?.pushes || 0) }}</template>
    </DataCard>
    <DataCard>
      <template #title>Cache Hits</template>
      <template #value>{{ formatNumber(stats?.cache_hits || 0) }}</template>
    </DataCard>
    <DataCard>
      <template #title>Bytes Served</template>
      <template #value>{{ formatBytes(stats?.bytes_served || 0) }}</template>
    </DataCard>
  </div>

  <div class="bg-slate-800/40 border border-slate-700/40 rounded-lg">
    <div class="px-4 py-3 border-b border-slate-700/40">
      <span class="text-sm font-medium text-slate-300">Repositories</span>
    </div>
    <div v-if="repos.length === 0" class="p-8 text-center text-slate-500 text-sm">
      No repositories yet
    </div>
    <div v-else class="divide-y divide-slate-700/30">
      <div
        v-for="repo in repos"
        :key="repo"
        class="px-4 py-2.5 flex items-center gap-3 hover:bg-slate-700/20 transition-colors"
      >
        <span class="text-slate-400 text-sm">&#128230;</span>
        <span class="text-sm text-slate-200">{{ repo }}</span>
      </div>
    </div>
  </div>
</template>
