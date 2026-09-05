<script setup lang="ts">
defineProps<{ value: number; class?: string }>()
</script>

<template>
  <div class="w-full bg-slate-700/40 rounded-full h-1.5 overflow-hidden">
    <div
      class="h-full rounded-full transition-all duration-500"
      :class="value > 80 ? 'bg-rose-500' : value > 60 ? 'bg-amber-500' : 'bg-violet-500'"
      :style="{ width: `${Math.min(value, 100)}%` }"
    />
  </div>
</template>
