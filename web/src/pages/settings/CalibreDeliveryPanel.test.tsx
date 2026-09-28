import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react'
import en from '../../i18n/locales/en.json'

// Resolve keys against the real English locale so the assertions read the
// strings users see.
function translate(key: string, arg?: unknown): string {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  let node: any = en
  for (const part of key.split('.')) node = node?.[part]
  let str = typeof node === 'string' ? node : key
  if (arg && typeof arg === 'object') {
    for (const [k, v] of Object.entries(arg as Record<string, unknown>)) {
      str = str.replace(new RegExp(`{{\\s*${k}\\s*}}`, 'g'), String(v))
    }
  }
  return str
}
const translation = { t: translate }

vi.mock('react-i18next', () => ({
  useTranslation: () => translation,
}))

vi.mock('../../api/client', () => ({
  api: {
    calibreDeliverySummary: vi.fn(),
    calibreDeliveries: vi.fn(),
    calibreDeliveryRetry: vi.fn(),
    calibreDeliveryClearPending: vi.fn(),
    calibreDeliveryReset: vi.fn(),
  },
}))

import { api } from '../../api/client'
import CalibreDeliveryPanel from './CalibreDeliveryPanel'

const mocked = api as unknown as Record<string, ReturnType<typeof vi.fn>>

const failedRow = {
  id: 7,
  bookId: 3,
  bookFileId: 30,
  filePath: '/books/Dune.epub',
  format: 'epub',
  state: 'failed',
  outcome: '',
  attempts: 8,
  lastError: 'plugin said no',
  lastErrorCode: 'bad_format',
  updatedAt: '2026-09-27T12:00:00Z',
  bookTitle: 'Dune',
  authorName: 'Frank Herbert',
}

describe('CalibreDeliveryPanel', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mocked.calibreDeliverySummary.mockResolvedValue({
      pending: 4,
      delivered: 12,
      failed: 1,
      skipped: 0,
      lastDeliveredAt: '2026-09-27T11:00:00Z',
      mode: 'plugin',
      target: { reachable: false, lastError: 'dial tcp: connection refused', checkedAt: '2026-09-27T12:01:00Z' },
    })
    mocked.calibreDeliveries.mockResolvedValue({ items: [failedRow], total: 1 })
  })

  it('shows the summary line with counts and why Calibre is unreachable', async () => {
    render(<CalibreDeliveryPanel />)
    const summary = await screen.findByTestId('calibre-delivery-summary')
    expect(summary).toHaveTextContent('Waiting 4')
    expect(summary).toHaveTextContent('Delivered 12')
    expect(summary).toHaveTextContent('Failed 1')
    expect(summary).toHaveTextContent('Last delivered')
    expect(screen.getByTestId('calibre-delivery-reachability')).toHaveTextContent(
      'Calibre not reachable: dial tcp: connection refused',
    )
    expect(mocked.calibreDeliveries).toHaveBeenCalledWith('failed', 50, 0)
  })

  it('says Calibre is reachable when the worker last reached it', async () => {
    mocked.calibreDeliverySummary.mockResolvedValue({
      pending: 0, delivered: 1, failed: 0, skipped: 0, mode: 'plugin', target: { reachable: true },
    })
    render(<CalibreDeliveryPanel />)
    expect(await screen.findByTestId('calibre-delivery-reachability')).toHaveTextContent('Calibre reachable')
  })

  it('lists failed deliveries and retries them', async () => {
    mocked.calibreDeliveryRetry.mockResolvedValue({ requeued: 1 })
    render(<CalibreDeliveryPanel />)
    const table = await screen.findByTestId('calibre-delivery-failed-table')
    const row = within(table).getByText('Dune').closest('tr')!
    expect(row).toHaveTextContent('Frank Herbert')
    expect(row).toHaveTextContent('bad_format')
    expect(row).toHaveTextContent('plugin said no')
    expect(row).toHaveTextContent('8')

    fireEvent.click(screen.getByRole('button', { name: 'Retry failed' }))
    await waitFor(() => expect(mocked.calibreDeliveryRetry).toHaveBeenCalledWith('failed'))
    expect(await screen.findByText('1 queued again.')).toBeInTheDocument()
    // Refreshed after the action.
    await waitFor(() => expect(mocked.calibreDeliverySummary).toHaveBeenCalledTimes(2))
  })

  it('disables Retry failed when nothing failed', async () => {
    mocked.calibreDeliverySummary.mockResolvedValue({
      pending: 0, delivered: 1, failed: 0, skipped: 0, mode: 'plugin', target: {},
    })
    mocked.calibreDeliveries.mockResolvedValue({ items: [], total: 0 })
    render(<CalibreDeliveryPanel />)
    expect(await screen.findByText('Nothing has failed.')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Retry failed' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Clear waiting' })).toBeDisabled()
  })

  it('clears the waiting books', async () => {
    mocked.calibreDeliveryClearPending.mockResolvedValue({ cleared: 4 })
    render(<CalibreDeliveryPanel />)
    fireEvent.click(await screen.findByRole('button', { name: 'Clear waiting' }))
    await waitFor(() => expect(mocked.calibreDeliveryClearPending).toHaveBeenCalledTimes(1))
    expect(await screen.findByText('4 removed from the queue.')).toBeInTheDocument()
  })

  it('resets only after the confirm dialog, which says what it is for', async () => {
    mocked.calibreDeliveryReset.mockResolvedValue({ removed: 17 })
    render(<CalibreDeliveryPanel />)
    fireEvent.click(await screen.findByRole('button', { name: 'Reset delivery state' }))
    expect(mocked.calibreDeliveryReset).not.toHaveBeenCalled()

    const dialog = screen.getByRole('dialog')
    expect(dialog).toHaveTextContent('different Calibre library')

    fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(mocked.calibreDeliveryReset).not.toHaveBeenCalled()

    fireEvent.click(screen.getByRole('button', { name: 'Reset delivery state' }))
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Reset' }))
    await waitFor(() => expect(mocked.calibreDeliveryReset).toHaveBeenCalledTimes(1))
    expect(await screen.findByText('Delivery state reset. 17 records removed.')).toBeInTheDocument()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('shows an action error without losing the panel', async () => {
    mocked.calibreDeliveryClearPending.mockRejectedValue(new Error('admin role required'))
    render(<CalibreDeliveryPanel />)
    fireEvent.click(await screen.findByRole('button', { name: 'Clear waiting' }))
    expect(await screen.findByText('That did not work: admin role required')).toBeInTheDocument()
    expect(screen.getByTestId('calibre-delivery-summary')).toBeInTheDocument()
  })
})
