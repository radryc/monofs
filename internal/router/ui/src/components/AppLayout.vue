<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { RouterView, RouterLink, useRoute } from 'vue-router'
import { useAppStore } from '../stores/app'
import ToastContainer from './ToastContainer.vue'
import ThemePicker from './ThemePicker.vue'
import CommandPalette from './CommandPalette.vue'
import AskDrawer from './AskDrawer.vue'
import {
  LayoutDashboard, Server, Users, Zap, GitBranch,
  BookOpen, Upload, Search, Layers, Download, Package, Container, SlidersHorizontal,
  Cpu, Menu, Play, Activity
} from 'lucide-vue-next'

const route = useRoute()
const store = useAppStore()
const pageTitle = computed(() => (route.meta.title as string | undefined) ?? 'Dashboard')
const logoSrc = import.meta.env.BASE_URL + 'static/static/monofs.png'
const currentUser = ref('')
const sidebarOpen = ref(false)

watch(() => route.path, () => { sidebarOpen.value = false })

onMounted(async () => {
  if (store.version) return
  try {
    const data = await fetch('/api/status').then(r => r.json())
    if (data?.version?.version) store.setVersion(data.version.version)
  } catch {}
})

onMounted(async () => {
  try {
    const res = await fetch('/api/auth/me')
    if (!res.ok) return
    const user = await res.json()
    currentUser.value = user.display || user.email || user.subject || user.client_id || ''
  } catch {}
})

const navItems = [
  { to: '/',             icon: LayoutDashboard, label: 'Dashboard' },
  { to: '/cicd',         icon: SlidersHorizontal, label: 'CI/CD' },
  { to: '/pipelines',    icon: Play,            label: 'Pipelines' },
  { to: '/bazel',        icon: Cpu,             label: 'Bazel' },
  { to: '/cluster',      icon: Server,          label: 'MetaStores' },
  { to: '/clients',      icon: Users,           label: 'Clients' },
  { to: '/performance',  icon: Zap,             label: 'Performance' },
  { to: '/replication',  icon: GitBranch,       label: 'Replication' },
  { to: '/repositories', icon: BookOpen,        label: 'Repositories' },
  { to: '/ingest',       icon: Upload,          label: 'Ingest' },
  { to: '/search',       icon: Search,          label: 'Search' },
  { to: '/indexer',      icon: Layers,          label: 'Indexer' },
  { to: '/fetchers',     icon: Download,        label: 'Fetchers' },
  { to: '/span-engine', icon: Activity,        label: 'Span engine' },
  { to: '/dependencies', icon: Package,         label: 'Dependencies' },
  { to: '/registry',     icon: Container,       label: 'Registry' },
]
</script>

<template>
  <div class="flex h-full min-h-screen">
    <a class="s-skip-link" href="#main-content">Skip to content</a>
    <!-- Sidebar -->
    <div v-if="sidebarOpen" class="fixed inset-0 z-30 bg-black/50 lg:hidden" @click="sidebarOpen = false" />
    <aside
      class="w-[236px] shrink-0 fixed top-0 left-0 h-full flex flex-col bg-slate-900/85 backdrop-blur-xl border-r border-slate-700/40 overflow-y-auto z-40 transition-transform duration-200 lg:translate-x-0"
      :class="sidebarOpen ? 'translate-x-0' : '-translate-x-full'"
    >
      <!-- Logo -->
      <div class="px-5 py-5 border-b border-slate-700/40">
        <div class="flex items-center gap-3">
          <img :src="logoSrc" alt="MonoFS" class="h-10 w-auto">
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
            ? 'bg-accent-blue/15 text-accent-blue border border-accent-blue/30'
            : 'text-slate-400 hover:text-slate-200 hover:bg-slate-800/50'"
        >
          <component :is="item.icon" class="w-4 h-4 shrink-0" />
          {{ item.label }}
        </RouterLink>
      </nav>

      <!-- Footer -->
      <div class="shrink-0 px-5 py-4 border-t border-slate-700/40 space-y-3">
        <div class="text-xs text-slate-500">
          <div>MonoFS Router</div>
        </div>
      </div>
    </aside>

    <!-- Main content -->
    <main class="flex-1 ml-0 lg:ml-[236px] min-h-screen" id="main-content">
      <header class="app-topbar">
        <button type="button" class="s-icon-btn lg:hidden" aria-label="Toggle navigation" @click="sidebarOpen = !sidebarOpen">
          <Menu class="w-4 h-4" />
        </button>
        <div class="app-topbar__crumb">
          <span>MonoFS</span>
          <span class="app-topbar__sep">/</span>
          <strong class="app-topbar__title">{{ pageTitle }}</strong>
        </div>
        <div class="flex items-center gap-3">
          <AskDrawer />
          <CommandPalette />
          <span v-if="currentUser" class="text-xs text-slate-400" title="Signed in">{{ currentUser }}</span>
          <ThemePicker />
          <a href="/auth/logout" class="text-xs font-medium text-slate-400 hover:text-slate-200 no-underline" title="Sign out">Sign out</a>
        </div>
      </header>
      <div class="p-6 lg:p-8 max-w-[1600px]">
        <RouterView />
      </div>
    </main>

    <ToastContainer />
  </div>
</template>
