import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, render, screen } from '@testing-library/react'
import CalendarPage from './CalendarPage'
import { api } from '../api/client'
import type { Book } from '../api/client'
import i18n from '../i18n'

vi.mock('../api/client', async importOriginal => {
  const actual = await importOriginal<typeof import('../api/client')>()
  return { ...actual, api: { ...actual.api, listAllBooks: vi.fn() } }
})

const release = { id: 1, title: 'Spring Release', releaseDate: '2026-03-05T00:00:00Z', monitored: true } as Book

// Month and weekday names used to be a hardcoded English array, so the
// calendar read "March" and "Mon" in every language.
describe('CalendarPage month names', () => {
  beforeEach(() => {
    // Only Date is faked: timers stay real so findBy* can poll.
    vi.useFakeTimers({ toFake: ['Date'] })
    vi.setSystemTime(new Date('2026-03-15T12:00:00Z'))
    vi.mocked(api.listAllBooks).mockResolvedValue([release])
  })

  afterEach(async () => {
    vi.useRealTimers()
    await act(async () => { await i18n.changeLanguage('en') })
  })

  it('renders English month and weekday names in English', async () => {
    await act(async () => { await i18n.changeLanguage('en') })
    render(<CalendarPage />)
    expect(await screen.findByText('March 2026')).toBeInTheDocument()
    expect(await screen.findByText('Releasing in March 2026')).toBeInTheDocument()
    expect(screen.getByText('Mar 5')).toBeInTheDocument()
    expect(screen.getByText('Mon')).toBeInTheDocument()
  })

  it('follows the active language', async () => {
    await act(async () => { await i18n.changeLanguage('fr') })
    render(<CalendarPage />)
    expect(await screen.findByText('mars 2026')).toBeInTheDocument()
    expect(await screen.findByText('Sorties en mars 2026')).toBeInTheDocument()
    expect(screen.getByText('5 mars')).toBeInTheDocument()
    expect(screen.getByText('lun.')).toBeInTheDocument()
    expect(screen.queryByText('March 2026')).not.toBeInTheDocument()
  })
})
