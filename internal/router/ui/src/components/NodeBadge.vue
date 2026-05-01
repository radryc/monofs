<script setup lang="ts">
defineProps<{ healthy: boolean; label?: string }>()
</script>

<template>
  <span
    class="inline-flex items-center gap-1.5 px-2 py-0.5 rounded-full text-xs font-semibold"
    :class="healthy ? 'bg-emerald-500/15 text-emerald-400 border border-emerald-500/20' : 'bg-rose-500/15 text-rose-400 border border-rose-500/20'"
  >
    <span class="w-1.5 h-1.5 rounded-full" :class="healthy ? 'bg-emerald-400' : 'bg-rose-400'" />
    {{ label ?? (healthy ? 'Healthy' : 'Unhealthy') }}
  </span>
</template>
