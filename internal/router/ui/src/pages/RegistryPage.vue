<script setup lang="ts">
import { ref } from 'vue'
import { useAutoRefresh, formatBytes, formatNumber, timeAgoUnix } from '../composables/useAutoRefresh'
import PageHeader from '../components/PageHeader.vue'
import DataCard from '../components/DataCard.vue'
import type {
  RegistryStats,
  RegistryRepoDetail,
  RegistryRepoSummary,
  RegistryRepoTag,
  RegistryReposResponse,
} from '../types/api'

const PAGE_SIZE = 100

const stats = ref<RegistryStats | null>(null)
const repos = ref<RegistryRepoSummary[]>([])
const totalRepos = ref(0)
const pageOffset = ref(0)
const hasMore = ref(false)
const nextOffset = ref(0)

const searchInput = ref('')
const searchQuery = ref('')
const dedupRatio = ref(0)
const lastUpdatedAt = ref<number | null>(null)
const refreshInFlight = ref(false)
const refreshError = ref('')

const expandedRepos = ref<Set<string>>(new Set())
type RepoTagState = {
  loading: boolean
  loaded: boolean
  stale: boolean
  error?: string
  tags: RegistryRepoTag[]
}
const repoTags = ref<Record<string, RepoTagState>>({})

let refreshGeneration = 0

function normalizeRepoItems(payload: unknown): RegistryRepoSummary[] {
  const value = payload as Partial<RegistryReposResponse> | null
  const items = Array.isArray(value?.items) ? value!.items : null
  if (items && items.length > 0) {
    return items
      .map(item => ({
        name: typeof item.name === 'string' ? item.name : '',
        updated_at: Number(item.updated_at || 0),
      }))
      .filter(item => item.name.length > 0)
  }

  const legacyNames = Array.isArray(value?.repositories) ? value!.repositories : []
  return legacyNames
    .filter((name): name is string => typeof name === 'string' && name.length > 0)
    .map(name => ({ name, updated_at: 0 }))
}

function normalizeTags(payload: unknown): RegistryRepoTag[] {
  const tags = (payload || []) as unknown[]
  const normalized = tags
    .map(tag => {
      const value = tag as Record<string, unknown>
      const name = typeof value.name === 'string' ? value.name : ''
      const digest = typeof value.digest === 'string' ? value.digest : '-'
      return { name, digest }
    })
    .filter(tag => tag.name.length > 0)

  normalized.sort((a, b) => a.name.localeCompare(b.name))
  return normalized
}

function shortDigest(digest: string): string {
  if (!digest || digest === '-') return '-'
  if (digest.length <= 24) return digest
  return `${digest.slice(0, 18)}...${digest.slice(-6)}`
}

function encodeRepoNameForPath(name: string): string {
  return name
    .split('/')
    .map(part => encodeURIComponent(part))
    .join('/')
}

function formatUpdated(ts: number): string {
  if (!ts) return 'unknown'
  return `${timeAgoUnix(ts)}`
}

async function fetchJSON(path: string): Promise<any> {
  const resp = await fetch(path, { cache: 'no-store' })
  if (!resp.ok) {
    const body = (await resp.text()).slice(0, 200)
    throw new Error(`${resp.status} ${resp.statusText}${body ? `: ${body}` : ''}`)
  }
  return resp.json()
}

async function refreshData() {
  if (refreshInFlight.value) return
  refreshInFlight.value = true
  const generation = ++refreshGeneration
  refreshError.value = ''

  try {
    const params = new URLSearchParams()
    params.set('limit', String(PAGE_SIZE))
    params.set('offset', String(pageOffset.value))
    if (searchQuery.value) params.set('q', searchQuery.value)
    const reposPath = `/api/registry/repos?${params.toString()}`

    const [statsRes, reposRes] = await Promise.all([
      fetchJSON('/api/registry/stats'),
      fetchJSON(reposPath),
    ])
    if (generation !== refreshGeneration) return

    stats.value = statsRes as RegistryStats
    const list = reposRes as RegistryReposResponse
    repos.value = normalizeRepoItems(list)
    totalRepos.value = Number(list.total || repos.value.length)
    hasMore.value = Boolean(list.has_more)
    nextOffset.value = Number(list.next_offset || pageOffset.value + repos.value.length)
    lastUpdatedAt.value = Date.now()

    if (stats.value && stats.value.bytes_fetched > 0) {
      dedupRatio.value = Math.round((1 - stats.value.bytes_fetched / (stats.value.bytes_served + stats.value.bytes_fetched)) * 100)
    } else {
      dedupRatio.value = 0
    }
  } catch (e) {
    refreshError.value = e instanceof Error ? e.message : String(e)
  } finally {
    refreshInFlight.value = false
  }
}

async function loadTags(repoName: string, force = false) {
  const existing = repoTags.value[repoName]
  if (existing && existing.loaded && !force) return

  const loadingState: RepoTagState = existing ?? { loading: false, loaded: false, stale: false, tags: [] }
  loadingState.loading = true
  loadingState.error = undefined
  repoTags.value[repoName] = loadingState

  try {
    const detail = (await fetchJSON(`/api/registry/repos/${encodeRepoNameForPath(repoName)}`)) as RegistryRepoDetail
    const nextState: RepoTagState = {
      loading: false,
      loaded: true,
      stale: false,
      tags: normalizeTags(detail.tags),
    }
    repoTags.value[repoName] = nextState
  } catch (e) {
    const fallbackTags = existing?.tags ?? []
    repoTags.value[repoName] = {
      loading: false,
      loaded: true,
      stale: fallbackTags.length > 0,
      error: e instanceof Error ? e.message : String(e),
      tags: fallbackTags,
    }
  }
}

async function toggleRepo(repoName: string) {
  const next = new Set(expandedRepos.value)
  if (next.has(repoName)) {
    next.delete(repoName)
    expandedRepos.value = next
    return
  }
  next.add(repoName)
  expandedRepos.value = next
  await loadTags(repoName)
}

async function applySearch() {
  searchQuery.value = searchInput.value.trim()
  pageOffset.value = 0
  await refreshData()
}

async function clearSearch() {
  searchInput.value = ''
  searchQuery.value = ''
  pageOffset.value = 0
  await refreshData()
}

async function goNextPage() {
  if (!hasMore.value) return
  pageOffset.value = nextOffset.value
  await refreshData()
}

async function goPrevPage() {
  if (pageOffset.value === 0) return
  pageOffset.value = Math.max(0, pageOffset.value - PAGE_SIZE)
  await refreshData()
}

useAutoRefresh(async () => {
  await refreshData()
}, 10000)
</script>

<template>
  <PageHeader title="Container Registry" subtitle="Browse images and drill down into tags" />

  <div class="grid grid-cols-1 md:grid-cols-4 gap-4 mb-6">
    <DataCard>
      <template #title>Total Images</template>
      <template #value>{{ formatNumber(totalRepos) }}</template>
    </DataCard>
    <DataCard>
      <template #title>Shown</template>
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
    <div class="px-4 py-3 border-b border-slate-700/40 flex flex-wrap items-center gap-3">
      <span class="text-sm font-medium text-slate-300">Images (newest tag changes first)</span>
      <span class="text-xs text-slate-500 ml-auto">Last update: {{ lastUpdatedAt ? new Date(lastUpdatedAt).toLocaleTimeString() : '-' }}</span>
      <span v-if="refreshInFlight" class="text-xs text-sky-300">Refreshing</span>
    </div>

    <div class="px-4 py-3 border-b border-slate-700/30 flex flex-wrap gap-2 items-center">
      <input
        v-model="searchInput"
        type="text"
        placeholder="Search image name..."
        class="bg-slate-900/60 border border-slate-700/40 rounded-lg px-3 py-2 text-sm text-slate-200 placeholder-slate-500 focus:outline-none focus:border-violet-500/60 w-72"
        @keydown.enter="applySearch"
      />
      <button
        @click="applySearch"
        class="px-3 py-2 rounded-lg text-sm bg-violet-600/20 text-violet-300 border border-violet-500/30 hover:bg-violet-600/30 transition-all"
      >
        Search
      </button>
      <button
        @click="clearSearch"
        class="px-3 py-2 rounded-lg text-sm border border-slate-700/40 text-slate-300 hover:bg-slate-700/30 transition-all"
      >
        Clear
      </button>

      <div class="ml-auto flex items-center gap-2">
        <button
          @click="goPrevPage"
          :disabled="pageOffset === 0 || refreshInFlight"
          class="px-3 py-2 rounded-lg text-sm border border-slate-700/40 text-slate-300 disabled:opacity-40 hover:bg-slate-700/30 transition-all"
        >
          ← Prev
        </button>
        <span class="text-xs text-slate-400">
          {{ totalRepos === 0 ? 0 : pageOffset + 1 }}-{{ Math.min(pageOffset + repos.length, totalRepos) }} of {{ formatNumber(totalRepos) }}
        </span>
        <button
          @click="goNextPage"
          :disabled="!hasMore || refreshInFlight"
          class="px-3 py-2 rounded-lg text-sm border border-slate-700/40 text-slate-300 disabled:opacity-40 hover:bg-slate-700/30 transition-all"
        >
          Next →
        </button>
      </div>
    </div>

    <div v-if="refreshError" class="px-4 py-3 text-sm text-amber-300 border-b border-slate-700/30">
      {{ refreshError }}
    </div>

    <div v-if="repos.length === 0" class="p-8 text-center text-slate-500 text-sm">
      No images found
    </div>

    <div v-else class="divide-y divide-slate-700/30">
      <div
        v-for="repo in repos"
        :key="repo.name"
        class="px-4 py-3 hover:bg-slate-700/20 transition-colors"
      >
        <button
          class="w-full text-left"
          @click="toggleRepo(repo.name)"
        >
          <div class="flex items-center gap-3">
            <span class="text-slate-400 text-sm">{{ expandedRepos.has(repo.name) ? '▼' : '▶' }}</span>
            <span class="text-slate-300 text-sm">📦</span>
            <span class="text-sm font-medium text-slate-200">{{ repo.name }}</span>
            <span class="ml-auto text-xs text-slate-500">updated {{ formatUpdated(repo.updated_at) }}</span>
          </div>
        </button>

        <div v-if="expandedRepos.has(repo.name)" class="pl-9 pr-2 pt-2">
          <div v-if="repoTags[repo.name]?.loading" class="text-xs text-sky-300">Loading tags...</div>
          <div v-else-if="repoTags[repo.name]?.tags?.length" class="space-y-1">
            <div class="grid grid-cols-1 md:grid-cols-[minmax(0,220px)_minmax(0,1fr)] gap-x-4 gap-y-1 text-[11px] text-slate-400 pb-1 border-b border-slate-700/30 mb-1">
              <span>Tag</span>
              <span>Digest</span>
            </div>
            <div
              v-for="tag in repoTags[repo.name]?.tags"
              :key="`${repo.name}:${tag.name}:${tag.digest}`"
              class="grid grid-cols-1 md:grid-cols-[minmax(0,220px)_minmax(0,1fr)] gap-x-4 gap-y-1 text-xs py-1 border-b last:border-b-0 border-slate-700/20"
            >
              <span class="text-slate-200">{{ tag.name }}</span>
              <span class="font-mono text-slate-400" :title="tag.digest">{{ shortDigest(tag.digest) }}</span>
            </div>
          </div>
          <div v-else class="text-xs text-slate-500">No tags published yet</div>
          <div v-if="repoTags[repo.name]?.stale" class="mt-1 text-[11px] text-amber-300/90">
            Showing last known tags due to transient fetch error.
          </div>
          <div v-if="repoTags[repo.name]?.error && !repoTags[repo.name]?.stale" class="mt-1 text-[11px] text-amber-300/90">
            Failed to load tags: {{ repoTags[repo.name]?.error }}
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
