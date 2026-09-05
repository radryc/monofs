<script setup lang="ts">
import { ref, computed } from 'vue'
import { useAutoRefresh } from '../composables/useAutoRefresh'
import PageHeader from '../components/PageHeader.vue'
import DataCard from '../components/DataCard.vue'
import type { FeatureInfo, RoutersData } from '../types/api'

const routers = ref<RoutersData | null>(null)

const primaryRouterStatus = computed(() => {
  const all = routers.value?.routers ?? []
  const local = all.find((entry) => entry.local && entry.status)
  return local?.status ?? all.find((entry) => entry.status)?.status ?? null
})

const featureCatalog = computed<FeatureInfo[]>(() => primaryRouterStatus.value?.features ?? [])
const cicdFeatures = computed<FeatureInfo[]>(() => featureCatalog.value.filter((feature) => feature.status === 'runtime-config'))
const coreFeatures = computed<FeatureInfo[]>(() => featureCatalog.value.filter((feature) => feature.status !== 'runtime-config'))

async function load() {
  routers.value = await fetch('/api/routers').then((r) => r.json())
}

const { loading } = useAutoRefresh(load, 15_000)
</script>

<template>
  <div>
    <PageHeader title="CI/CD" subtitle="Runtime-configurable router features and rollout guidance" />

    <DataCard :loading="loading" class="mb-6">
      <template #header>
        <div class="flex items-center justify-between gap-4 flex-wrap">
          <div>
            <h2 class="text-base font-semibold text-slate-200">CI/CD Feature Controls</h2>
            <p class="text-xs text-slate-400 mt-0.5">Runtime-configurable feature switches for monofs-router. Apply flag changes in your deployment pipeline, then roll out the router.</p>
          </div>
          <div class="text-xs text-slate-500">{{ cicdFeatures.length }} switch{{ cicdFeatures.length === 1 ? '' : 'es' }}</div>
        </div>
      </template>

      <div class="px-4 pt-4">
        <div class="rounded-lg border border-sky-600/20 bg-sky-500/5 px-4 py-3 text-xs text-sky-100/85">
          <span class="font-semibold">How this works:</span>
          this panel shows the router's current state and the exact startup flags to enable or disable each CI/CD-managed feature.
        </div>
      </div>

      <div v-if="cicdFeatures.length" class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4 p-4">
        <div
          v-for="feature in cicdFeatures"
          :key="feature.id"
          class="rounded-xl border border-slate-700/30 bg-slate-900/30 px-4 py-4"
        >
          <div class="flex items-center justify-between gap-3">
            <div class="text-sm font-semibold text-slate-200">{{ feature.name }}</div>
            <span
              class="text-[11px] px-2 py-0.5 rounded-full border"
              :class="feature.enabled ? 'bg-emerald-500/10 text-emerald-300 border-emerald-500/20' : 'bg-slate-700/50 text-slate-300 border-slate-600/40'"
            >
              {{ feature.enabled ? 'enabled' : 'disabled' }}
            </span>
          </div>
          <div class="mt-1 text-xs text-slate-400">{{ feature.description }}</div>
          <div v-if="feature.help_hint" class="mt-2 text-[11px] text-slate-400">{{ feature.help_hint }}</div>
          <div class="mt-3 space-y-2">
            <div>
              <div class="text-[11px] font-semibold uppercase tracking-wide text-emerald-300/90">Enable</div>
              <code class="mt-1 block text-[11px] leading-relaxed text-emerald-200 bg-slate-950/70 border border-emerald-500/20 rounded px-2 py-1 whitespace-pre-wrap break-all">
                {{ feature.enable_hint || 'No enable hint provided' }}
              </code>
            </div>
            <div>
              <div class="text-[11px] font-semibold uppercase tracking-wide text-slate-300">Disable</div>
              <code class="mt-1 block text-[11px] leading-relaxed text-slate-200 bg-slate-950/70 border border-slate-600/30 rounded px-2 py-1 whitespace-pre-wrap break-all">
                {{ feature.disable_hint || 'No disable hint provided' }}
              </code>
            </div>
          </div>
          <div class="mt-2 text-[11px] text-slate-500">mode: runtime-config</div>
        </div>
      </div>
      <div v-else class="px-6 py-8 text-center text-sm text-slate-500">CI/CD feature controls unavailable</div>
    </DataCard>

    <DataCard :loading="loading" class="mb-6">
      <template #header>
        <div class="flex items-center justify-between gap-4 flex-wrap">
          <div>
            <h2 class="text-base font-semibold text-slate-200">Feature Help and Tutorial</h2>
            <p class="text-xs text-slate-400 mt-0.5">What each feature does and how to roll changes out safely.</p>
          </div>
          <div class="text-xs text-slate-500">{{ featureCatalog.length }} total feature{{ featureCatalog.length === 1 ? '' : 's' }}</div>
        </div>
      </template>

      <div class="p-4 grid grid-cols-1 lg:grid-cols-5 gap-4">
        <div class="lg:col-span-2 rounded-xl border border-slate-700/30 bg-slate-900/25 px-4 py-3">
          <div class="text-sm font-semibold text-slate-200">Quick Tutorial</div>
          <ol class="mt-2 space-y-1 text-xs text-slate-300 list-decimal list-inside">
            <li>Pick a feature in the CI/CD controls panel above.</li>
            <li>Add the enable or disable flag in your deployment manifest or startup args for monofs-router.</li>
            <li>For Policy-gated Sync, provide a policy file and set --policy-config to that path.</li>
            <li>Roll out or restart router pods so the new args are applied.</li>
            <li>Return to this page and confirm the feature status changed.</li>
          </ol>
        </div>

        <div class="lg:col-span-3 space-y-3">
          <div class="rounded-xl border border-slate-700/30 bg-slate-900/25 px-4 py-3">
            <div class="text-xs font-semibold uppercase tracking-wide text-slate-400">Runtime-config Features (CI/CD switches)</div>
            <div v-if="cicdFeatures.length" class="mt-2 space-y-2">
              <div v-for="feature in cicdFeatures" :key="`help-${feature.id}`" class="rounded-lg border border-slate-700/20 bg-slate-950/30 px-3 py-2">
                <div class="text-sm font-semibold text-slate-200">{{ feature.name }}</div>
                <p class="mt-1 text-xs text-slate-300">{{ feature.help_hint || feature.description }}</p>
              </div>
            </div>
            <div v-else class="mt-2 text-xs text-slate-500">No runtime-config features reported by this router.</div>
          </div>

          <div class="rounded-xl border border-slate-700/30 bg-slate-900/25 px-4 py-3">
            <div class="text-xs font-semibold uppercase tracking-wide text-slate-400">Always-on Core Features</div>
            <div v-if="coreFeatures.length" class="mt-2 space-y-2">
              <div v-for="feature in coreFeatures" :key="`core-${feature.id}`" class="rounded-lg border border-slate-700/20 bg-slate-950/30 px-3 py-2">
                <div class="flex items-center justify-between gap-3">
                  <div class="text-sm font-semibold text-slate-200">{{ feature.name }}</div>
                  <span class="text-[11px] px-2 py-0.5 rounded-full border bg-emerald-500/10 text-emerald-300 border-emerald-500/20">always enabled</span>
                </div>
                <p class="mt-1 text-xs text-slate-300">{{ feature.help_hint || feature.description }}</p>
              </div>
            </div>
            <div v-else class="mt-2 text-xs text-slate-500">No always-on features reported by this router.</div>
          </div>
        </div>
      </div>
    </DataCard>
  </div>
</template>