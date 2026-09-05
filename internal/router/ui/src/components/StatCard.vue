<script setup lang="ts">
defineProps<{
  icon?: string
  label: string
  value: string | number
  sub?: string
  color?: 'violet' | 'emerald' | 'rose' | 'amber' | 'sky' | 'teal'
}>()
</script>

<template>
  <div class="bg-slate-900/60 backdrop-blur-sm border border-slate-700/40 rounded-lg px-3 py-2.5 flex flex-wrap items-center gap-x-2 gap-y-1">
    <div class="flex items-center gap-1.5 min-w-0">
      <div v-if="icon" class="text-base leading-none shrink-0">{{ icon }}</div>
      <div class="text-xs text-slate-400 truncate">{{ label }}</div>
    </div>
    <div class="text-lg font-bold text-slate-100 leading-none shrink-0">{{ value }}</div>
    <div v-if="sub" class="basis-full text-xs text-slate-500 truncate">{{ sub }}</div>
  </div>
</template>
