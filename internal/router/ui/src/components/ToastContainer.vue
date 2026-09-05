<script setup lang="ts">
import { useAppStore } from '../stores/app'
import { X, CheckCircle, AlertCircle, Info, AlertTriangle } from 'lucide-vue-next'
const store = useAppStore()

const icons = { success: CheckCircle, error: AlertCircle, info: Info, warning: AlertTriangle }
const colors = {
  success: 'border-emerald-500/40 bg-emerald-950/80 text-emerald-300',
  error:   'border-rose-500/40 bg-rose-950/80 text-rose-300',
  info:    'border-sky-500/40 bg-sky-950/80 text-sky-300',
  warning: 'border-amber-500/40 bg-amber-950/80 text-amber-300',
}
</script>

<template>
  <div class="fixed bottom-4 right-4 z-50 flex flex-col gap-2 items-end">
    <TransitionGroup
      enter-active-class="transition duration-200"
      enter-from-class="opacity-0 translate-x-4"
      leave-active-class="transition duration-200"
      leave-to-class="opacity-0 translate-x-4"
    >
      <div
        v-for="toast in store.toasts"
        :key="toast.id"
        class="flex items-start gap-3 px-4 py-3 rounded-xl border backdrop-blur-xl shadow-2xl max-w-sm text-sm"
        :class="colors[toast.type]"
      >
        <component :is="icons[toast.type]" class="w-4 h-4 mt-0.5 shrink-0" />
        <span class="flex-1">{{ toast.message }}</span>
        <button @click="store.removeToast(toast.id)" class="shrink-0 opacity-60 hover:opacity-100">
          <X class="w-3.5 h-3.5" />
        </button>
      </div>
    </TransitionGroup>
  </div>
</template>
