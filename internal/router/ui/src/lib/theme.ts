export const THEME_KEY = 'strata.theme'
export const LEGACY_THEME_KEY = 'monofs.theme'

export const THEMES = [
  { id: 'system', label: 'System' },
  { id: 'midnight', label: 'Midnight' },
  { id: 'daylight', label: 'Daylight' },
  { id: 'graphite', label: 'Graphite' },
  { id: 'ocean', label: 'Ocean' },
  { id: 'forest', label: 'Forest' },
] as const

export type Theme = (typeof THEMES)[number]['id']
export type ResolvedTheme = Exclude<Theme, 'system'>

export const DEFAULT_THEME: Theme = 'system'

export function isTheme(value: string | null | undefined): value is Theme {
  return THEMES.some((theme) => theme.id === value)
}

export function resolveTheme(theme: Theme): ResolvedTheme {
  if (theme !== 'system') return theme
  try {
    if (window.matchMedia('(prefers-color-scheme: light)').matches) return 'daylight'
  } catch {
    /* matchMedia unavailable */
  }
  return 'midnight'
}

export function readStoredTheme(): Theme {
  try {
    const stored = localStorage.getItem(THEME_KEY)
    if (isTheme(stored)) return stored
    const legacy = localStorage.getItem(LEGACY_THEME_KEY)
    if (isTheme(legacy) && legacy !== 'system') return legacy
  } catch {
    /* localStorage unavailable */
  }
  return DEFAULT_THEME
}

export function applyTheme(theme: Theme, persist = true): void {
  const next = isTheme(theme) ? theme : DEFAULT_THEME
  const resolved = resolveTheme(next)
  document.documentElement.setAttribute('data-theme', resolved)
  document.documentElement.dataset.themePreference = next
  if (persist) {
    try {
      localStorage.setItem(THEME_KEY, next)
    } catch {
      /* localStorage unavailable */
    }
  }
  window.dispatchEvent(
    new CustomEvent('strata:themechange', {
      detail: { preference: next, resolved },
    })
  )
}

let systemListenerAttached = false

export function watchSystemTheme(): void {
  if (systemListenerAttached || typeof window === 'undefined') return
  systemListenerAttached = true
  try {
    const media = window.matchMedia('(prefers-color-scheme: light)')
    media.addEventListener?.('change', () => {
      if (readStoredTheme() === 'system') applyTheme('system', false)
    })
  } catch {
    /* matchMedia unavailable */
  }
}
