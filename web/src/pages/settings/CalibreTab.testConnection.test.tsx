import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'

vi.mock('react-i18next', () => ({
  useTranslation: () => ({ t: (key: string, fallback?: unknown) => (typeof fallback === 'string' ? fallback : key) }),
}))

vi.mock('../../api/client', () => ({
  api: {
    listSettings: vi.fn(),
    setSetting: vi.fn(),
    testCalibre: vi.fn(),
    calibreImportStatus: vi.fn(),
    calibreSyncStatus: vi.fn(),
    calibreRuns: vi.fn(),
    calibreImportStart: vi.fn(),
    calibreSyncStart: vi.fn(),
    calibreRunRollback: vi.fn(),
    calibreRunRollbackPreview: vi.fn(),
    calibreDeliverySummary: vi.fn(),
    calibreDeliveries: vi.fn(),
  },
}))

import { api } from '../../api/client'
import CalibreTab from './CalibreTab'

const mocked = api as unknown as Record<string, ReturnType<typeof vi.fn>>

const WARNING = 'Bindery Bridge 0.6.1 is older than 0.6.2. Update the Calibre plugin.'

// Test connection in plugin mode (#2831): the version used to win the
// headline and hide what the path probe found, and an outdated bridge was
// never mentioned at all.
describe('CalibreTab Test connection', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mocked.listSettings.mockResolvedValue([
      { key: 'calibre.mode', value: 'plugin' },
      { key: 'calibre.plugin_url', value: 'http://calibre:8099' },
    ])
    mocked.calibreImportStatus.mockResolvedValue({ running: false })
    mocked.calibreSyncStatus.mockResolvedValue({ running: false })
    mocked.calibreRuns.mockResolvedValue([])
    mocked.calibreDeliverySummary.mockResolvedValue({ pending: 0, delivered: 0, failed: 0, skipped: 0, mode: 'plugin', target: {} })
    mocked.calibreDeliveries.mockResolvedValue({ items: [], total: 0 })
  })

  it('shows the book the probe checked and the bridge warning', async () => {
    mocked.testCalibre.mockResolvedValue({
      ok: 'true',
      version: 'calibredb plugin v0.6.1 (Calibre 9.8)',
      message: 'plugin reachable, and it can read /mnt/books and the book at "/mnt/books/Author/Book.epub"',
      sample: '/mnt/books/Author/Book.epub',
      warning: WARNING,
    })
    render(<CalibreTab />)
    fireEvent.click(await screen.findByText('Test connection'))

    expect(await screen.findByTestId('calibre-test-detail')).toHaveTextContent('/mnt/books/Author/Book.epub')
    expect(screen.getByTestId('calibre-test-warning')).toHaveTextContent(WARNING)
    expect(screen.getByText(/Plugin reachable — calibredb plugin v0.6.1/)).toBeInTheDocument()
  })

  it('shows the warning when the probe fails too', async () => {
    const err = Object.assign(new Error('plugin reachable, and Calibre can see the library root "S:\\BOOKS", but not the book'), {
      body: { error: 'x', warning: WARNING },
    })
    mocked.testCalibre.mockRejectedValue(err)
    render(<CalibreTab />)
    fireEvent.click(await screen.findByText('Test connection'))

    expect(await screen.findByTestId('calibre-test-warning')).toHaveTextContent(WARNING)
    expect(screen.getByText(/Could not reach plugin — plugin reachable, and Calibre can see the library root "S:\\BOOKS"/)).toBeInTheDocument()
  })

  it('shows neither line for a current bridge without a path probe', async () => {
    mocked.testCalibre.mockResolvedValue({
      ok: 'true',
      version: 'calibredb plugin v0.6.2 (Calibre 9.8)',
      message: 'plugin reachable',
    })
    render(<CalibreTab />)
    fireEvent.click(await screen.findByText('Test connection'))

    expect(await screen.findByText(/Plugin reachable — calibredb plugin v0.6.2/)).toBeInTheDocument()
    expect(screen.queryByTestId('calibre-test-detail')).toBeNull()
    expect(screen.queryByTestId('calibre-test-warning')).toBeNull()
  })
})
