<script setup lang="ts">
import { ref, onMounted, watchEffect } from 'vue'
import { X, BookOpen } from 'lucide-vue-next'
import { marked } from 'marked'

const props = defineProps<{ open: boolean }>()
const emit = defineEmits<{ 'update:open': [value: boolean] }>()

const html = ref('')
const loading = ref(false)

onMounted(async () => {
  loading.value = true
  try {
    const resp = await fetch('/api/pipelines/help')
    const md = await resp.text()
    html.value = await marked(md)
  } catch {
    html.value = '<p class="text-slate-500">Failed to load documentation.</p>'
  } finally {
    loading.value = false
  }
})

watchEffect(() => {
  if (props.open) {
    document.body.style.overflow = 'hidden'
  } else {
    document.body.style.overflow = ''
  }
})

function close() {
  emit('update:open', false)
}
</script>

<template>
  <Teleport to="body">
    <transition name="fade">
      <div v-if="open" class="fixed inset-0 z-50 flex">
        <div class="absolute inset-0 bg-black/50 backdrop-blur-sm" @click="close" />

        <div class="relative ml-auto w-full max-w-2xl h-full bg-slate-950 border-l border-slate-700/40 shadow-2xl flex flex-col">
          <div class="flex items-center justify-between px-6 py-4 border-b border-slate-700/40 shrink-0">
            <div class="flex items-center gap-2">
              <BookOpen class="w-4 h-4 text-violet-400" />
              <h2 class="text-sm font-semibold text-slate-200">Pipeline Documentation</h2>
            </div>
            <button
              class="p-1.5 rounded-lg text-slate-400 hover:text-slate-200 hover:bg-slate-800/50 transition-colors"
              @click="close"
            >
              <X class="w-4 h-4" />
            </button>
          </div>

          <div class="flex-1 overflow-y-auto">
            <div v-if="loading" class="flex items-center justify-center h-full">
              <div class="text-sm text-slate-500 animate-pulse">Loading documentation...</div>
            </div>
            <div
              v-else
              class="prose prose-sm prose-invert max-w-none px-6 py-6 [&_h1]:text-xl [&_h1]:font-bold [&_h1]:text-slate-100 [&_h1]:mb-4 [&_h2]:text-base [&_h2]:font-semibold [&_h2]:text-slate-200 [&_h2]:mt-8 [&_h2]:mb-3 [&_h3]:text-sm [&_h3]:font-semibold [&_h3]:text-slate-300 [&_h3]:mt-6 [&_h3]:mb-2 [&_p]:text-xs [&_p]:text-slate-400 [&_p]:leading-relaxed [&_p]:mb-3 [&_ul]:text-xs [&_ul]:text-slate-400 [&_li]:mb-1 [&_ol]:text-xs [&_ol]:text-slate-400 [&_strong]:text-slate-200 [&_code]:text-violet-400 [&_code]:bg-slate-800/70 [&_code]:px-1 [&_code]:py-0.5 [&_code]:rounded [&_code]:text-[11px] [&_pre]:bg-slate-900 [&_pre]:border [&_pre]:border-slate-700/30 [&_pre]:rounded-lg [&_pre]:p-4 [&_pre]:mb-4 [&_pre]:overflow-x-auto [&_pre_code]:bg-transparent [&_pre_code]:p-0 [&_pre_code]:text-[11px] [&_pre_code]:leading-relaxed [&_table]:w-full [&_table]:text-xs [&_table]:mb-4 [&_table]:border-collapse [&_th]:text-left [&_th]:text-slate-300 [&_th]:font-medium [&_th]:px-3 [&_th]:py-2 [&_th]:border-b [&_th]:border-slate-700/40 [&_td]:text-slate-400 [&_td]:px-3 [&_td]:py-2 [&_td]:border-b [&_td]:border-slate-700/20 [&_a]:text-violet-400 [&_a]:underline [&_blockquote]:border-l-2 [&_blockquote]:border-violet-500/30 [&_blockquote]:pl-4 [&_blockquote]:text-slate-500 [&_blockquote]:text-xs [&_hr]:border-slate-700/30 [&_hr]:my-6"
              v-html="html"
            />
          </div>
        </div>
      </div>
    </transition>
  </Teleport>
</template>

<style scoped>
.fade-enter-active,
.fade-leave-active {
  transition: opacity 0.2s ease;
}
.fade-enter-from,
.fade-leave-to {
  opacity: 0;
}
</style>
