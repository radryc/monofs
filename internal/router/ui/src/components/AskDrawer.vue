<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { ExternalLink, Sparkles, X } from 'lucide-vue-next'

const route = useRoute()
const open = ref(false)
const drawer = ref<HTMLElement | null>(null)
const closeButton = ref<HTMLButtonElement | null>(null)
let previousFocus: HTMLElement | null = null
const base = ((import.meta.env.VITE_AGENT_URL as string | undefined) || 'http://localhost:3000').replace(/\/$/, '')
const url = computed(() => {
  const context = `MonoFS · ${route.fullPath}`
  return `${base}/embed/ask?context=${encodeURIComponent(context)}`
})

watch(open, async (value) => {
  if (value) {
    previousFocus = document.activeElement as HTMLElement | null
    await nextTick()
    closeButton.value?.focus()
  } else {
    previousFocus?.focus()
  }
})

function onKeydown(event: KeyboardEvent) {
  if (!open.value || !drawer.value) return
  if (event.key === 'Escape') {
    open.value = false
    return
  }
  if (event.key !== 'Tab') return
  const focusable = Array.from(drawer.value.querySelectorAll<HTMLElement>(
    'a[href], button:not([disabled]), iframe, input, select, textarea, [tabindex]:not([tabindex="-1"])'
  ))
  if (focusable.length === 0) return
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

onMounted(() => window.addEventListener('keydown', onKeydown))
onBeforeUnmount(() => window.removeEventListener('keydown', onKeydown))
</script>

<template>
  <button type="button" class="s-btn s-btn-secondary" title="Ask Strata about this view" @click="open = true">
    <Sparkles class="w-4 h-4 text-accent-purple" />
    Ask
  </button>

  <Teleport to="body">
    <div v-if="open" class="s-drawer-backdrop" @click.self="open = false">
      <aside ref="drawer" class="s-drawer" role="dialog" aria-modal="true" aria-label="Ask Strata">
        <header class="s-drawer__header">
          <div class="flex items-center gap-2">
            <Sparkles class="w-4 h-4 text-accent-purple" />
            <span class="s-drawer__title">Ask Strata</span>
          </div>
          <div class="flex items-center gap-1">
            <a class="s-icon-btn" :href="url" target="_blank" rel="noreferrer" title="Open in Agent" aria-label="Open in Agent">
              <ExternalLink class="w-4 h-4" />
            </a>
            <button ref="closeButton" type="button" class="s-icon-btn" aria-label="Close Ask Strata" @click="open = false">
              <X class="w-4 h-4" />
            </button>
          </div>
        </header>
        <div class="s-drawer__body">
          <iframe title="Ask Strata" :src="url" class="h-full w-full border-0" allow="clipboard-read; clipboard-write" />
        </div>
      </aside>
    </div>
  </Teleport>
</template>
