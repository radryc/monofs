<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import {
  BookOpen, Container, Cpu, Download, Layers, LayoutDashboard, Package, Play,
  Search, Server, SlidersHorizontal, Upload, Users, Zap, GitBranch,
} from 'lucide-vue-next'

const router = useRouter()
const open = ref(false)
const query = ref('')
const active = ref(0)
const input = ref<HTMLInputElement | null>(null)
const card = ref<HTMLElement | null>(null)

const items = [
  { to: '/', label: 'Dashboard', icon: LayoutDashboard },
  { to: '/cicd', label: 'CI/CD', icon: SlidersHorizontal },
  { to: '/pipelines', label: 'Pipelines', icon: Play },
  { to: '/bazel', label: 'Bazel', icon: Cpu },
  { to: '/cluster', label: 'MetaStores', icon: Server },
  { to: '/clients', label: 'Clients', icon: Users },
  { to: '/performance', label: 'Performance', icon: Zap },
  { to: '/replication', label: 'Replication', icon: GitBranch },
  { to: '/repositories', label: 'Repositories', icon: BookOpen },
  { to: '/ingest', label: 'Ingest', icon: Upload },
  { to: '/search', label: 'Search', icon: Search },
  { to: '/indexer', label: 'Indexer', icon: Layers },
  { to: '/fetchers', label: 'Fetchers', icon: Download },
  { to: '/dependencies', label: 'Dependencies', icon: Package },
  { to: '/registry', label: 'Registry', icon: Container },
]

const filtered = computed(() => {
  const needle = query.value.trim().toLowerCase()
  return items.filter((item) => item.label.toLowerCase().includes(needle))
})

async function show() {
  open.value = true
  query.value = ''
  active.value = 0
  await nextTick()
  input.value?.focus()
}

function close() {
  open.value = false
}

function choose(index = active.value) {
  const item = filtered.value[index]
  if (!item) return
  close()
  router.push(item.to)
}

function onInput() {
  active.value = 0
}

function onKeydown(event: KeyboardEvent) {
  if (event.key === 'ArrowDown') {
    event.preventDefault()
    active.value = Math.min(active.value + 1, filtered.value.length - 1)
  } else if (event.key === 'ArrowUp') {
    event.preventDefault()
    active.value = Math.max(active.value - 1, 0)
  } else if (event.key === 'Enter') {
    event.preventDefault()
    choose()
  } else if (event.key === 'Escape') {
    close()
  }
}

function globalKeydown(event: KeyboardEvent) {
  if (open.value && event.key === 'Tab' && card.value) {
    const focusable = Array.from(card.value.querySelectorAll<HTMLElement>(
      'input, button:not([disabled]), a[href], [tabindex]:not([tabindex="-1"])'
    ))
    if (focusable.length > 0) {
      const first = focusable[0]
      const last = focusable[focusable.length - 1]
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault()
        last.focus()
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault()
        first.focus()
      }
    }
  }
  const target = event.target as HTMLElement | null
  const typing = target?.tagName === 'INPUT' || target?.tagName === 'TEXTAREA' || target?.isContentEditable
  if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k') {
    event.preventDefault()
    open.value ? close() : show()
  } else if (!typing && event.key === '/') {
    event.preventDefault()
    show()
  }
}

onMounted(() => window.addEventListener('keydown', globalKeydown))
onBeforeUnmount(() => window.removeEventListener('keydown', globalKeydown))

defineExpose({ show })
</script>

<template>
  <button type="button" class="s-btn s-btn-ghost" title="Quick jump (Ctrl/Cmd-K)" @click="show">
    <Search class="w-4 h-4" />
    <span class="hidden sm:inline">Quick jump</span>
    <kbd class="s-kbd">⌘K</kbd>
  </button>

  <Teleport to="body">
    <div v-if="open" class="command-palette" role="dialog" aria-modal="true" aria-label="Quick jump" @click.self="close">
      <div ref="card" class="command-palette-card">
        <input ref="input" v-model="query" type="search" placeholder="Jump to a page…" autocomplete="off" @input="onInput" @keydown="onKeydown" />
        <div class="command-list">
          <button
            v-for="(item, index) in filtered"
            :key="item.to"
            type="button"
            class="command-item"
            :class="{ 'is-active': index === active }"
            @click="choose(index)"
            @mouseenter="active = index"
          >
            <span class="flex items-center gap-2"><component :is="item.icon" class="w-4 h-4" />{{ item.label }}</span>
            <kbd>{{ item.to }}</kbd>
          </button>
          <div v-if="filtered.length === 0" class="command-empty">No matching pages.</div>
        </div>
      </div>
    </div>
  </Teleport>
</template>

<style scoped>
.command-palette {
  position: fixed;
  inset: 0;
  z-index: 300;
  display: grid;
  place-items: start center;
  padding: 12vh 16px 16px;
  background: var(--ui-scrim);
  backdrop-filter: blur(4px);
}
.command-palette-card {
  width: min(620px, 100%);
  overflow: hidden;
  border: 1px solid var(--ui-border);
  border-radius: var(--strata-radius-md);
  background: var(--ui-panel);
  box-shadow: var(--ui-shadow);
}
.command-palette-card input {
  width: 100%;
  min-height: 48px;
  padding: 12px 16px;
  border: 0;
  border-bottom: 1px solid var(--ui-border);
  background: var(--ui-input);
  color: var(--ui-text);
  font-size: 15px;
  outline: none;
}
.command-list { max-height: min(58vh, 520px); overflow-y: auto; padding: 6px; }
.command-item {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  width: 100%;
  min-height: 36px;
  padding: 7px 10px;
  border: 1px solid transparent;
  border-radius: var(--strata-radius-sm);
  background: transparent;
  color: var(--ui-muted);
  font-size: 13px;
  text-align: left;
  cursor: pointer;
}
.command-item:hover,
.command-item.is-active { border-color: var(--ui-overlay-border); background: var(--ui-overlay-soft); color: var(--ui-text); }
.command-item kbd { padding: 1px 5px; border: 1px solid var(--ui-border); border-radius: 4px; color: var(--ui-faint); font-family: var(--mono); font-size: 10px; }
.command-empty { padding: 20px; color: var(--ui-faint); font-size: 13px; text-align: center; }
</style>
