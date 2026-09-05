<script setup lang="ts">
import { ref } from 'vue'
import { useAutoRefresh } from '../composables/useAutoRefresh'
import PageHeader from '../components/PageHeader.vue'
import DataCard from '../components/DataCard.vue'
import StatCard from '../components/StatCard.vue'
import { Play, Clock, CheckCircle, XCircle, Hammer } from 'lucide-vue-next'
import type { PipelineSummary, PipelineStatsData } from '../types/api'

const pipelines = ref<PipelineSummary[]>([])
const stats = ref<PipelineStatsData | null>(null)

async function load() {
  try {
    const [plResp, stResp] = await Promise.all([
      fetch('/api/pipelines').then(r => r.json()),
      fetch('/api/pipelines/stats').then(r => r.json()),
    ])
    if (plResp?.pipelines) pipelines.value = plResp.pipelines
    if (stResp?.total_runs !== undefined) stats.value = stResp as PipelineStatsData
  } catch {}
}

const { loading } = useAutoRefresh(load, 10_000)

const stateIcon = (state: string) => {
  switch (state) {
    case 'succeeded': return CheckCircle
    case 'failed': return XCircle
    case 'running': return Clock
    case 'pending': return Clock
    default: return Hammer
  }
}

const stateColor = (state: string) => {
  switch (state) {
    case 'succeeded': return 'text-emerald-400'
    case 'failed': return 'text-red-400'
    case 'running': return 'text-amber-400'
    case 'pending': return 'text-blue-400'
    default: return 'text-slate-400'
  }
}
</script>

<template>
  <div>
    <PageHeader title="Pipelines" subtitle="CI/CD pipeline orchestration across the monorepo" />

    <div v-if="stats" class="grid grid-cols-2 sm:grid-cols-4 gap-4 mb-6">
      <StatCard icon="📊" label="Total Runs" :value="stats.total_runs" />
      <StatCard icon="✅" label="Succeeded" :value="stats.succeeded_runs" color="emerald"
        :sub="stats.success_rate.toFixed(0) + '% success rate'" />
      <StatCard icon="❌" label="Failed" :value="stats.failed_runs" color="rose" />
      <StatCard icon="⏱️" label="Avg Duration" :value="stats.avg_duration_ms + 'ms'" color="amber"
        :sub="'p50: ' + stats.p50_duration_ms + 'ms · p95: ' + stats.p95_duration_ms + 'ms'" />
    </div>

    <DataCard :loading="loading">
      <template #header>
        <div class="flex items-center justify-between">
          <h2 class="text-sm font-semibold text-slate-200">Pipeline Configurations</h2>
          <span class="text-xs text-slate-500">{{ pipelines.length }} configured</span>
        </div>
      </template>

      <div v-if="pipelines.length" class="divide-y divide-slate-700/20">
        <div
          v-for="pipeline in pipelines"
          :key="pipeline.name"
          class="px-5 py-4 hover:bg-slate-800/20 transition-colors flex items-center justify-between gap-4 flex-wrap"
        >
          <div class="min-w-0 flex-1">
            <div class="flex items-center gap-2">
              <component :is="stateIcon(pipeline.last_run_state)" class="w-4 h-4 shrink-0"
                :class="stateColor(pipeline.last_run_state)" />
              <span class="text-sm font-semibold text-slate-200 truncate font-mono">{{ pipeline.name }}</span>
            </div>
            <div class="mt-1 flex items-center gap-2 text-xs text-slate-500">
              <span v-if="pipeline.source_dir" class="text-slate-400">{{ pipeline.source_dir }}</span>
              <span class="px-1.5 py-0.5 rounded text-[11px] font-medium"
                :class="{
                  'bg-emerald-500/10 text-emerald-300': pipeline.last_run_state === 'succeeded',
                  'bg-red-500/10 text-red-300': pipeline.last_run_state === 'failed',
                  'bg-amber-500/10 text-amber-300': pipeline.last_run_state === 'running',
                  'bg-slate-700/50 text-slate-300': pipeline.last_run_state === 'unknown',
                }">
                {{ pipeline.last_run_state }}
              </span>
              <span v-if="pipeline.run_count > 0">{{ pipeline.run_count }} run{{ pipeline.run_count > 1 ? 's' : '' }}</span>
            </div>
          </div>
          <button
            class="flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-medium text-slate-300 bg-slate-800/60 hover:bg-slate-700/60 border border-slate-700/30 transition-colors"
          >
            <Play class="w-3 h-3" /> Run
          </button>
        </div>
      </div>
      <div v-else class="px-6 py-16 text-center">
        <Hammer class="w-10 h-10 text-slate-600 mx-auto mb-4" />
        <div class="text-sm text-slate-400">No pipelines configured</div>
        <div class="mt-2 text-xs text-slate-600 max-w-md mx-auto">
          <p>Add pipeline configs under <code class="text-violet-400">.monofs/pipelines/</code> in your workspace.</p>
        </div>
      </div>
    </DataCard>
  </div>
</template>
