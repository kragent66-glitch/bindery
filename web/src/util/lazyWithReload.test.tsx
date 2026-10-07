import { Suspense, type ComponentType } from 'react'
import { render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ErrorBoundary from '../components/ErrorBoundary'
import { CHUNK_RELOAD_KEY, isChunkLoadError, lazyWithReload } from './lazyWithReload'

// The error a browser raises when a lazy route's chunk 404s after an upgrade.
const chunkError = () => new TypeError('Failed to fetch dynamically imported module: http://localhost/assets/BooksPage-abc123.js')

function Page() {
  return <p>page loaded</p>
}

function mount(factory: () => Promise<{ default: ComponentType }>) {
  const Lazy = lazyWithReload(factory)
  return render(
    <ErrorBoundary>
      <Suspense fallback={<p>loading</p>}>
        <Lazy />
      </Suspense>
    </ErrorBoundary>,
  )
}

describe('lazyWithReload', () => {
  const original = window.location
  let reload: ReturnType<typeof vi.fn>

  beforeEach(() => {
    sessionStorage.clear()
    reload = vi.fn()
    Object.defineProperty(window, 'location', {
      configurable: true,
      value: { ...original, reload },
    })
    vi.spyOn(console, 'error').mockImplementation(() => {})
  })

  afterEach(() => {
    Object.defineProperty(window, 'location', { configurable: true, value: original })
    sessionStorage.clear()
    vi.restoreAllMocks()
  })

  it('reloads the page once when a chunk fails to load, instead of showing the error page', async () => {
    mount(() => Promise.reject(chunkError()))
    await waitFor(() => expect(reload).toHaveBeenCalledTimes(1))
    expect(sessionStorage.getItem(CHUNK_RELOAD_KEY)).not.toBeNull()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(screen.getByText('loading')).toBeInTheDocument()
  })

  it('shows the error page instead of reloading again when the chunk still fails after the reload', async () => {
    sessionStorage.setItem(CHUNK_RELOAD_KEY, '1')
    mount(() => Promise.reject(chunkError()))
    expect(await screen.findByRole('alert')).toBeInTheDocument()
    expect(reload).not.toHaveBeenCalled()
  })

  it('does not reload for an error that is not a failed chunk load', async () => {
    mount(() => Promise.reject(new Error('kaboom')))
    expect(await screen.findByRole('alert')).toBeInTheDocument()
    expect(reload).not.toHaveBeenCalled()
  })

  it('clears the guard after a chunk loads, so a later upgrade can reload again', async () => {
    sessionStorage.setItem(CHUNK_RELOAD_KEY, '1')
    mount(() => Promise.resolve({ default: Page }))
    expect(await screen.findByText('page loaded')).toBeInTheDocument()
    expect(sessionStorage.getItem(CHUNK_RELOAD_KEY)).toBeNull()
    expect(reload).not.toHaveBeenCalled()
  })

  it('recognises the chunk error messages of each browser', () => {
    expect(isChunkLoadError(chunkError())).toBe(true)
    expect(isChunkLoadError(new TypeError('error loading dynamically imported module: x.js'))).toBe(true)
    expect(isChunkLoadError(new TypeError('Importing a module script failed.'))).toBe(true)
    expect(isChunkLoadError(new Error('Unable to preload CSS for /assets/x.css'))).toBe(true)
    expect(isChunkLoadError(new Error('kaboom'))).toBe(false)
    expect(isChunkLoadError(undefined)).toBe(false)
  })
})

// Every code split route and settings tab must go through the wrapper: a bare
// React.lazy brings the error page back for that one route.
const sources = import.meta.glob(['../App.tsx', '../pages/SettingsPage.tsx'], {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>

describe('lazy routes', () => {
  it.each(Object.keys(sources))('%s loads every chunk through lazyWithReload', file => {
    const src = sources[file]
    expect(src).toMatch(/lazyWithReload\(\(\) => import\(/)
    expect(src).not.toMatch(/\blazy\(\(\) => import\(/)
  })
})
