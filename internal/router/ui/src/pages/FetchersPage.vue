<script setup lang="ts">
import { ref, computed } from 'vue'
import { useAutoRefresh, formatBytes, formatNumber } from '../composables/useAutoRefresh'
import PageHeader from '../components/PageHeader.vue'
import StatCard from '../components/StatCard.vue'
import DataCard from '../components/DataCard.vue'
import NodeBadge from '../components/NodeBadge.vue'
import type { FetcherStats, FetcherKeyStatus, LogEngineData, RegistryStats, FetcherStorageObjectsResponse, StorageObject } from '../types/api'

const fetchers = ref<FetcherStats | null>(null)
const keyStatus = ref<FetcherKeyStatus | null>(null)
const logEngine = ref<LogEngineData | null>(null)
const registryStats = ref<RegistryStats | null>(null)
const storageObjects = ref<FetcherStorageObjectsResponse | null>(null)
const detailed = ref(false)
const confirming = ref(false)
const confirmMessage = ref('')

async function loadFetchers() {
  const url = detailed.value ? '/api/fetchers?detailed=true' : '/api/fetchers'
  fetchers.value = await fetch(url).then(r => r.json())
}

async function loadKeyStatus() {
  try {
    keyStatus.value = await fetch('/api/fetcher-key-status').then(r => r.json())
  } catch {}
}

async function confirmKeys() {
  confirming.value = true
  confirmMessage.value = ''
  try {
    const res = await fetch('/api/confirm-fetcher-key', { method: 'POST' }).then(r => r.json())
    if (res.ok) {
      confirmMessage.value = 'Keys confirmed on all fetchers'
    } else {
      const failed = (res.results || []).filter((x: any) => !x.ok).map((x: any) => x.address).join(', ')
      confirmMessage.value = `Some fetchers failed: ${failed || 'unknown'}`
    }
  } catch (e: any) {
    confirmMessage.value = `Error: ${e.message || 'request failed'}`
  } finally {
    confirming.value = false
    await loadKeyStatus()
  }
}

async function loadLogEngine() {
  logEngine.value = await fetch('/api/logengine').then(r => r.json())
}

async function loadRegistryStats() {
  try {
    registryStats.value = await fetch('/api/registry/stats').then(r => r.json())
  } catch {}
}

async function loadStorageObjects() {
  try {
    storageObjects.value = await fetch('/api/fetchers/storage-objects').then(r => r.json())
  } catch {}
}

const { loading: fetchersLoading } = useAutoRefresh(async () => {
  await Promise.allSettled([loadFetchers(), loadKeyStatus(), loadLogEngine(), loadRegistryStats(), loadStorageObjects()])
}, 10_000)

function hitColor(rate: number): string {
  if (rate >= 0.9) return 'text-emerald-400'
  if (rate >= 0.6) return 'text-amber-400'
  return 'text-rose-400'
}

// Backend type metadata
const backendMeta: Record<string, { icon: string; label: string; desc: string }> = {
  git:  { icon: '📁', label: 'Git',                desc: 'Cloned repository objects' },
  blob: { icon: '📦', label: 'Packager Archives',  desc: 'Compacted archive objects' },
  s3:   { icon: '☁️', label: 'S3',                 desc: 'S3-compatible object storage' },
  http: { icon: '🌐', label: 'HTTP',               desc: 'Generic HTTP sources' },
  oci:  { icon: '🐳', label: 'OCI',                desc: 'OCI registry images' },
}

const blobStatsEntries = computed(() => {
  const bs = fetchers.value?.blob_stats
  if (!bs) return []
  const total = Object.values(bs).reduce((s, v) => s + v.blob_bytes, 0)
  return Object.entries(bs).map(([key, val]) => ({
    key,
    ...val,
    meta: backendMeta[key] ?? { icon: '📦', label: key, desc: key },
    pct: total > 0 ? ((val.blob_bytes / total) * 100) : 0,
  }))
})

const storageBlobsEntries = computed(() => {
  const sb = fetchers.value?.storage_blobs
  if (!sb) return []
  return Object.entries(sb).map(([key, val]) => ({ key, ...val }))
})

const totalBlobs = computed(() => {
  const sb = fetchers.value?.storage_blobs
  if (!sb) return 0
  return Object.values(sb).reduce((sum, v) => sum + v.blob_count, 0)
})

const storageUnhealthyFetchers = computed(() => {
  return (fetchers.value?.fetchers ?? []).filter(f => !f.storage_healthy)
})

const anyStorageUnhealthy = computed(() => storageUnhealthyFetchers.value.length > 0)

const pendingKeyFetchers = computed(() => {
  return (keyStatus.value?.fetchers ?? []).filter(f => f.state === 'pending')
})

const anyKeyPending = computed(() => pendingKeyFetchers.value.length > 0)

const storageObjectSearchQuery = ref('')
const storageObjectTypeFilter = ref('')
const storageObjectPageSize = 50
const storageObjectCurrentPage = ref(1)

const storageObjectAllEntries = computed(() => {
  const all: { fetcher: string; object: StorageObject }[] = []
  for (const fetcher of (storageObjects.value?.fetchers ?? [])) {
    if (!fetcher.healthy || !fetcher.objects) continue
    for (const obj of fetcher.objects) {
      all.push({ fetcher: fetcher.address, object: obj })
    }
  }
  return all
})

const storageObjectFilteredEntries = computed(() => {
  if (!storageObjectSearchQuery.value.trim() && !storageObjectTypeFilter.value) return []
  const q = storageObjectSearchQuery.value.trim().toLowerCase()
  const type = storageObjectTypeFilter.value
  let results = storageObjectAllEntries.value
  if (q) {
    results = results.filter(e =>
      e.object.key.toLowerCase().includes(q) ||
      (e.object.bucket || '').toLowerCase().includes(q)
    )
  }
  if (type) {
    results = results.filter(e => e.object.storage_type === type)
  }
  return results
})

const storageObjectTotalPages = computed(() =>
  Math.max(1, Math.ceil(storageObjectFilteredEntries.value.length / storageObjectPageSize))
)

const storageObjectDisplayEntries = computed(() => {
  const start = (storageObjectCurrentPage.value - 1) * storageObjectPageSize
  return storageObjectFilteredEntries.value.slice(start, start + storageObjectPageSize)
})

const storageObjectSearchActive = computed(() =>
  !!storageObjectSearchQuery.value.trim() || !!storageObjectTypeFilter.value
)

function clearStorageSearch() {
  storageObjectSearchQuery.value = ''
  storageObjectTypeFilter.value = ''
  storageObjectCurrentPage.value = 1
}

const storageObjectCount = computed(() => storageObjects.value?.total_objects ?? 0)
const storageObjectsHealthy = computed(() => storageObjects.value?.healthy ?? false)
</script>

<template>
  <div>
    <PageHeader title="Fetchers" subtitle="Blob fetcher services for external data access (DMZ layer)" />

    <!-- Encryption key guard pending banner -->
    <div
      v-if="anyKeyPending"
      class="mb-6 p-4 rounded-xl border bg-amber-900/20 border-amber-500/30 text-amber-100"
    >
      <div class="flex items-start gap-3">
        <span class="text-xl mt-0.5">🔑</span>
        <div class="flex-1 min-w-0">
          <div class="font-semibold text-sm">Encryption key rotation pending</div>
          <div class="text-xs text-amber-300/80 mt-1">
            A fetcher is running with an encryption key that differs from the accepted key fingerprint.
            Blob operations are blocked until the new key is confirmed. If you intentionally rotated the key,
            click Confirm to accept it everywhere.
          </div>
          <div class="mt-2 space-y-1">
            <div
              v-for="f in pendingKeyFetchers"
              :key="f.address"
              class="text-xs font-mono text-amber-300/60"
            >
              {{ f.address }}
              <span v-if="f.key_source">· {{ f.key_source }}</span>
            </div>
          </div>
          <div class="mt-3 flex items-center gap-3">
            <button
              :disabled="confirming"
              @click="confirmKeys"
              class="px-3 py-1.5 text-xs font-semibold rounded-lg bg-amber-500/20 hover:bg-amber-500/30 border border-amber-500/40 text-amber-200 disabled:opacity-50 disabled:cursor-not-allowed"
            >
              {{ confirming ? 'Confirming...' : 'Confirm new key' }}
            </button>
            <span v-if="confirmMessage" class="text-xs text-amber-200/80">{{ confirmMessage }}</span>
          </div>
        </div>
      </div>
    </div>

    <!-- Storage backend unreachable banner -->
    <div
      v-if="anyStorageUnhealthy"
      class="mb-6 p-4 rounded-xl border bg-rose-900/20 border-rose-500/30 text-rose-100"
    >
      <div class="flex items-start gap-3">
        <span class="text-xl mt-0.5">🔴</span>
        <div class="flex-1 min-w-0">
          <div class="font-semibold text-sm">Storage backend unreachable</div>
          <div class="text-xs text-rose-300/80 mt-1">
            The blob storage backend is not accessible by one or more fetchers.
            Blob fetch requests may be retried but could eventually fail.
          </div>
          <div class="mt-2 space-y-1">
            <div
              v-for="f in storageUnhealthyFetchers"
              :key="f.address"
              class="text-xs font-mono text-rose-300/60"
            >
              {{ f.address }}: {{ f.storage_error || 'unreachable' }}
            </div>
          </div>
        </div>
      </div>
    </div>

    <!-- Overview stat cards -->
    <div v-if="fetchers" class="grid grid-cols-2 sm:grid-cols-4 gap-4 mb-6">
      <StatCard
        icon="🔄"
        label="Fetchers"
        :value="`${fetchers.healthy_fetchers ?? 0}/${fetchers.total_fetchers ?? 0}`"
        :sub="`${fetchers.total_fetchers ? (((fetchers.healthy_fetchers ?? 0) / fetchers.total_fetchers) * 100).toFixed(0) : 0}% healthy`"
        tooltip="Servers that retrieve blob content (files, archives, images) from local cache or remote storage like S3 or Git."
      />
      <StatCard icon="📥" label="Total Requests" :value="formatNumber(fetchers.total_requests ?? 0)"
        tooltip="Total blob fetch requests served across all fetchers. Each request retrieves a single file or blob by its content hash." />
      <StatCard icon="💾" label="Cache Hit Rate" :value="`${((fetchers.aggregated_hit_rate ?? 0) * 100).toFixed(1)}%`"
        tooltip="How often a requested blob was already cached locally. Higher is better — it means fewer trips to remote storage." />
      <StatCard icon="📦" label="Data Served" :value="formatBytes(fetchers.total_bytes_served ?? 0)"
        tooltip="Total data sent to clients. Includes both cache hits (fast) and remote fetches (slower)." />
      <StatCard icon="🗃️" label="Backend Blobs" :value="formatNumber(totalBlobs)" :sub="`${formatNumber(storageBlobsEntries.length)} storage IDs`"
        tooltip="Total blob objects stored across all backend types (Git repos, archives, S3 objects, OCI images). The number of distinct storage IDs is shown below." />
      <StatCard icon="🧰" label="Sync Jobs"
        :value="`${fetchers.sync_worker?.active_jobs ?? 0}/${fetchers.sync_worker?.total_jobs ?? 0}`"
        :sub="`${formatNumber(fetchers.sync_worker?.publish_jobs ?? 0)} publish jobs`"
        tooltip="Sync jobs refresh repositories from their upstream remotes (GitHub, GitLab, etc.) and package them into bundles." />
      <StatCard icon="🚀" label="Published Repos" :value="formatNumber(fetchers.sync_worker?.published_repositories ?? 0)"
        tooltip="Repositories that have been successfully packaged into git bundles and are ready to be ingested by the cluster." />
      <StatCard icon="📦" label="Staged Bundles" :value="formatNumber(fetchers.sync_worker?.staged_bundles ?? 0)"
        tooltip="Git bundles that have been built and are waiting to be published to the MetaStore nodes." />
      <StatCard icon="🗂️" label="Worktree Bytes" :value="formatBytes(fetchers.sync_worker?.worktree_bytes ?? 0)"
        tooltip="Disk space used by temporary git worktrees created during bundle publishing. Should be zero when no publishes are active." />
    </div>

    <DataCard v-if="fetchers" class="mb-6" :loading="fetchersLoading">
      <template #header>
        <h2 class="text-sm font-semibold text-slate-200">Sync Worker</h2>
      </template>
      <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4 p-5 text-xs">
        <div class="bg-slate-800/40 rounded-xl border border-slate-700/30 p-4 space-y-1.5" title="Currently running sync jobs. Sync jobs refresh git repositories and build bundles from them.">
          <div class="text-slate-400">Active Jobs</div>
          <div class="text-lg font-semibold text-slate-200">{{ formatNumber(fetchers.sync_worker?.active_jobs ?? 0) }}</div>
          <div class="text-slate-500">{{ formatNumber(fetchers.sync_worker?.completed_jobs ?? 0) }} completed</div>
        </div>
        <div class="bg-slate-800/40 rounded-xl border border-slate-700/30 p-4 space-y-1.5" title="Sync jobs that also publish the result as a git bundle to MetaStore nodes.">
          <div class="text-slate-400">Publishes</div>
          <div class="text-lg font-semibold text-slate-200">{{ formatNumber(fetchers.sync_worker?.publish_jobs ?? 0) }}</div>
          <div class="text-slate-500">{{ formatNumber(fetchers.sync_worker?.published_repositories ?? 0) }} repos pushed</div>
        </div>
        <div class="bg-slate-800/40 rounded-xl border border-slate-700/30 p-4 space-y-1.5" title="Number of git bundles that have been built and are ready to publish. These are compressed snapshots of repository data.">
          <div class="text-slate-400">Bundle Cache</div>
          <div class="text-lg font-semibold text-slate-200">{{ formatNumber(fetchers.sync_worker?.staged_bundles ?? 0) }}</div>
          <div class="text-slate-500">{{ formatBytes(fetchers.sync_worker?.staged_bundle_bytes ?? 0) }}</div>
        </div>
        <div class="bg-slate-800/40 rounded-xl border border-slate-700/30 p-4 space-y-1.5" title="Number of git repositories cached locally. Stage failures happen when a bundle cannot be built from a repo.">
          <div class="text-slate-400">Git Cache</div>
          <div class="text-lg font-semibold text-slate-200">{{ formatNumber(fetchers.sync_worker?.git_cache_entries ?? 0) }}</div>
          <div class="text-slate-500">{{ formatNumber(fetchers.sync_worker?.bundle_stage_failures ?? 0) }} stage failures</div>
        </div>
      </div>
    </DataCard>

    <!-- Object Store Operations -->
    <DataCard v-if="fetchers" class="mb-6" :loading="fetchersLoading">
      <template #header>
        <div>
          <h2 class="text-sm font-semibold text-slate-200">Object Store</h2>
          <p class="text-xs text-slate-400 mt-0.5">S3 / GCS cloud storage operations across all fetchers</p>
        </div>
      </template>
      <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4 p-5 text-xs">
        <div class="bg-slate-800/40 rounded-xl border border-slate-700/30 p-4 space-y-1.5" title="How many archive files have been uploaded to S3 or GCS cloud storage. Each archive can contain many blob objects packed together for efficiency.">
          <div class="text-slate-400">Objects Stored</div>
          <div class="text-lg font-semibold text-emerald-400">{{ formatNumber(fetchers.cloud_objects_stored ?? 0) }}</div>
          <div class="text-slate-500">Archives uploaded to cloud</div>
        </div>
        <div class="bg-slate-800/40 rounded-xl border border-slate-700/30 p-4 space-y-1.5" title="How many archive files were downloaded from cloud storage. This happens when a blob is needed but not found in the local cache on disk.">
          <div class="text-slate-400">Objects Retrieved</div>
          <div class="text-lg font-semibold text-sky-400">{{ formatNumber(fetchers.cloud_objects_retrieved ?? 0) }}</div>
          <div class="text-slate-500">Archives downloaded from cloud</div>
        </div>
      </div>
    </DataCard>

    <!-- Backend Storage Objects (S3 / GCS / local) -->
    <DataCard v-if="storageObjects" class="mb-6" :loading="fetchersLoading">
      <template #header>
        <div>
          <div class="flex items-center justify-between mb-3">
            <div>
              <h2 class="text-sm font-semibold text-slate-200">Backend Storage Objects</h2>
              <p class="text-xs text-slate-400 mt-0.5">Objects stored in the configured backend (S3 / GCS / local)</p>
            </div>
            <div class="flex items-center gap-2">
              <span class="text-xs px-2 py-1 rounded-lg border" :class="storageObjectsHealthy ? 'border-emerald-500/30 bg-emerald-500/10 text-emerald-300' : 'border-rose-500/30 bg-rose-500/10 text-rose-300'">
                {{ storageObjectsHealthy ? 'Reachable' : 'Unreachable' }}
              </span>
              <span class="text-xs text-slate-400">{{ formatNumber(storageObjectCount) }} objects</span>
            </div>
          </div>
          <div class="flex items-center gap-2">
            <div class="relative flex-1 max-w-xs">
              <input
                v-model.trim="storageObjectSearchQuery"
                type="text"
                placeholder="Search by key or bucket name..."
                class="w-full bg-slate-800/60 border border-slate-600/40 rounded-lg px-3 py-1.5 pr-8 text-xs text-slate-200 placeholder-slate-500 focus:outline-none focus:border-sky-500/50 focus:ring-1 focus:ring-sky-500/30"
                @input="storageObjectCurrentPage = 1"
              />
              <svg v-if="storageObjectSearchQuery" @click="clearStorageSearch" class="absolute right-2 top-1/2 -translate-y-1/2 w-3.5 h-3.5 text-slate-500 hover:text-slate-300 cursor-pointer" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                <path d="M18 6L6 18M6 6l12 12"/>
              </svg>
            </div>
            <select
              v-model="storageObjectTypeFilter"
              class="bg-slate-800/60 border border-slate-600/40 rounded-lg px-2.5 py-1.5 text-xs text-slate-300 focus:outline-none focus:border-sky-500/50"
              @change="storageObjectCurrentPage = 1"
            >
              <option value="">All types</option>
              <option value="s3">S3</option>
              <option value="gcs">GCS</option>
              <option value="local">Local</option>
            </select>
          </div>
        </div>
      </template>
      <div v-if="!storageObjectSearchActive" class="px-6 py-12 text-center">
        <div class="text-slate-400 text-xs">
          {{ formatNumber(storageObjectCount) }} objects available. Use the search above to find specific objects.
        </div>
      </div>
      <div v-else-if="!storageObjectFilteredEntries.length" class="px-6 py-12 text-center">
        <div class="text-slate-500 text-xs">No objects match your search.</div>
      </div>
      <div v-else class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-slate-700/40 text-xs text-slate-400 uppercase tracking-wider">
              <th class="text-left px-6 py-3 font-medium">Storage Type</th>
              <th class="text-left px-6 py-3 font-medium">Bucket</th>
              <th class="text-left px-6 py-3 font-medium">Object Key</th>
              <th class="text-right px-6 py-3 font-medium">Size</th>
              <th class="text-right px-6 py-3 font-medium">Last Modified</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-700/20">
            <tr v-for="entry in storageObjectDisplayEntries" :key="`${entry.fetcher}-${entry.object.key}`" class="hover:bg-slate-800/30 transition-colors">
              <td class="px-6 py-3 text-slate-300">
                <span class="inline-flex items-center gap-1.5">
                  <span v-if="entry.object.storage_type === 's3'">☁️</span>
                  <span v-else-if="entry.object.storage_type === 'gcs'">☁️</span>
                  <span v-else>💾</span>
                  <span class="text-xs uppercase tracking-wide">{{ entry.object.storage_type }}</span>
                </span>
              </td>
              <td class="px-6 py-3 text-slate-400 font-mono text-xs">{{ entry.object.bucket || '-' }}</td>
              <td class="px-6 py-3 text-slate-300 font-mono text-xs truncate max-w-[300px]" :title="entry.object.key">{{ entry.object.key }}</td>
              <td class="px-6 py-3 text-right text-slate-300">{{ formatBytes(entry.object.size) }}</td>
              <td class="px-6 py-3 text-right text-slate-400 text-xs">{{ entry.object.last_modified ? new Date(entry.object.last_modified * 1000).toLocaleString() : '-' }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <div v-if="storageObjectSearchActive && storageObjectFilteredEntries.length > 0" class="px-6 py-2 border-t border-slate-700/40 flex items-center justify-between text-xs text-slate-400">
        <span>{{ formatNumber(storageObjectFilteredEntries.length) }} results<span v-if="storageObjectFilteredEntries.length > storageObjectPageSize"> &mdash; page {{ storageObjectCurrentPage }} of {{ storageObjectTotalPages }}</span></span>
        <div v-if="storageObjectTotalPages > 1" class="flex items-center gap-1">
          <button
            @click="storageObjectCurrentPage = Math.max(1, storageObjectCurrentPage - 1)"
            :disabled="storageObjectCurrentPage <= 1"
            class="px-2 py-1 rounded-md border border-slate-600/40 disabled:opacity-30 disabled:cursor-not-allowed hover:bg-slate-700/50 transition-colors"
          >Prev</button>
          <span class="px-2 text-slate-500">{{ storageObjectCurrentPage }} / {{ storageObjectTotalPages }}</span>
          <button
            @click="storageObjectCurrentPage = Math.min(storageObjectTotalPages, storageObjectCurrentPage + 1)"
            :disabled="storageObjectCurrentPage >= storageObjectTotalPages"
            class="px-2 py-1 rounded-md border border-slate-600/40 disabled:opacity-30 disabled:cursor-not-allowed hover:bg-slate-700/50 transition-colors"
          >Next</button>
        </div>
        <div v-else class="text-slate-500">
          Source: {{ storageObjects.fetchers.length }} fetcher diagnostics endpoint(s)
        </div>
      </div>
    </DataCard>

    <!-- Archive Storage by backend type -->
    <DataCard v-if="blobStatsEntries.length" class="mb-6" :loading="fetchersLoading">
      <template #header>
        <h2 class="text-sm font-semibold text-slate-200">Archive Storage</h2>
      </template>
      <div class="flex flex-col lg:flex-row gap-4 p-5">
        <div class="flex-1 grid grid-cols-1 sm:grid-cols-2 gap-4">
          <div
            v-for="entry in blobStatsEntries"
            :key="entry.key"
            class="bg-slate-800/40 rounded-xl border border-slate-700/30 p-4"
          >
            <div class="flex items-center gap-2 mb-2">
              <span class="text-xl">{{ entry.meta.icon }}</span>
              <div>
                <div class="text-sm font-semibold text-slate-200">{{ entry.meta.label }}</div>
                <div class="text-xs text-slate-500">{{ entry.meta.desc }}</div>
              </div>
            </div>
            <div class="text-xs space-y-1 mt-3">
              <div class="flex justify-between">
                <span class="text-slate-400">Archives</span>
                <span class="text-slate-200 font-medium">{{ formatNumber(entry.blob_count) }}</span>
              </div>
              <div class="flex justify-between">
                <span class="text-slate-400">Size</span>
                <span class="text-slate-200 font-medium">{{ formatBytes(entry.blob_bytes) }}</span>
              </div>
              <div class="flex justify-between">
                <span class="text-slate-400">Share</span>
                <span class="text-slate-200 font-medium">{{ entry.pct.toFixed(1) }}%</span>
              </div>
            </div>
            <div class="mt-2 h-1 bg-slate-700 rounded-full overflow-hidden">
              <div class="h-full bg-violet-500 rounded-full" :style="{ width: `${entry.pct}%` }"></div>
            </div>
          </div>
        </div>

        <!-- Registry Data Usage -->
        <div v-if="registryStats" class="lg:w-64 shrink-0 bg-violet-900/20 rounded-xl border border-violet-700/30 p-4 h-fit">
          <div class="flex items-center gap-2 mb-3">
            <span class="text-xl">🐳</span>
            <div>
              <div class="text-sm font-semibold text-slate-200">Registry Data</div>
              <div class="text-xs text-slate-500">OCI blob storage on disk</div>
            </div>
          </div>
          <div class="text-xs space-y-1.5 mt-3">
            <div class="flex justify-between">
              <span class="text-slate-400">Blobs</span>
              <span class="text-slate-200 font-medium">{{ formatNumber(registryStats.blob_count) }}</span>
            </div>
            <div class="flex justify-between">
              <span class="text-slate-400">On Disk</span>
              <span class="text-slate-200 font-medium">{{ formatBytes(registryStats.bytes_stored) }}</span>
            </div>
            <div class="flex justify-between">
              <span class="text-slate-400">Fetched</span>
              <span class="text-slate-200 font-medium">{{ formatBytes(registryStats.bytes_fetched) }}</span>
            </div>
            <div class="flex justify-between">
              <span class="text-slate-400">Served</span>
              <span class="text-slate-200 font-medium">{{ formatBytes(registryStats.bytes_served) }}</span>
            </div>
          </div>
          <div class="mt-3 pt-3 border-t border-violet-700/30 text-xs">
            <div class="flex justify-between text-slate-500">
              <span>Pulls / Pushes</span>
              <span class="text-slate-400">{{ formatNumber(registryStats.pulls) }} / {{ formatNumber(registryStats.pushes) }}</span>
            </div>
          </div>
        </div>
      </div>

      <!-- Per-dependency file breakdown -->
      <div v-if="storageBlobsEntries.length" class="border-t border-slate-700/40 px-6 py-4">
        <h3 class="text-sm font-semibold text-slate-300 mb-3">Per-Dependency Files</h3>
        <table class="w-full text-sm">
          <thead>
            <tr class="text-xs text-slate-400 uppercase tracking-wider border-b border-slate-700/40">
              <th class="text-left py-2 font-medium">Key</th>
              <th class="text-right py-2 font-medium">Blobs</th>
              <th class="text-right py-2 font-medium">Size</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-700/20">
            <tr v-for="entry in storageBlobsEntries" :key="entry.key" class="hover:bg-slate-800/30">
              <td class="py-2 font-mono text-xs text-slate-400">{{ entry.key }}</td>
              <td class="py-2 text-right text-slate-300">{{ formatNumber(entry.blob_count) }}</td>
              <td class="py-2 text-right text-slate-300">{{ formatBytes(entry.blob_bytes) }}</td>
            </tr>
            <tr class="border-t border-slate-600/40 bg-slate-800/20 font-semibold">
              <td class="py-2 text-slate-300">Total</td>
              <td class="py-2 text-right text-slate-200">{{ formatNumber(totalBlobs) }}</td>
              <td class="py-2 text-right text-slate-200">-</td>
            </tr>
          </tbody>
        </table>
      </div>
    </DataCard>

    <!-- Log Store / Doctor telemetry -->
    <DataCard v-if="logEngine?.nodes?.length" class="mb-6">
      <template #header>
        <div>
          <h2 class="text-sm font-semibold text-slate-200">Log Store</h2>
          <p class="text-xs text-slate-400 mt-0.5">Doctor telemetry engine — per-node chunk counts</p>
        </div>
      </template>
      <div class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-slate-700/40 text-xs text-slate-400 uppercase tracking-wider">
              <th class="text-left px-6 py-3 font-medium">Node</th>
              <th class="text-right px-6 py-3 font-medium" title="Number of log data chunks stored on this Doctor node.">Logs</th>
              <th class="text-right px-6 py-3 font-medium" title="Number of metrics data chunks stored on this Doctor node.">Metrics</th>
              <th class="text-right px-6 py-3 font-medium" title="Number of trace data chunks stored on this Doctor node.">Traces</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-700/20">
            <tr v-for="n in logEngine.nodes" :key="n.address" class="hover:bg-slate-800/30 transition-colors">
              <td class="px-6 py-3 font-mono text-xs text-slate-300">{{ n.node_id || n.address }}</td>
              <td class="px-6 py-3 text-right text-slate-300">{{ formatNumber(n.log_chunks) }}</td>
              <td class="px-6 py-3 text-right text-slate-300">{{ formatNumber(n.metric_chunks) }}</td>
              <td class="px-6 py-3 text-right text-slate-300">{{ formatNumber(n.trace_chunks) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </DataCard>

    <!-- Fetcher instances -->
    <DataCard :loading="fetchersLoading" class="mb-6">
      <template #header>
        <div class="flex items-center justify-between">
          <h2 class="text-sm font-semibold text-slate-200">Fetcher Instances</h2>
          <button
            @click="loadFetchers"
            class="text-xs text-violet-400 hover:text-violet-300 transition-colors px-3 py-1 rounded hover:bg-violet-500/10"
          >🔄 Refresh</button>
        </div>
      </template>
      <div v-if="fetchers?.fetchers?.length" class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4 p-5">
        <div
          v-for="f in fetchers.fetchers"
          :key="f.address"
          class="bg-slate-800/40 rounded-xl border p-4"
          :class="f.healthy ? 'border-slate-700/30' : 'border-rose-500/20'"
        >
          <div class="flex items-center justify-between mb-3">
            <div class="font-mono text-xs text-slate-300 truncate flex-1 mr-2">{{ f.address }}</div>
            <NodeBadge :healthy="f.healthy" />
          </div>
          <div class="grid grid-cols-2 gap-2 text-xs">
            <div class="bg-slate-900/40 rounded-lg p-2 text-center" title="Total blob requests this specific fetcher has handled.">
              <div class="text-slate-400 mb-1">Requests</div>
              <div class="font-semibold text-slate-200">{{ formatNumber(f.total_requests) }}</div>
            </div>
            <div class="bg-slate-900/40 rounded-lg p-2 text-center" title="How often this fetcher found the blob in its own local cache. Higher means faster responses.">
              <div class="text-slate-400 mb-1">Cache Rate</div>
              <div class="font-semibold" :class="hitColor(f.cache_hit_rate)">
                {{ ((f.cache_hit_rate || 0) * 100).toFixed(1) }}%
              </div>
            </div>
            <div class="bg-slate-900/40 rounded-lg p-2 text-center col-span-2" title="Total data this fetcher has sent back to clients.">
              <div class="text-slate-400 mb-1">Bytes Served</div>
              <div class="font-semibold text-slate-200">{{ formatBytes(f.bytes_served) }}</div>
            </div>
            <div class="bg-slate-900/40 rounded-lg p-2 text-center" title="Sync jobs currently running on this fetcher.">
              <div class="text-slate-400 mb-1">Sync Jobs</div>
              <div class="font-semibold text-slate-200">{{ formatNumber(f.sync_worker?.active_jobs ?? 0) }}</div>
            </div>
            <div class="bg-slate-900/40 rounded-lg p-2 text-center" title="Repositories this fetcher has published as git bundles.">
              <div class="text-slate-400 mb-1">Published</div>
              <div class="font-semibold text-slate-200">{{ formatNumber(f.sync_worker?.published_repositories ?? 0) }}</div>
            </div>
          </div>
        </div>
      </div>
      <div v-else class="py-12 text-center text-slate-400 text-sm">No fetcher instances available</div>
    </DataCard>

    <!-- Stats table -->
    <DataCard :loading="fetchersLoading">
      <template #header>
        <div class="flex items-center justify-between">
          <h2 class="text-sm font-semibold text-slate-200">Statistics</h2>
          <label class="flex items-center gap-2 text-sm text-slate-400 cursor-pointer select-none" title="Show a per-source breakdown below for each fetcher instance (git, blob, s3, oci, etc.).">
            <input
              type="checkbox"
              v-model="detailed"
              @change="loadFetchers"
              class="rounded border-slate-600 bg-slate-800 text-violet-500"
            />
            Per-source stats
          </label>
        </div>
      </template>
      <div v-if="fetchers?.fetchers?.length" class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-slate-700/40 text-xs text-slate-400 uppercase tracking-wider">
              <th class="text-left px-6 py-3 font-medium">Fetcher</th>
              <th class="text-right px-6 py-3 font-medium" title="Total blob fetch requests handled.">Requests</th>
              <th class="text-right px-6 py-3 font-medium" title="Requests served from local cache (fast).">Hits</th>
              <th class="text-right px-6 py-3 font-medium" title="Requests that missed the cache and needed a remote fetch.">Misses</th>
              <th class="text-right px-6 py-3 font-medium" title="Percentage of requests served from cache. Hits / (Hits + Misses).">Cache Rate</th>
              <th class="text-right px-6 py-3 font-medium" title="Total data sent to clients.">Bytes</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-700/20">
            <tr v-for="f in fetchers.fetchers" :key="f.address" class="hover:bg-slate-800/30 transition-colors">
              <td class="px-6 py-3 font-mono text-xs text-slate-300">{{ f.address }}</td>
              <td class="px-6 py-3 text-right text-slate-300">{{ formatNumber(f.total_requests) }}</td>
              <td class="px-6 py-3 text-right text-emerald-400">{{ formatNumber(f.cache_hits) }}</td>
              <td class="px-6 py-3 text-right text-slate-400">{{ formatNumber(f.cache_misses) }}</td>
              <td class="px-6 py-3 text-right" :class="hitColor(f.cache_hit_rate)">
                {{ ((f.cache_hit_rate || 0) * 100).toFixed(1) }}%
              </td>
              <td class="px-6 py-3 text-right text-slate-300">{{ formatBytes(f.bytes_served) }}</td>
            </tr>
            <!-- Totals row -->
            <tr class="border-t border-slate-600/40 bg-slate-800/20 font-semibold">
              <td class="px-6 py-3 text-slate-300">Total</td>
              <td class="px-6 py-3 text-right text-slate-200">{{ formatNumber(fetchers.total_requests ?? 0) }}</td>
              <td class="px-6 py-3 text-right text-emerald-400">{{ formatNumber(fetchers.total_cache_hits ?? 0) }}</td>
              <td class="px-6 py-3 text-right text-slate-400">{{ formatNumber(fetchers.total_cache_misses ?? 0) }}</td>
              <td class="px-6 py-3 text-right" :class="hitColor(fetchers.aggregated_hit_rate ?? 0)">
                {{ ((fetchers.aggregated_hit_rate ?? 0) * 100).toFixed(1) }}%
              </td>
              <td class="px-6 py-3 text-right text-slate-200">{{ formatBytes(fetchers.total_bytes_served ?? 0) }}</td>
            </tr>
          </tbody>
        </table>

        <!-- Per-source breakdown (only shown when detailed mode + fetcher has source_stats) -->
        <template v-if="detailed">
          <div
            v-for="f in fetchers.fetchers?.filter(f => f.source_stats && Object.keys(f.source_stats).length)"
            :key="`src-${f.address}`"
            class="border-t border-slate-700/40 px-6 py-5"
          >
            <h3 class="text-sm font-semibold text-slate-300 mb-3">Per-Source: {{ f.address }}</h3>
            <table class="w-full text-sm">
              <thead>
                <tr class="text-xs text-slate-400 uppercase tracking-wider">
                  <th class="text-left py-2 font-medium">Source</th>
                  <th class="text-right py-2 font-medium" title="Requests to this specific backend type.">Requests</th>
                  <th class="text-right py-2 font-medium" title="Fetch failures from this backend.">Errors</th>
                  <th class="text-right py-2 font-medium" title="Data fetched from this backend (from remote, not cache).">Bytes Fetched</th>
                  <th class="text-right py-2 font-medium" title="Average time a fetch from this backend takes.">Avg Latency</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-slate-700/20">
                <tr v-for="(s, srcKey) in f.source_stats" :key="srcKey" class="hover:bg-slate-800/30">
                  <td class="py-2 font-mono text-xs text-slate-400 truncate max-w-[200px]">{{ srcKey }}</td>
                  <td class="py-2 text-right text-slate-300">{{ formatNumber(s.requests) }}</td>
                  <td class="py-2 text-right" :class="s.errors > 0 ? 'text-rose-400' : 'text-slate-400'">{{ formatNumber(s.errors) }}</td>
                  <td class="py-2 text-right text-slate-300">{{ formatBytes(s.bytes_fetched) }}</td>
                  <td class="py-2 text-right text-slate-300">{{ s.avg_latency_ms?.toFixed(1) ?? '-' }}ms</td>
                </tr>
              </tbody>
            </table>
          </div>
        </template>
      </div>
      <div v-else class="py-12 text-center text-slate-400 text-sm">No stats available</div>
    </DataCard>
  </div>
</template>
