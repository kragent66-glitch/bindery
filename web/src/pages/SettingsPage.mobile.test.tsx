import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { act, fireEvent, render, screen } from '@testing-library/react'
import SettingsPage from './SettingsPage'
import { mockMatchMedia } from '../test-utils'

vi.mock('../auth/AuthContext', () => ({
  useAuth: () => ({ isAdmin: true, status: { authenticated: true, role: 'admin' } }),
}))
vi.mock('react-i18next', () => ({
  useTranslation: () => ({
    t: (key: string) => key,
    i18n: { changeLanguage: vi.fn() },
  }),
}))
vi.mock('../api/client', async importOriginal => {
  const actual = await importOriginal<typeof import('../api/client')>()
  return {
    ...actual,
    api: {
      ...actual.api,
      listIndexers: vi.fn().mockResolvedValue([]),
      listDownloadClients: vi.fn().mockResolvedValue([]),
      listProwlarr: vi.fn().mockResolvedValue([]),
    },
  }
})
vi.mock('../components/ProtocolMismatchWarning', () => ({ default: () => null }))

// Stub every tab so the test is about the navigation, not the tabs.
vi.mock('./settings/GeneralTab', () => ({ default: () => <p>general tab</p> }))
vi.mock('./settings/AboutTab', () => ({ default: () => <p>about tab</p> }))
vi.mock('./settings/IndexersTab', () => ({ default: () => <p>indexers tab</p> }))
vi.mock('./settings/ClientsTab', () => ({ default: () => <p>clients tab</p> }))
vi.mock('./settings/NotificationsTab', () => ({ default: () => <p>notifications tab</p> }))
vi.mock('./settings/QualityTab', () => ({ default: () => <p>quality tab</p> }))
vi.mock('./settings/MetadataTab', () => ({ default: () => <p>metadata tab</p> }))
vi.mock('./settings/RootFoldersTab', () => ({ default: () => <p>rootfolders tab</p> }))
vi.mock('./settings/CalibreTab', () => ({ default: () => <p>calibre tab</p> }))
vi.mock('./settings/ABSTab', () => ({ default: () => <p>abs tab</p> }))
vi.mock('./settings/GrimmoryTab', () => ({ default: () => <p>grimmory tab</p> }))
vi.mock('./settings/ApiKeysTab', () => ({ default: () => <p>api keys tab</p> }))
vi.mock('./settings/ImportTab', () => ({ default: () => <p>import tab</p> }))
vi.mock('./settings/BlocklistTab', () => ({ default: () => <p>blocklist tab</p> }))
vi.mock('./settings/LogsTab', () => ({ default: () => <p>logs tab</p> }))
vi.mock('./settings/AdvancedTab', () => ({ default: () => <p>advanced tab</p> }))

let restore: () => void = () => {}

beforeEach(() => {
  window.history.replaceState(null, '', '/settings')
})
afterEach(() => {
  restore()
  vi.restoreAllMocks()
})

describe('SettingsPage navigation on a phone', () => {
  // Below md the 16 entry sidebar stacked above the content, so tapping a
  // tab changed something a screen further down and the tap looked dead.
  it('replaces the sidebar with a select below md', async () => {
    restore = mockMatchMedia(q => q.includes('48rem'))
    render(<SettingsPage />)
    const select = screen.getByRole('combobox', { name: 'settings.sectionLabel' })
    expect(screen.queryByRole('button', { name: 'settings.tabs.indexers' })).not.toBeInTheDocument()
    expect(await screen.findByText('general tab')).toBeInTheDocument()

    fireEvent.change(select, { target: { value: 'logs' } })
    expect(await screen.findByText('logs tab')).toBeInTheDocument()
    expect(select).toHaveValue('logs')
  })

  it('groups the select options under translated headings', () => {
    restore = mockMatchMedia(true)
    render(<SettingsPage />)
    const groups = Array.from(document.querySelectorAll('optgroup')).map(g => g.label)
    expect(groups).toEqual([
      'settings.groups.sources',
      'settings.groups.library',
      'settings.groups.integrations',
      'settings.groups.system',
    ])
  })

  it('keeps the sidebar from md up', async () => {
    restore = mockMatchMedia(false)
    render(<SettingsPage />)
    expect(screen.queryByRole('combobox', { name: 'settings.sectionLabel' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'settings.tabs.indexers' })).toBeInTheDocument()
    expect(await screen.findByText('general tab')).toBeInTheDocument()
  })

  it('translates the sidebar group headings and the Preview chip', () => {
    render(<SettingsPage />)
    for (const key of ['sources', 'library', 'integrations', 'system']) {
      expect(screen.getByText(`settings.groups.${key}`)).toBeInTheDocument()
    }
    expect(screen.getByText('settings.previewBadge')).toBeInTheDocument()
    expect(screen.queryByText('Sources')).not.toBeInTheDocument()
    expect(screen.queryByText('Preview')).not.toBeInTheDocument()
  })
})

describe('SettingsPage tab history', () => {
  // Android back left Settings entirely because tab changes replaced the
  // history entry instead of adding one.
  it('pushes a history entry per tab change', async () => {
    const push = vi.spyOn(window.history, 'pushState')
    const replace = vi.spyOn(window.history, 'replaceState')
    render(<SettingsPage />)
    fireEvent.click(screen.getByRole('button', { name: 'settings.tabs.logs' }))
    expect(await screen.findByText('logs tab')).toBeInTheDocument()
    expect(push).toHaveBeenCalledTimes(1)
    expect(String(push.mock.calls[0][2])).toContain('?tab=logs')
    expect(replace).not.toHaveBeenCalled()
  })

  it('does not push again when the active tab is picked', () => {
    const push = vi.spyOn(window.history, 'pushState')
    render(<SettingsPage />)
    fireEvent.click(screen.getByRole('button', { name: 'settings.tabs.general' }))
    expect(push).not.toHaveBeenCalled()
  })

  it('follows the URL back to the previous tab', async () => {
    render(<SettingsPage />)
    fireEvent.click(screen.getByRole('button', { name: 'settings.tabs.logs' }))
    expect(await screen.findByText('logs tab')).toBeInTheDocument()

    // What the browser does on back: restore the URL, then fire popstate.
    act(() => {
      window.history.replaceState(null, '', '/settings')
      window.dispatchEvent(new PopStateEvent('popstate'))
    })
    expect(await screen.findByText('general tab')).toBeInTheDocument()
  })

  it('still opens a deep linked tab', async () => {
    window.history.replaceState(null, '', '/settings?tab=calibre')
    render(<SettingsPage />)
    expect(await screen.findByText('calibre tab')).toBeInTheDocument()
  })
})
