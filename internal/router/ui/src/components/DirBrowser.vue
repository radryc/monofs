<script setup lang="ts">
import { ref, watch, onMounted } from 'vue'

const props = defineProps<{ modelValue: string }>()
const emit = defineEmits<{ 'update:modelValue': [value: string] }>()

interface FSEntry {
  name: string
  is_dir: boolean
}

interface FSListResponse {
  path: string
  entries: FSEntry[]
  error?: string
}

const currentPath = ref('/')
const entries = ref<FSEntry[]>([])
const error = ref('')
const loading = ref(false)

async function browse(path: string) {
  loading.value = true
  error.value = ''
  try {
    const res = await fetch(`/api/fs/ls?path=${encodeURIComponent(path)}`)
    const data: FSListResponse = await res.json()
    if (data.error) {
      error.value = data.error
      entries.value = []
    } else {
      currentPath.value = data.path
      entries.value = data.entries
    }
  } catch {
    error.value = 'Failed to browse directory'
  } finally {
    loading.value = false
  }
}

function enter(name: string) {
  const sep = currentPath.value.endsWith('/') ? '' : '/'
  browse(currentPath.value + sep + name)
}

function goUp() {
  if (currentPath.value === '/') return
  const parent = currentPath.value.split('/').slice(0, -1).join('/') || '/'
  browse(parent)
}

function selectDir() {
  emit('update:modelValue', currentPath.value)
}

function dirParts(path: string): { label: string; path: string }[] {
  if (path === '/') return [{ label: '/', path: '/' }]
  const parts = path.split('/').filter(Boolean)
  return [{ label: '/', path: '/' }, ...parts.map((p, i) => ({
    label: p,
    path: '/' + parts.slice(0, i + 1).join('/'),
  }))]
}

onMounted(() => {
  const initial = props.modelValue || '/'
  browse(initial)
})

watch(() => props.modelValue, (val) => {
  if (val && val !== currentPath.value) {
    browse(val)
  }
})
</script>

<template>
  <div class="border border-slate-700/40 rounded-xl overflow-hidden">
    <div class="flex items-center gap-2 px-4 py-3 bg-slate-800/40 border-b border-slate-700/30">
      <button
        @click="goUp"
        :disabled="currentPath === '/'"
        class="p-1 rounded text-slate-400 hover:text-slate-200 hover:bg-slate-700/50 disabled:opacity-30 disabled:cursor-not-allowed"
        title="Up"
      >
        <svg class="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m18 15-6-6-6 6"/></svg>
      </button>
      <div class="flex items-center gap-1 text-xs text-slate-400 overflow-x-auto">
        <template v-for="(part, i) in dirParts(currentPath)" :key="part.path">
          <span v-if="i > 0" class="text-slate-600">/</span>
          <button
            @click="browse(part.path)"
            class="hover:text-violet-400 hover:underline whitespace-nowrap"
          >{{ part.label }}</button>
        </template>
      </div>
    </div>

    <div v-if="loading" class="flex items-center justify-center py-8 text-slate-400 text-sm gap-2">
      <svg class="animate-spin w-4 h-4" viewBox="0 0 24 24" fill="none">
        <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"/>
        <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z"/>
      </svg>
      Loading...
    </div>

    <div v-else-if="error" class="flex items-center justify-center py-8 px-4 text-rose-400 text-sm">
      {{ error }}
    </div>

    <div v-else class="max-h-64 overflow-y-auto">
      <div v-if="entries.length === 0" class="py-8 text-center text-sm text-slate-500">
        Empty directory
      </div>
      <button
        v-for="e in entries.filter(x => x.is_dir)"
        :key="e.name"
        @click="enter(e.name)"
        class="w-full flex items-center gap-3 px-4 py-2 text-sm text-slate-300 hover:bg-slate-700/40 text-left transition-colors"
      >
        <svg class="w-4 h-4 text-amber-400/70 shrink-0" viewBox="0 0 24 24" fill="currentColor"><path d="M10 4H4c-1.1 0-2 .9-2 2v12c0 1.1.9 2 2 2h16c1.1 0 2-.9 2-2V8c0-1.1-.9-2-2-2h-8l-2-2z"/></svg>
        <span class="truncate">{{ e.name }}</span>
      </button>
      <div
        v-for="e in entries.filter(x => !x.is_dir)"
        :key="e.name"
        class="flex items-center gap-3 px-4 py-1.5 text-sm text-slate-500"
      >
        <svg class="w-4 h-4 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/></svg>
        <span class="truncate">{{ e.name }}</span>
      </div>
    </div>

    <div class="px-4 py-3 border-t border-slate-700/30 bg-slate-800/20">
      <button
        @click="selectDir"
        class="w-full py-2 rounded-lg text-sm font-medium bg-violet-600/20 text-violet-300 border border-violet-500/30 hover:bg-violet-600/30 transition-colors"
      >
        Select &quot;{{ currentPath }}&quot;
      </button>
    </div>
  </div>
</template>
