import { ref, onUnmounted } from 'vue'

/**
 * Calls `fn` immediately, then every `intervalMs` milliseconds.
 * Cleans up automatically on component unmount.
 */
export function useAutoRefresh(fn: () => void | Promise<void>, intervalMs: number) {
  const loading = ref(true)
  const error = ref<string | null>(null)

  async function run() {
    try {
      await fn()
      error.value = null
    } catch (e: unknown) {
      error.value = e instanceof Error ? e.message : String(e)
    } finally {
      loading.value = false
    }
  }

  run()
  const timer = setInterval(run, intervalMs)
  onUnmounted(() => clearInterval(timer))

  return { loading, error }
}

/**
 * Format bytes into human-readable string
 */
export function formatBytes(bytes: number): string {
  if (bytes === 0) return '0 B'
  const k = 1024
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(k))
  return `${parseFloat((bytes / Math.pow(k, i)).toFixed(1))} ${sizes[i]}`
}

export function formatNumber(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`
  return String(n)
}

export function formatDate(d: string): string {
  if (!d) return '-'
  try {
    return new Date(d).toLocaleString()
  } catch {
    return d
  }
}

export function formatPercent(v: number): string {
  return `${(v * 100).toFixed(1)}%`
}

export function timeAgo(d: string): string {
  if (!d) return '-'
  const ms = Date.now() - new Date(d).getTime()
  const s = Math.floor(ms / 1000)
  if (s < 60) return `${s}s ago`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ago`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h ago`
  return `${Math.floor(h / 24)}d ago`
}

/** Like timeAgo but accepts a Unix timestamp (seconds, as returned by protobuf int64). */
export function timeAgoUnix(ts: number): string {
  if (!ts) return '-'
  const s = Math.floor((Date.now() - ts * 1000) / 1000)
  if (s < 0) return 'just now'
  if (s < 60) return `${s}s ago`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ago`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h ago`
  return `${Math.floor(h / 24)}d ago`
}
