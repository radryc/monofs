<script setup lang="ts">
import { ref } from 'vue'
import { THEMES, applyTheme, readStoredTheme, type Theme } from '../lib/theme'

const theme = ref<Theme>(readStoredTheme())

applyTheme(theme.value, false)

function onChange(event: Event) {
  const next = (event.target as HTMLSelectElement).value as Theme
  theme.value = next
  applyTheme(next)
}
</script>

<template>
  <label class="theme-picker" title="Color theme">
    <span class="theme-picker-swatch" aria-hidden="true" />
    <select aria-label="Color theme" :value="theme" @change="onChange">
      <option v-for="item in THEMES" :key="item.id" :value="item.id">
        {{ item.label }}
      </option>
    </select>
  </label>
</template>
