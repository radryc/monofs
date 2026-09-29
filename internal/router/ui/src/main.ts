import { createApp } from 'vue'
import { createPinia } from 'pinia'
import './style.css'
import App from './App.vue'
import { router } from './router'

// When deployed under a sub-path (e.g. VITE_BASE=/monofs/ behind an ALB that
// does not rewrite paths), rewrite root-absolute fetch() targets onto the base
// so the UI's many fetch('/api/...') calls resolve under /monofs/api/...
const BASE_URL = import.meta.env.BASE_URL
if (BASE_URL && BASE_URL !== '/') {
  const prefix = BASE_URL.replace(/\/$/, '')
  const nativeFetch = window.fetch.bind(window)
  const underPrefix = (path: string) => path === prefix || path.startsWith(prefix + '/')
  window.fetch = (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    if (typeof input === 'string' && input.startsWith('/') && !underPrefix(input)) {
      input = prefix + input
    } else if (input instanceof Request) {
      const u = new URL(input.url)
      if (u.origin === window.location.origin && u.pathname.startsWith('/') && !underPrefix(u.pathname)) {
        u.pathname = prefix + u.pathname
        input = new Request(u.toString(), input)
      }
    }
    return nativeFetch(input, init)
  }
}

const app = createApp(App)
app.use(createPinia())
app.use(router)
app.mount('#app')
