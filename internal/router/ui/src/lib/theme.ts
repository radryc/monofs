export const THEME_KEY = 'monofs.theme'

export const THEMES = [
  { id: 'midnight', label: 'Midnight' },
  { id: 'graphite', label: 'Graphite' },
  { id: 'ocean', label: 'Ocean' },
  { id: 'forest', label: 'Forest' },
  { id: 'daylight', label: 'Daylight' },
] as const

export type Theme = (typeof THEMES)[number]['id']

export const DEFAULT_THEME: Theme = 'midnight'

export function isTheme(value: string | null | undefined): value is Theme {
  return THEMES.some((theme) => theme.id === value)
}

export function readStoredTheme(): Theme {
  try {
    const stored = localStorage.getItem(THEME_KEY)
    if (isTheme(stored)) return stored
  } catch {
    /* localStorage unavailable */
  }
  return DEFAULT_THEME
}

export function applyTheme(theme: Theme, persist = true): void {
  const next = isTheme(theme) ? theme : DEFAULT_THEME
  document.documentElement.setAttribute('data-theme', next)
  if (persist) {
    try {
      localStorage.setItem(THEME_KEY, next)
    } catch {
      /* localStorage unavailable */
    }
  }
}
