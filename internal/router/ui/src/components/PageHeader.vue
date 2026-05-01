<script setup lang="ts">
defineProps<{
  title: string
  subtitle?: string
  refresh?: string
}>()
</script>

<template>
  <div class="mb-7">
    <div class="flex items-end justify-between gap-4 flex-wrap">
      <div>
        <h1 class="text-2xl font-bold text-slate-100">{{ title }}</h1>
        <p v-if="subtitle" class="text-sm text-slate-400 mt-1">{{ subtitle }}</p>
      </div>
      <slot name="actions" />
    </div>
  </div>
</template>
