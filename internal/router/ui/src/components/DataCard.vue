<script setup lang="ts">
defineProps<{ loading?: boolean; error?: string | null }>()
</script>

<template>
  <div class="bg-slate-900/60 backdrop-blur-sm border border-slate-700/40 rounded-2xl overflow-hidden">
    <!-- Card header -->
    <div v-if="$slots.header" class="px-6 py-4 border-b border-slate-700/40">
      <slot name="header" />
    </div>
    <!-- Loading state -->
    <div v-if="loading" class="flex items-center justify-center py-16 text-slate-400 text-sm gap-3">
      <svg class="animate-spin w-5 h-5" viewBox="0 0 24 24" fill="none">
        <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"/>
        <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z"/>
      </svg>
      Loading...
    </div>
    <!-- Error state -->
    <div v-else-if="error" class="flex items-center justify-center py-12 px-6 text-rose-400 text-sm gap-2">
      <span class="font-medium">Error:</span> {{ error }}
    </div>
    <!-- Content -->
    <slot v-else />
  </div>
</template>
