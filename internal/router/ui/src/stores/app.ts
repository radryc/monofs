import { defineStore } from 'pinia'
import { ref } from 'vue'

export interface Toast {
  id: number
  type: 'success' | 'error' | 'info' | 'warning'
  message: string
}

export const useAppStore = defineStore('app', () => {
  const version = ref('')
  const toasts = ref<Toast[]>([])
  let nextId = 1

  function setVersion(v: string) {
    version.value = v
  }

  function addToast(type: Toast['type'], message: string, durationMs = 4000) {
    const id = nextId++
    toasts.value.push({ id, type, message })
    setTimeout(() => removeToast(id), durationMs)
  }

  function removeToast(id: number) {
    const idx = toasts.value.findIndex((t) => t.id === id)
    if (idx !== -1) toasts.value.splice(idx, 1)
  }

  return { version, toasts, setVersion, addToast, removeToast }
})
