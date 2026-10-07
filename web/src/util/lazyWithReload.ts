import { lazy, type ComponentType, type LazyExoticComponent } from 'react'

// Route and settings tab chunks are fetched by hashed file name the first time
// they are opened. A tab left open across an upgrade still holds the old
// index, so its next navigation asks for a chunk the new build no longer
// serves, the import rejects, and the error page shows. Reloading fetches the
// new index and its chunk names. The sessionStorage flag allows one reload per
// failure: if the chunk still fails after it, the error is real and is thrown
// to the error boundary instead of reloading forever. Any successful chunk
// load clears the flag, so a later upgrade in the same tab gets its reload too.

export const CHUNK_RELOAD_KEY = 'bindery:chunk-reload'

// Messages for a failed dynamic import across browsers: Chromium, Firefox,
// Safari, and Vite's own preload helper.
const CHUNK_ERROR = /Failed to fetch dynamically imported module|error loading dynamically imported module|Importing a module script failed|Unable to preload CSS|ChunkLoadError|Loading (CSS )?chunk .* failed/i

export function isChunkLoadError(err: unknown): boolean {
  if (err instanceof Error) return CHUNK_ERROR.test(`${err.name}: ${err.message}`)
  return typeof err === 'string' && CHUNK_ERROR.test(err)
}

function readFlag(): boolean | null {
  try {
    return window.sessionStorage.getItem(CHUNK_RELOAD_KEY) !== null
  } catch {
    return null
  }
}

function setFlag(): boolean {
  try {
    window.sessionStorage.setItem(CHUNK_RELOAD_KEY, String(Date.now()))
    return true
  } catch {
    return false
  }
}

function clearFlag() {
  try {
    window.sessionStorage.removeItem(CHUNK_RELOAD_KEY)
  } catch {
    // Storage unavailable: nothing was set, nothing to clear.
  }
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
export function lazyWithReload<T extends ComponentType<any>>(
  factory: () => Promise<{ default: T }>,
): LazyExoticComponent<T> {
  return lazy(async () => {
    try {
      const mod = await factory()
      clearFlag()
      return mod
    } catch (err) {
      // Reload only when the flag can be both read and written; without
      // storage there is no loop guard, so the error page is the safer end.
      if (isChunkLoadError(err) && readFlag() === false && setFlag()) {
        window.location.reload()
        // Stay suspended until the reload replaces the page.
        return new Promise<never>(() => {})
      }
      throw err
    }
  })
}
