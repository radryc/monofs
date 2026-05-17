<script setup lang="ts">
import { ref, computed } from 'vue'
import { useAppStore } from '../stores/app'
import { useAutoRefresh, timeAgoUnix, formatNumber, formatBytes } from '../composables/useAutoRefresh'
import PageHeader from '../components/PageHeader.vue'
import DataCard from '../components/DataCard.vue'
import type { ListClientsResponse, ListGuardianClientsResponse, WhitelistData } from '../types/api'

const store = useAppStore()
const activeTab = ref<'fuse' | 'guardian' | 'whitelist'>('fuse')

const clients = ref<ListClientsResponse | null>(null)
const guardianClients = ref<ListGuardianClientsResponse | null>(null)
const whitelist = ref<WhitelistData | null>(null)
const newClientId = ref('')

async function loadAll() {
  const [cRes, gRes, wRes] = await Promise.allSettled([
    fetch('/api/clients').then(r => r.json()),
    fetch('/api/guardian/clients').then(r => r.json()),
    fetch('/api/whitelist').then(r => r.json()),
  ])
  if (cRes.status === 'fulfilled') clients.value = cRes.value
  if (gRes.status === 'fulfilled') guardianClients.value = gRes.value
  if (wRes.status === 'fulfilled') whitelist.value = wRes.value
}

const { loading } = useAutoRefresh(loadAll, 10_000)

// FUSE clients are non-guardian (guardian-* clients register here too but belong in guardian tab)
const fuseClients = computed(() =>
  (clients.value?.clients ?? []).filter(c => !c.client_id.startsWith('guardian-'))
)

async function toggleWhitelist() {
  if (!whitelist.value) return
  try {
    await fetch('/api/whitelist/toggle', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ enabled: !whitelist.value.enabled }),
    })
    await loadAll()
  } catch (e) {
    store.addToast('error', 'Failed to toggle whitelist')
  }
}

async function addWhitelistClient() {
  const id = newClientId.value.trim()
  if (!id) return
  try {
    await fetch('/api/whitelist', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ client_id: id }),
    })
    newClientId.value = ''
    await loadAll()
    store.addToast('success', `Added ${id} to whitelist`)
  } catch {
    store.addToast('error', 'Failed to add client')
  }
}

async function removeWhitelistClient(clientId: string) {
  try {
    await fetch('/api/whitelist', {
      method: 'DELETE',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ client_id: clientId }),
    })
    await loadAll()
    store.addToast('success', `Removed ${clientId}`)
  } catch {
    store.addToast('error', 'Failed to remove client')
  }
}
</script>

<template>
  <div>
    <PageHeader title="Clients" subtitle="Connected FUSE clients, Guardian clients, and ingestion whitelist" />

    <!-- Tabs -->
    <div class="flex gap-1 mb-6 bg-slate-900/40 rounded-xl p-1 w-fit border border-slate-700/40">
      <button
        v-for="tab in [['fuse', 'FUSE Clients'], ['guardian', 'Guardian Clients'], ['whitelist', 'Whitelist']] as const"
        :key="tab[0]"
        @click="activeTab = tab[0]"
        class="px-4 py-2 rounded-lg text-sm font-medium transition-all"
        :class="activeTab === tab[0]
          ? 'bg-violet-600/20 text-violet-300 border border-violet-500/30'
          : 'text-slate-400 hover:text-slate-200'"
      >
        {{ tab[1] }}
        <span v-if="tab[0] === 'fuse' && clients" class="ml-1.5 text-xs opacity-70">{{ fuseClients.length }}</span>
        <span v-if="tab[0] === 'guardian' && guardianClients" class="ml-1.5 text-xs opacity-70">{{ guardianClients.guardian_clients?.length ?? 0 }}</span>
      </button>
    </div>

    <!-- FUSE Clients -->
    <DataCard v-if="activeTab === 'fuse'" :loading="loading">
      <template #header>
        <h2 class="text-sm font-semibold text-slate-200">
          FUSE Clients
          <span v-if="clients" class="ml-2 text-xs font-normal text-slate-400">{{ fuseClients.length }} connected</span>
        </h2>
      </template>
      <div v-if="fuseClients.length" class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-slate-700/40 text-xs text-slate-400 uppercase tracking-wider">
              <th class="text-left px-6 py-3 font-medium">Client ID</th>
              <th class="text-left px-6 py-3 font-medium">Hostname</th>
              <th class="text-left px-6 py-3 font-medium">Mount Point</th>
              <th class="text-right px-6 py-3 font-medium">Operations</th>
              <th class="text-right px-6 py-3 font-medium">Bytes Read</th>
              <th class="text-right px-6 py-3 font-medium">Errors</th>
              <th class="text-right px-6 py-3 font-medium">Last Seen</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-700/20">
            <tr v-for="c in fuseClients" :key="c.client_id" class="hover:bg-slate-800/30 transition-colors">
              <td class="px-6 py-3 font-mono text-xs text-violet-300">{{ c.client_id }}</td>
              <td class="px-6 py-3 text-slate-300 text-xs">{{ c.hostname || '-' }}</td>
              <td class="px-6 py-3 font-mono text-xs text-slate-400">{{ c.mount_point || '-' }}</td>
              <td class="px-6 py-3 text-right text-slate-300">{{ formatNumber(c.operations_count ?? 0) }}</td>
              <td class="px-6 py-3 text-right text-slate-300">{{ formatBytes(c.bytes_read ?? 0) }}</td>
              <td class="px-6 py-3 text-right" :class="(c.errors_count ?? 0) > 0 ? 'text-rose-400' : 'text-slate-400'">{{ c.errors_count ?? 0 }}</td>
              <td class="px-6 py-3 text-right text-xs text-slate-400">{{ timeAgoUnix(c.last_heartbeat) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <div v-else class="py-12 text-center text-slate-400 text-sm">No FUSE clients connected</div>
    </DataCard>

    <!-- Guardian Clients -->
    <DataCard v-if="activeTab === 'guardian'" :loading="loading">
      <template #header>
        <h2 class="text-sm font-semibold text-slate-200">Guardian Clients</h2>
      </template>
      <div v-if="guardianClients?.guardian_clients?.length" class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-slate-700/40 text-xs text-slate-400 uppercase tracking-wider">
              <th class="text-left px-6 py-3 font-medium">Client ID</th>
              <th class="text-left px-6 py-3 font-medium">Base URL</th>
              <th class="text-left px-6 py-3 font-medium">Router</th>
              <th class="text-left px-6 py-3 font-medium">State</th>
              <th class="text-right px-6 py-3 font-medium">Last Seen</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-700/20">
            <tr v-for="c in guardianClients.guardian_clients" :key="c.client_id" class="hover:bg-slate-800/30 transition-colors">
              <td class="px-6 py-3 font-mono text-xs text-violet-300">{{ c.client_id }}</td>
              <td class="px-6 py-3 text-xs text-slate-400 break-all">{{ c.base_url || '-' }}</td>
              <td class="px-6 py-3 text-xs text-slate-400">{{ c.router || '-' }}</td>
              <td class="px-6 py-3">
                <span class="px-2 py-0.5 rounded-full text-xs font-medium"
                  :class="c.state === 'connected' ? 'bg-emerald-500/10 text-emerald-400' : 'bg-amber-500/10 text-amber-400'">
                  {{ c.state }}
                </span>
              </td>
              <td class="px-6 py-3 text-right text-xs text-slate-400">{{ timeAgoUnix(c.last_heartbeat) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <div v-else class="py-12 text-center text-slate-400 text-sm">No Guardian clients connected</div>
    </DataCard>

    <!-- Whitelist -->
    <DataCard v-if="activeTab === 'whitelist'" :loading="loading">
      <template #header>
        <div class="flex items-center justify-between gap-4 flex-wrap">
          <div>
            <h2 class="text-sm font-semibold text-slate-200">Ingestion Whitelist</h2>
            <p class="text-xs text-slate-400 mt-0.5">When enabled, only whitelisted clients can ingest data</p>
          </div>
          <button
            @click="toggleWhitelist"
            class="px-4 py-2 rounded-lg text-sm font-medium border transition-all"
            :class="whitelist?.enabled
              ? 'bg-emerald-500/10 text-emerald-400 border-emerald-500/30 hover:bg-emerald-500/20'
              : 'bg-slate-700/30 text-slate-400 border-slate-600/40 hover:bg-slate-700/50'"
          >
            {{ whitelist?.enabled ? '✓ Enabled' : 'Disabled' }}
          </button>
        </div>
      </template>

      <div class="px-6 py-5 border-b border-slate-700/30">
        <div class="flex gap-3">
          <input
            v-model="newClientId"
            type="text"
            placeholder="Client ID to whitelist..."
            class="flex-1 bg-slate-800/60 border border-slate-700/40 rounded-lg px-4 py-2.5 text-sm text-slate-200 placeholder-slate-500 focus:outline-none focus:border-violet-500/60 focus:ring-1 focus:ring-violet-500/20"
            @keydown.enter="addWhitelistClient"
          />
          <button
            @click="addWhitelistClient"
            class="px-4 py-2.5 rounded-lg bg-violet-600/20 text-violet-300 border border-violet-500/30 text-sm font-medium hover:bg-violet-600/30 transition-all"
          >
            Add
          </button>
        </div>
      </div>

      <div v-if="whitelist?.clients?.length" class="divide-y divide-slate-700/20">
        <div v-for="entry in whitelist.clients" :key="entry.client_id" class="flex items-center justify-between px-6 py-3">
          <div>
            <span class="font-mono text-sm text-slate-300">{{ entry.client_id }}</span>
            <span v-if="entry.label" class="ml-2 text-xs text-slate-500">{{ entry.label }}</span>
          </div>
          <button @click="removeWhitelistClient(entry.client_id)" class="text-xs text-rose-400 hover:text-rose-300 transition-colors px-3 py-1 rounded hover:bg-rose-500/10">
            Remove
          </button>
        </div>
      </div>
      <div v-else class="py-10 text-center text-slate-400 text-sm">No whitelisted clients</div>
    </DataCard>
  </div>
</template>
