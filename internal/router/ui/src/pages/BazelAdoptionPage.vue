<script setup lang="ts">
import { ref, computed } from 'vue'
import { useAutoRefresh } from '../composables/useAutoRefresh'
import PageHeader from '../components/PageHeader.vue'
import StatCard from '../components/StatCard.vue'
import DataCard from '../components/DataCard.vue'
import ProgressBar from '../components/ProgressBar.vue'
import type { BazelAdoptionData } from '../types/api'
import { CheckCircle, AlertTriangle, Clock, Package, Cpu } from 'lucide-vue-next'

const data = ref<BazelAdoptionData | null>(null)

const { loading } = useAutoRefresh(async () => {
  const resp = await fetch('/api/bazel/status').then(r => r.json())
  data.value = resp as BazelAdoptionData
}, 15_000)

const stateBadgeClass = (state: string) => {
  switch (state) {
    case 'native': return 'bg-slate-500/10 text-slate-400 border-slate-500/20'
    case 'generating': return 'bg-amber-500/10 text-amber-400 border-amber-500/20'
    case 'partial': return 'bg-sky-500/10 text-sky-400 border-sky-500/20'
    case 'active': return 'bg-emerald-500/10 text-emerald-400 border-emerald-500/20'
    case 'hermetic': return 'bg-violet-500/10 text-violet-400 border-violet-500/20'
    default: return 'bg-slate-500/10 text-slate-400 border-slate-500/20'
  }
}

const stateIcon = (state: string) => {
  switch (state) {
    case 'active': return CheckCircle
    case 'hermetic': return Cpu
    case 'generating': return Clock
    case 'partial': return AlertTriangle
    default: return Package
  }
}

const buildSystemLabel = (bs: string) => {
  const labels: Record<string, string> = {
    go: 'Go', npm: 'npm', cargo: 'Cargo', maven: 'Maven',
    gradle: 'Gradle', make: 'Make', unknown: '—',
  }
  return labels[bs] ?? bs
}

const migrationQueue = computed(() => {
  if (!data.value?.repos) return []
  return data.value.repos
    .filter(r => r.state === 'generating' || r.state === 'partial' || r.state === 'native')
    .slice(0, 3)
})

const nativeCount = computed(() =>
  data.value?.repos.filter(r => r.state === 'native').length ?? 0)
const generatingCount = computed(() =>
  data.value?.repos.filter(r => r.state === 'generating').length ?? 0)
const partialCount = computed(() =>
  data.value?.repos.filter(r => r.state === 'partial').length ?? 0)
</script>

<template>
  <div>
    <PageHeader title="Bazel Adoption" subtitle="Repo-by-repo migration progress toward hermetic Bazel builds" />

    <div v-if="data?.ok && data.total_repos > 0" class="grid grid-cols-2 sm:grid-cols-4 gap-4 mb-6">
      <StatCard icon="📦" label="Total Repos" :value="data.total_repos" />
      <StatCard icon="✅" label="Active" :value="data.active_count" color="emerald"
        :sub="data.hermetic_count + ' hermetic'" />
      <StatCard icon="📈" label="Adoption" :value="data.adoption_pct.toFixed(1) + '%'" color="violet"
        :sub="data.active_count + data.hermetic_count + ' of ' + data.total_repos" />
      <StatCard icon="🔄" label="Pending" :value="nativeCount + generatingCount + partialCount" color="amber"
        :sub="'native · generating · partial'" />
    </div>

    <DataCard v-if="data?.ok && data.total_repos > 0" :loading="loading">
      <template #header>
        <div class="flex items-center justify-between">
          <h2 class="text-sm font-semibold text-slate-200">Adoption Progress</h2>
          <span class="text-xs text-slate-500">{{ data.adoption_pct.toFixed(0) }}% migrated</span>
        </div>
      </template>
      <div class="px-5 pt-2 pb-4">
        <div class="h-3 rounded-full bg-slate-800 overflow-hidden flex">
          <div v-if="data.hermetic_count > 0"
            class="h-full bg-violet-500 transition-all duration-500"
            :style="{ width: `${(data.hermetic_count / data.total_repos) * 100}%` }" />
          <div v-if="data.active_count > 0"
            class="h-full bg-emerald-500 transition-all duration-500"
            :style="{ width: `${(data.active_count / data.total_repos) * 100}%` }" />
          <div v-if="partialCount > 0"
            class="h-full bg-sky-500 transition-all duration-500"
            :style="{ width: `${(partialCount / data.total_repos) * 100}%` }" />
          <div v-if="generatingCount > 0"
            class="h-full bg-amber-500 transition-all duration-500"
            :style="{ width: `${(generatingCount / data.total_repos) * 100}%` }" />
        </div>
        <div class="flex items-center gap-3 mt-2 text-[11px] text-slate-500 flex-wrap">
          <span class="flex items-center gap-1"><span class="w-2.5 h-2.5 rounded-sm bg-violet-500 inline-block" /> Hermetic {{ data.hermetic_count }}</span>
          <span class="flex items-center gap-1"><span class="w-2.5 h-2.5 rounded-sm bg-emerald-500 inline-block" /> Active {{ data.active_count }}</span>
          <span class="flex items-center gap-1"><span class="w-2.5 h-2.5 rounded-sm bg-sky-500 inline-block" /> Partial {{ partialCount }}</span>
          <span class="flex items-center gap-1"><span class="w-2.5 h-2.5 rounded-sm bg-amber-500 inline-block" /> Generating {{ generatingCount }}</span>
          <span class="flex items-center gap-1"><span class="w-2.5 h-2.5 rounded-sm bg-slate-500 inline-block" /> Native {{ nativeCount }}</span>
        </div>
      </div>
    </DataCard>

    <div class="grid grid-cols-1 lg:grid-cols-3 gap-6 mt-6">
      <DataCard v-if="data?.ok && data.repos.length" :loading="loading" class="lg:col-span-2">
        <template #header>
          <h2 class="text-sm font-semibold text-slate-200">Repositories</h2>
        </template>
        <div class="divide-y divide-slate-700/20">
          <div
            v-for="repo in data.repos"
            :key="repo.display_path"
            class="px-5 py-3.5 hover:bg-slate-800/20 transition-colors"
          >
            <div class="flex items-center justify-between gap-4 flex-wrap">
              <div class="min-w-0 flex-1">
                <div class="flex items-center gap-2">
                  <component :is="stateIcon(repo.state)" class="w-3.5 h-3.5 shrink-0"
                    :class="{
                      'text-emerald-400': repo.state === 'active',
                      'text-violet-400': repo.state === 'hermetic',
                      'text-sky-400': repo.state === 'partial',
                      'text-amber-400': repo.state === 'generating',
                      'text-slate-500': repo.state === 'native',
                    }" />
                  <span class="text-sm font-medium text-slate-200 truncate font-mono">{{ repo.display_path }}</span>
                </div>
                <div class="flex items-center gap-2 mt-1 text-xs text-slate-500">
                  <span>{{ buildSystemLabel(repo.build_system) }}</span>
                  <span v-if="repo.commit_hash" class="text-slate-600">·</span>
                  <span v-if="repo.commit_hash" class="font-mono text-slate-600">{{ repo.commit_hash.slice(0, 7) }}</span>
                </div>
              </div>
              <div class="flex items-center gap-3 shrink-0">
                <div class="hidden sm:block w-24">
                  <ProgressBar :value="repo.state === 'hermetic' ? 100 : repo.state === 'active' ? 80 : repo.state === 'partial' ? 50 : repo.state === 'generating' ? 25 : 5" />
                </div>
                <span class="text-xs font-medium px-2 py-0.5 rounded-md border"
                  :class="stateBadgeClass(repo.state)">
                  {{ repo.state }}
                </span>
              </div>
            </div>
          </div>
        </div>
      </DataCard>

      <div class="space-y-6">
        <DataCard v-if="migrationQueue.length" :loading="false">
          <template #header>
            <h2 class="text-sm font-semibold text-slate-200 flex items-center gap-2">
              <AlertTriangle class="w-3.5 h-3.5 text-amber-400" /> Migration Queue
            </h2>
          </template>
          <div class="divide-y divide-slate-700/20">
            <div v-for="repo in migrationQueue" :key="repo.display_path"
              class="px-5 py-3.5">
              <div class="flex items-center gap-2 mb-2">
                <span class="text-xs font-mono text-slate-300 truncate">{{ repo.display_path }}</span>
                <span class="text-[11px] font-medium px-1.5 py-0.5 rounded border"
                  :class="stateBadgeClass(repo.state)">{{ repo.state }}</span>
              </div>
              <div class="text-[11px] text-slate-500 space-y-0.5">
                <p v-if="repo.state === 'native'">
                  Run <code class="text-violet-400">monofs-bazelctl generate --repo={{ repo.display_path }}</code>
                </p>
                <p v-if="repo.state === 'generating'">
                  Run <code class="text-violet-400">monofs-bazelctl validate --repo={{ repo.display_path }}</code> then promote
                </p>
                <p v-if="repo.state === 'partial'">
                  Run <code class="text-violet-400">monofs-bazelctl generate --repo={{ repo.display_path }}</code> to fill gaps
                </p>
              </div>
            </div>
          </div>
        </DataCard>

        <DataCard :loading="false">
          <template #header>
            <h2 class="text-sm font-semibold text-slate-200">State Reference</h2>
          </template>
          <div class="px-5 py-3 space-y-2 text-xs">
            <div class="flex items-start gap-2">
              <span class="text-slate-400 w-20 shrink-0 mt-0.5">native</span>
              <span class="text-slate-500">No BUILD files, uses native build (Make/go/npm)</span>
            </div>
            <div class="flex items-start gap-2">
              <span class="text-amber-400 w-20 shrink-0 mt-0.5">generating</span>
              <span class="text-slate-500">BUILD files being generated and validated</span>
            </div>
            <div class="flex items-start gap-2">
              <span class="text-sky-400 w-20 shrink-0 mt-0.5">partial</span>
              <span class="text-slate-500">Some BUILD files exist, native still primary</span>
            </div>
            <div class="flex items-start gap-2">
              <span class="text-emerald-400 w-20 shrink-0 mt-0.5">active</span>
              <span class="text-slate-500">Full BUILD coverage, Bazel is primary</span>
            </div>
            <div class="flex items-start gap-2">
              <span class="text-violet-400 w-20 shrink-0 mt-0.5">hermetic</span>
              <span class="text-slate-500">Bazel-only, no native build fallbacks</span>
            </div>
          </div>
        </DataCard>
      </div>
    </div>

    <div v-if="data?.ok && data.total_repos === 0" class="rounded-xl border border-slate-700/30 bg-slate-900/25 px-6 py-16 text-center">
      <Package class="w-10 h-10 text-slate-600 mx-auto mb-4" />
      <div class="text-sm text-slate-400">No Bazel workspace detected</div>
      <div class="mt-2 text-xs text-slate-600 max-w-md mx-auto">
        <p>Mount the workspace and ensure <code class="text-violet-400">.monofs/workspace.json</code> exists.</p>
      </div>
    </div>

    <div v-if="data && !data.ok" class="rounded-xl border border-slate-700/30 bg-slate-900/25 px-6 py-16 text-center">
      <AlertTriangle class="w-10 h-10 text-amber-500 mx-auto mb-4" />
      <div class="text-sm text-slate-400">{{ data.message || 'Unable to load Bazel status' }}</div>
    </div>
  </div>
</template>
