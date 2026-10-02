import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import en from '../i18n/locales/en.json'

vi.mock('react-i18next', () => ({
  useTranslation: () => ({
    t: (key: string) => {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      let node: any = en
      for (const part of key.split('.')) node = node?.[part]
      return typeof node === 'string' ? node : key
    },
  }),
}))

vi.mock('../api/client', () => ({
  api: { bookCalibreState: vi.fn() },
}))

import { api } from '../api/client'
import CalibreDeliveryChip from './CalibreDeliveryChip'

const mocked = api as unknown as Record<string, ReturnType<typeof vi.fn>>

describe('CalibreDeliveryChip', () => {
  beforeEach(() => vi.clearAllMocks())

  it.each([
    ['pending', 'Waiting for Calibre'],
    ['delivered', 'In Calibre'],
    ['failed', 'Calibre failed'],
  ])('shows %s as %s', async (state, label) => {
    mocked.bookCalibreState.mockResolvedValue({ state })
    render(<CalibreDeliveryChip bookId={5} />)
    expect(await screen.findByTestId('calibre-delivery-chip')).toHaveTextContent(label)
    expect(mocked.bookCalibreState).toHaveBeenCalledWith(5)
  })

  it.each(['off', 'none', 'skipped'])('renders nothing for %s', async state => {
    mocked.bookCalibreState.mockResolvedValue({ state })
    const { container } = render(<CalibreDeliveryChip bookId={5} />)
    await waitFor(() => expect(mocked.bookCalibreState).toHaveBeenCalled())
    expect(container).toBeEmptyDOMElement()
  })

  it('renders nothing when the state cannot be read', async () => {
    mocked.bookCalibreState.mockRejectedValue(new Error('boom'))
    const { container } = render(<CalibreDeliveryChip bookId={5} />)
    await waitFor(() => expect(mocked.bookCalibreState).toHaveBeenCalled())
    expect(container).toBeEmptyDOMElement()
  })

  it('puts the failure detail an admin receives in the tooltip', async () => {
    mocked.bookCalibreState.mockResolvedValue({ state: 'failed', lastErrorCode: 'bad_format', lastError: 'nope' })
    render(<CalibreDeliveryChip bookId={5} />)
    expect(await screen.findByTestId('calibre-delivery-chip')).toHaveAttribute('title', 'bad_format: nope')
  })
})
