const REDIRECT_DEBOUNCE_KEY = '_monofs_auth_redirecting'
const REDIRECT_DEBOUNCE_MS = 3000

function isAuthPath(url: string): boolean {
  try {
    const path = new URL(url, window.location.origin).pathname
    return path.startsWith('/auth/')
  } catch {
    return false
  }
}

function currentPathIsAuth(): boolean {
  return isAuthPath(window.location.pathname)
}

export function installFetchAuthHandler() {
  const _fetch = window.fetch

  window.fetch = async function (input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
    if (sessionStorage.getItem(REDIRECT_DEBOUNCE_KEY)) {
      return _fetch(input, init)
    }

    if (typeof input === 'string' && isAuthPath(input)) {
      return _fetch(input, init)
    }
    if (input instanceof Request && isAuthPath(input.url)) {
      return _fetch(input, init)
    }

    try {
      const resp = await _fetch(input, init)

      if (resp.status === 401 && !sessionStorage.getItem(REDIRECT_DEBOUNCE_KEY) && !currentPathIsAuth()) {
        sessionStorage.setItem(REDIRECT_DEBOUNCE_KEY, '1')
        setTimeout(() => { sessionStorage.removeItem(REDIRECT_DEBOUNCE_KEY) }, REDIRECT_DEBOUNCE_MS)
        window.location.href = '/auth/login'
        throw new Error('Session expired — redirecting to login')
      }

      return resp
    } catch (err) {
      if (sessionStorage.getItem(REDIRECT_DEBOUNCE_KEY)) {
        throw new Error('Session expired — redirecting to login')
      }
      throw err
    }
  }
}
