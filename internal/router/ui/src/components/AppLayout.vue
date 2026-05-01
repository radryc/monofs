<script setup lang="ts">
import { onMounted } from 'vue'
import { RouterView, RouterLink, useRoute } from 'vue-router'
import { useAppStore } from '../stores/app'
import ToastContainer from './ToastContainer.vue'
import {
  LayoutDashboard, Server, Users, Zap, GitBranch,
  BookOpen, Upload, Search, Layers, Download, Package
} from 'lucide-vue-next'

const route = useRoute()
const store = useAppStore()

onMounted(async () => {
  if (store.version) return
  try {
    const data = await fetch('/api/status').then(r => r.json())
    if (data?.version?.version) store.setVersion(data.version.version)
  } catch {}
})

const navItems = [
  { to: '/',             icon: LayoutDashboard, label: 'Dashboard' },
  { to: '/cluster',      icon: Server,          label: 'MetaStores' },
  { to: '/clients',      icon: Users,           label: 'Clients' },
  { to: '/performance',  icon: Zap,             label: 'Performance' },
  { to: '/replication',  icon: GitBranch,       label: 'Replication' },
  { to: '/repositories', icon: BookOpen,        label: 'Repositories' },
  { to: '/ingest',       icon: Upload,          label: 'Ingest' },
  { to: '/search',       icon: Search,          label: 'Search' },
  { to: '/indexer',      icon: Layers,          label: 'Indexer' },
  { to: '/fetchers',     icon: Download,        label: 'Fetchers' },
  { to: '/dependencies', icon: Package,         label: 'Dependencies' },
]
</script>

<template>
  <div class="flex h-full min-h-screen">
    <!-- Sidebar -->
    <aside class="w-64 shrink-0 fixed top-0 left-0 h-full flex flex-col bg-slate-900/85 backdrop-blur-xl border-r border-slate-700/40 overflow-y-auto z-40">
      <!-- Logo -->
      <div class="px-5 py-5 border-b border-slate-700/40">
        <div class="flex items-center gap-3">
          <img :src="'/static/static/monofs.png'" alt="MonoFS" class="h-10 w-auto">
          <div>
            <span class="text-sm font-bold text-slate-200 tracking-widest">MONOFS</span>
            <div class="text-xs text-slate-500 mt-0.5">v{{ store.version || '...' }}</div>
          </div>
        </div>
      </div>

      <!-- Nav -->
      <nav class="flex-1 px-3 py-4 space-y-0.5">
        <RouterLink
          v-for="item in navItems"
          :key="item.to"
          :to="item.to"
          class="flex items-center gap-3 px-3 py-2.5 rounded-lg text-sm font-medium transition-all duration-150"
          :class="route.path === item.to
            ? 'bg-violet-600/20 text-violet-300 border border-violet-500/30'
            : 'text-slate-400 hover:text-slate-200 hover:bg-slate-800/50'"
        >
          <component :is="item.icon" class="w-4 h-4 shrink-0" />
          {{ item.label }}
        </RouterLink>
      </nav>

      <!-- Footer -->
      <div class="px-5 py-4 border-t border-slate-700/40">
        <div class="text-xs text-slate-500">
          <div>MonoFS Router</div>
        </div>
      </div>
    </aside>

    <!-- Main content -->
    <main class="flex-1 ml-64 min-h-screen">
      <div class="p-6 lg:p-8 max-w-[1600px]">
        <RouterView />
      </div>
    </main>

    <ToastContainer />
  </div>
</template>
