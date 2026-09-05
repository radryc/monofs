<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'
import PageHeader from '../components/PageHeader.vue'
import type { SearchIndex, SearchResponse, SearchResult } from '../types/api'

// Search state
const query = ref('')
const filterStorageId = ref('')
const caseSensitive = ref(false)
const regexMode = ref(false)
const filePattern = ref('')
const searching = ref(false)
const searchError = ref('')
const response = ref<SearchResponse | null>(null)
const currentPage = ref(1)
const perPage = 20

// Indexes for filter dropdown
const indexes = ref<SearchIndex[]>([])

// File viewer
const viewerOpen = ref(false)
const viewerFile = ref('')
const viewerContent = ref('')
const viewerLanguage = ref('')
const viewerLine = ref(0)
const viewerLoading = ref(false)

const allResults = computed(() => response.value?.results ?? [])
const totalPages = computed(() => Math.ceil(allResults.value.length / perPage))
const pageResults = computed(() => allResults.value.slice((currentPage.value - 1) * perPage, currentPage.value * perPage))

onMounted(async () => {
  try {
    const data = await fetch('/api/search/indexes').then(r => r.json())
    indexes.value = (data.indexes ?? []).filter((i: SearchIndex) => i.status === 3)
  } catch {}
})

async function executeSearch() {
  const q = query.value.trim()
  if (!q) return
  searching.value = true
  searchError.value = ''
  response.value = null
  currentPage.value = 1
  try {
    const res = await fetch('/api/search', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        query: q,
        storage_id: filterStorageId.value,
        case_sensitive: caseSensitive.value,
        regex: regexMode.value,
        max_results: 500,
        file_patterns: filePattern.value ? [filePattern.value] : [],
      }),
    })
    if (!res.ok) throw new Error(await res.text())
    response.value = await res.json()
  } catch (e) {
    searchError.value = e instanceof Error ? e.message : String(e)
  } finally {
    searching.value = false
  }
}

async function openFile(storageId: string, filePath: string, lineNum: number) {
  viewerOpen.value = true
  viewerFile.value = filePath
  viewerLine.value = lineNum
  viewerLoading.value = true
  viewerContent.value = ''
  try {
    const res = await fetch('/api/file/content', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ storage_id: storageId, file_path: filePath }),
    })
    const data = await res.json()
    viewerContent.value = data.content ?? ''
    viewerLanguage.value = data.language ?? 'text'
  } catch {
    viewerContent.value = 'Failed to load file content'
  } finally {
    viewerLoading.value = false
  }
}

function closeViewer() { viewerOpen.value = false }

// Escape handler
function onKeydown(e: KeyboardEvent) {
  if (e.key === 'Escape') closeViewer()
  if (e.key === 'Enter' && !viewerOpen.value) executeSearch()
}
onMounted(() => document.addEventListener('keydown', onKeydown))
onUnmounted(() => document.removeEventListener('keydown', onKeydown))

function escapeHtml(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
}

function highlightResult(r: SearchResult & { line_content?: string; matches?: {start:number;end:number}[] }): string {
  let content = escapeHtml(r.line_content ?? r.content ?? '')
  const matches = (r as any).matches as {start:number;end:number}[] | undefined
  if (matches?.length) {
    const sorted = [...matches].sort((a, b) => b.start - a.start)
    for (const m of sorted) {
      const before = content.slice(0, m.start)
      const match = content.slice(m.start, m.end)
      const after = content.slice(m.end)
      content = `${before}<mark class="bg-violet-500/30 text-violet-200 rounded px-0.5">${match}</mark>${after}`
    }
  }
  return content
}

function repoLabel(r: SearchResult & { display_path?: string; storage_id?: string }): string {
  return (r as any).display_path || r.storage_id?.slice(0, 12) || '?'
}
</script>

<template>
  <div>
    <PageHeader title="Search" subtitle="Full-text code search across all indexed repositories" />

    <!-- Search bar -->
    <div class="bg-slate-900/60 backdrop-blur-sm border border-slate-700/40 rounded-2xl p-5 mb-6">
      <div class="flex gap-3 mb-4">
        <input
          v-model="query"
          type="text"
          placeholder="Search code..."
          class="flex-1 bg-slate-800/60 border border-slate-700/40 rounded-xl px-4 py-3 text-sm text-slate-200 placeholder-slate-500 focus:outline-none focus:border-violet-500/60 focus:ring-1 focus:ring-violet-500/20 font-mono"
          @keydown.enter="executeSearch"
        />
        <button
          @click="executeSearch"
          :disabled="searching"
          class="px-6 py-3 rounded-xl text-sm font-semibold bg-violet-600/20 text-violet-300 border border-violet-500/30 hover:bg-violet-600/30 transition-all disabled:opacity-50 disabled:cursor-not-allowed"
        >
          {{ searching ? 'Searching...' : 'Search' }}
        </button>
      </div>

      <!-- Filters row -->
      <div class="flex flex-wrap gap-3 items-center">
        <select
          v-model="filterStorageId"
          class="bg-slate-800/60 border border-slate-700/40 rounded-lg px-3 py-2 text-sm text-slate-300 focus:outline-none focus:border-violet-500/60"
        >
          <option value="">All repositories</option>
          <option v-for="idx in indexes" :key="idx.storage_id" :value="idx.storage_id">
            {{ idx.display_path || idx.storage_id?.slice(0, 20) }}
          </option>
        </select>

        <input
          v-model="filePattern"
          type="text"
          placeholder="File pattern (e.g. *.go)"
          class="bg-slate-800/60 border border-slate-700/40 rounded-lg px-3 py-2 text-sm text-slate-300 placeholder-slate-500 focus:outline-none focus:border-violet-500/60 w-48"
        />

        <label class="flex items-center gap-2 text-sm text-slate-400 cursor-pointer select-none">
          <input type="checkbox" v-model="caseSensitive" class="rounded border-slate-600 bg-slate-800 text-violet-500 focus:ring-violet-500/30" />
          Case sensitive
        </label>

        <label class="flex items-center gap-2 text-sm text-slate-400 cursor-pointer select-none">
          <input type="checkbox" v-model="regexMode" class="rounded border-slate-600 bg-slate-800 text-violet-500 focus:ring-violet-500/30" />
          Regex
        </label>
      </div>
    </div>

    <!-- Results -->
    <div v-if="searching" class="flex items-center justify-center py-16 gap-3 text-slate-400">
      <svg class="animate-spin w-5 h-5" viewBox="0 0 24 24" fill="none">
        <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"/>
        <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z"/>
      </svg>
      Searching...
    </div>

    <div v-else-if="searchError" class="bg-rose-950/30 border border-rose-500/20 rounded-2xl p-6 text-rose-400 text-sm">
      {{ searchError }}
    </div>

    <div v-else-if="response">
      <!-- Results header -->
      <div class="flex items-center justify-between mb-4">
        <div class="text-sm text-slate-400">
          Found <span class="font-semibold text-slate-200">{{ allResults.length }}</span> results
          <span v-if="response.duration_ms" class="ml-2 text-xs text-slate-500">
            in {{ response.duration_ms.toFixed(1) }}ms
          </span>
        </div>
        <div v-if="totalPages > 1" class="text-xs text-slate-400">
          Page {{ currentPage }} of {{ totalPages }}
        </div>
      </div>

      <div class="space-y-2 mb-6">
        <div
          v-for="(r, i) in pageResults"
          :key="i"
          @click="openFile((r as any).storage_id || '', (r as any).file_path || r.file, r.line)"
          class="bg-slate-900/60 border border-slate-700/40 rounded-xl p-4 cursor-pointer hover:border-violet-500/40 hover:bg-slate-800/40 transition-all"
        >
          <div class="flex items-center gap-2 mb-1.5">
            <span class="text-base">📄</span>
            <span class="font-mono text-sm font-semibold text-slate-200">
              {{ ((r as any).file_path || r.file || '').split('/').pop() }}
            </span>
            <span class="text-slate-500 text-xs">:{{ r.line }}</span>
            <span class="ml-auto text-xs text-slate-500 bg-slate-800/60 px-2 py-0.5 rounded-full">
              {{ repoLabel(r as any) }}
            </span>
          </div>
          <div class="font-mono text-xs text-slate-500 mb-2 truncate">
            {{ (r as any).file_path || r.file }}
          </div>
          <div class="font-mono text-xs bg-slate-950/60 rounded-lg px-3 py-2 text-slate-300 overflow-x-auto">
            <span class="text-slate-600 mr-3 select-none">{{ r.line }}</span>
            <span v-html="highlightResult(r as any)" />
          </div>
        </div>
      </div>

      <!-- Pagination -->
      <div v-if="totalPages > 1" class="flex items-center justify-center gap-3">
        <button
          @click="currentPage--"
          :disabled="currentPage === 1"
          class="px-4 py-2 rounded-lg text-sm border border-slate-700/40 text-slate-300 disabled:opacity-40 hover:bg-slate-800/40 transition-all"
        >← Prev</button>
        <span class="text-sm text-slate-400">{{ currentPage }} / {{ totalPages }}</span>
        <button
          @click="currentPage++"
          :disabled="currentPage === totalPages"
          class="px-4 py-2 rounded-lg text-sm border border-slate-700/40 text-slate-300 disabled:opacity-40 hover:bg-slate-800/40 transition-all"
        >Next →</button>
      </div>
    </div>

    <div v-else class="text-center py-20 text-slate-500 text-sm">
      Enter a search query above and press Enter or click Search
    </div>

    <!-- File viewer modal -->
    <Teleport to="body">
      <div
        v-if="viewerOpen"
        class="fixed inset-0 bg-slate-950/80 backdrop-blur-sm z-50 flex items-start justify-center p-6 overflow-auto"
        @click.self="closeViewer"
      >
        <div class="w-full max-w-5xl bg-slate-900 border border-slate-700/60 rounded-2xl shadow-2xl overflow-hidden">
          <!-- Viewer header -->
          <div class="flex items-center justify-between px-5 py-4 border-b border-slate-700/40">
            <div class="font-mono text-sm text-slate-300 truncate flex-1">{{ viewerFile }}</div>
            <div class="flex items-center gap-3 ml-4">
              <span v-if="viewerLine" class="text-xs text-slate-400">Line {{ viewerLine }}</span>
              <button @click="closeViewer" class="text-slate-400 hover:text-slate-200 transition-colors">✕</button>
            </div>
          </div>
          <!-- Viewer content -->
          <div v-if="viewerLoading" class="flex items-center justify-center py-16 text-slate-400 gap-3 text-sm">
            <svg class="animate-spin w-5 h-5" viewBox="0 0 24 24" fill="none">
              <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"/>
              <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z"/>
            </svg>
            Loading file...
          </div>
          <pre v-else class="p-5 text-xs font-mono text-slate-300 overflow-auto max-h-[70vh] leading-relaxed whitespace-pre-wrap bg-slate-950/60"><code>{{ viewerContent }}</code></pre>
        </div>
      </div>
    </Teleport>
  </div>
</template>
