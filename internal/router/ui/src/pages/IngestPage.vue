<script setup lang="ts">
import { ref, computed } from 'vue'
import { useAppStore } from '../stores/app'
import PageHeader from '../components/PageHeader.vue'
import DataCard from '../components/DataCard.vue'
import type { IngestResponse } from '../types/api'

const store = useAppStore()

const ingestionType = ref('git')
const source = ref('')
const refBranch = ref('')
const sourceId = ref('')
const fetchType = ref('blob')
const loading = ref(false)
const lastResult = ref<IngestResponse | null>(null)

const sourceLabel = computed(() => ingestionType.value === 'git' ? 'Repository URL' : 'Source')
const sourcePlaceholder = computed(() => 'https://github.com/user/repo.git')

async function submit() {
  if (!source.value.trim()) {
    store.addToast('error', 'Source URL is required')
    return
  }
  loading.value = true
  lastResult.value = null
  try {
    const params = new URLSearchParams({
      source: source.value.trim(),
      ref: refBranch.value.trim(),
      source_id: sourceId.value.trim(),
      ingestion_type: ingestionType.value,
      fetch_type: fetchType.value,
      replicate_data: 'false',
    })
    const res = await fetch('/api/ingest', {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: params.toString(),
    })
    const data: IngestResponse = await res.json()
    lastResult.value = data
    if (data.success) {
      store.addToast('success', `Ingestion started: ${source.value}`)
      source.value = ''
      refBranch.value = ''
      sourceId.value = ''
    } else {
      store.addToast('error', data.message || 'Ingestion failed')
    }
  } catch (e) {
    store.addToast('error', 'Network error')
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div>
    <PageHeader title="Ingest" subtitle="Add a new source to the MonoFS cluster" />

    <div class="grid grid-cols-1 xl:grid-cols-2 gap-6">
      <!-- Ingest form -->
      <DataCard>
        <template #header>
          <h2 class="text-sm font-semibold text-slate-200">New Ingestion</h2>
        </template>
        <form @submit.prevent="submit" class="px-6 py-5 space-y-5">
          <!-- Source type -->
          <div>
            <label class="block text-sm font-semibold text-slate-200 mb-2">
              Source Type <span class="text-rose-400">*</span>
            </label>
            <select
              v-model="ingestionType"
              class="w-full bg-slate-800/60 border border-slate-700/40 rounded-lg px-4 py-2.5 text-sm text-slate-200 focus:outline-none focus:border-violet-500/60 focus:ring-1 focus:ring-violet-500/20"
            >
              <option value="git">Git Repository</option>
              <option value="s3" disabled>S3 Bucket (Coming Soon)</option>
              <option value="file" disabled>Local Filesystem (Coming Soon)</option>
            </select>
          </div>

          <!-- Source URL -->
          <div>
            <label class="block text-sm font-semibold text-slate-200 mb-2">
              {{ sourceLabel }} <span class="text-rose-400">*</span>
            </label>
            <input
              v-model="source"
              type="text"
              :placeholder="sourcePlaceholder"
              required
              class="w-full bg-slate-800/60 border border-slate-700/40 rounded-lg px-4 py-2.5 text-sm text-slate-200 placeholder-slate-500 focus:outline-none focus:border-violet-500/60 focus:ring-1 focus:ring-violet-500/20"
            />
            <p class="text-xs text-slate-500 mt-1">Supports GitHub, GitLab, Gitea, etc.</p>
          </div>

          <!-- Branch -->
          <div>
            <label class="block text-sm font-medium text-slate-300 mb-2">Branch</label>
            <input
              v-model="refBranch"
              type="text"
              placeholder="main"
              class="w-full bg-slate-800/60 border border-slate-700/40 rounded-lg px-4 py-2.5 text-sm text-slate-200 placeholder-slate-500 focus:outline-none focus:border-violet-500/60 focus:ring-1 focus:ring-violet-500/20"
            />
            <p class="text-xs text-slate-500 mt-1">Optional, defaults to "main"</p>
          </div>

          <!-- Advanced -->
          <details class="border border-slate-700/40 rounded-xl overflow-hidden">
            <summary class="px-4 py-3 cursor-pointer text-sm font-medium text-slate-300 hover:text-slate-200 select-none">
              ⚙️ Advanced Options
            </summary>
            <div class="px-4 pb-4 pt-3 border-t border-slate-700/40 space-y-4">
              <div>
                <label class="block text-sm font-medium text-slate-300 mb-2">Source ID (optional)</label>
                <input
                  v-model="sourceId"
                  type="text"
                  placeholder="Auto-generated from source"
                  class="w-full bg-slate-800/60 border border-slate-700/40 rounded-lg px-4 py-2.5 text-sm text-slate-200 placeholder-slate-500 focus:outline-none focus:border-violet-500/60 focus:ring-1 focus:ring-violet-500/20"
                />
                <p class="text-xs text-slate-500 mt-1">Custom display path for this source</p>
              </div>
              <div>
                <label class="block text-sm font-medium text-slate-300 mb-2">Storage Backend</label>
                <select
                  v-model="fetchType"
                  class="w-full bg-slate-800/60 border border-slate-700/40 rounded-lg px-4 py-2.5 text-sm text-slate-200 focus:outline-none focus:border-violet-500/60 focus:ring-1 focus:ring-violet-500/20"
                >
                  <option value="git">Git (Lazy Fetch)</option>
                  <option value="blob">Blob (Packager Archive)</option>
                  <option value="s3" disabled>S3 Storage (Coming Soon)</option>
                  <option value="local" disabled>Local Cache (Coming Soon)</option>
                </select>
              </div>
            </div>
          </details>

          <button
            type="submit"
            :disabled="loading"
            class="w-full py-3 rounded-xl text-sm font-semibold transition-all"
            :class="loading
              ? 'bg-violet-600/30 text-violet-300/50 cursor-not-allowed'
              : 'bg-violet-600/20 text-violet-300 border border-violet-500/30 hover:bg-violet-600/30'"
          >
            <span v-if="loading" class="flex items-center justify-center gap-2">
              <svg class="animate-spin w-4 h-4" viewBox="0 0 24 24" fill="none">
                <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"/>
                <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z"/>
              </svg>
              Ingesting...
            </span>
            <span v-else>Ingest Repository</span>
          </button>
        </form>
      </DataCard>

      <!-- Result / info -->
      <div class="space-y-4">
        <DataCard v-if="lastResult">
          <template #header>
            <h2 class="text-sm font-semibold text-slate-200">Result</h2>
          </template>
          <div class="px-6 py-5">
            <div :class="lastResult.success ? 'text-emerald-400' : 'text-rose-400'" class="text-sm font-medium mb-2">
              {{ lastResult.success ? '✓ Success' : '✗ Failed' }}
            </div>
            <p class="text-sm text-slate-300">{{ lastResult.message }}</p>
          </div>
        </DataCard>

        <DataCard>
          <template #header>
            <h2 class="text-sm font-semibold text-slate-200">About Ingestion</h2>
          </template>
          <div class="px-6 py-5 space-y-3 text-sm text-slate-400">
            <p><span class="text-slate-300 font-medium">Git (Lazy Fetch)</span> — Files are fetched on-demand from the remote Git repository.</p>
            <p><span class="text-slate-300 font-medium">Blob (Packager Archive)</span> — Repository is archived as a blob package and stored in the MetaStore. Recommended for most workloads.</p>
            <p class="text-xs text-slate-500">Ingestion runs asynchronously. Check the Repositories page to monitor progress.</p>
          </div>
        </DataCard>
      </div>
    </div>
  </div>
</template>
