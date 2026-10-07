import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import FixMatchModal from './FixMatchModal'
import { api } from '../api/client'
import type { Book } from '../api/client'

// The component passes an English default to every t() call, so returning the
// default keeps the test asserting on the real copy rather than key names.
const tMock = vi.hoisted(() => (key: string, fallback?: string) => fallback ?? key)

vi.mock('react-i18next', () => ({
  useTranslation: () => ({ t: tMock }),
}))

vi.mock('../api/client', async importOriginal => {
  const actual = await importOriginal<typeof import('../api/client')>()
  return {
    ...actual,
    api: {
      ...actual.api,
      listBooks: vi.fn(),
      reassignFile: vi.fn(),
      reassignFilePreview: vi.fn(),
    },
  }
})

const SRC = '/library/My Own Layout/some-file.epub'
const DEST = '/library/Jane Doe/Right Book (2020)/Right Book - Jane Doe.epub'

function book(overrides: Partial<Book> = {}): Book {
  return {
    id: 7,
    foreignBookId: 'OL1W',
    authorId: 3,
    title: 'Right Book',
    description: '',
    imageUrl: '',
    genres: [],
    monitored: true,
    status: 'imported',
    filePath: '',
    mediaType: 'ebook',
    ebookFilePath: '',
    audiobookFilePath: '',
    excluded: false,
    ...overrides,
  } as Book
}

function renderModal(onReassigned = vi.fn()) {
  render(
    <FixMatchModal
      sourceBookId={1}
      path={SRC}
      format="ebook"
      onClose={() => {}}
      onReassigned={onReassigned}
    />,
  )
  return onReassigned
}

// pickCandidate types a search term and clicks the single result.
async function pickCandidate() {
  fireEvent.change(screen.getByPlaceholderText(/Search your library/), { target: { value: 'right' } })
  const candidate = await screen.findByRole('button', { name: /Right Book/ }, { timeout: 2000 })
  fireEvent.click(candidate)
}

describe('FixMatchModal', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.mocked(api.listBooks).mockResolvedValue({ items: [book()], total: 1 } as never)
    vi.mocked(api.reassignFilePreview).mockResolvedValue({
      source: SRC,
      destination: DEST,
      format: 'ebook',
      status: 'move',
    })
    vi.mocked(api.reassignFile).mockResolvedValue({ id: 99 })
  })

  it('does not reassign when a candidate is picked; it asks for confirmation first', async () => {
    renderModal()
    await pickCandidate()

    // The confirmation step is up...
    await screen.findByText(/This moves and renames the file on disk/)
    // ...and nothing has been committed.
    expect(api.reassignFile).not.toHaveBeenCalled()
  })

  it('shows the destination path the file will be moved to', async () => {
    renderModal()
    await pickCandidate()

    await waitFor(() =>
      expect(api.reassignFilePreview).toHaveBeenCalledWith({
        path: SRC,
        targetBookId: 7,
        format: 'ebook',
      }),
    )
    expect(await screen.findByText(DEST)).toBeInTheDocument()
    expect(screen.getByText('It will be moved to')).toBeInTheDocument()
    expect(screen.getByText(SRC, { selector: 'dd' })).toBeInTheDocument()
  })

  it('warns that the move is not undoable from the UI', async () => {
    renderModal()
    await pickCandidate()

    expect(
      await screen.findByText(/cannot undo it for you/),
    ).toBeInTheDocument()
  })

  it('only reassigns once the confirm button is clicked', async () => {
    const onReassigned = renderModal()
    await pickCandidate()

    const confirm = await screen.findByRole('button', { name: 'Move and reassign' })
    await waitFor(() => expect(confirm).not.toBeDisabled())
    fireEvent.click(confirm)

    await waitFor(() =>
      expect(api.reassignFile).toHaveBeenCalledWith({ path: SRC, targetBookId: 7, format: 'ebook' }),
    )
    await waitFor(() => expect(onReassigned).toHaveBeenCalledWith(7))
  })

  it('lets the user back out of the confirmation without reassigning', async () => {
    renderModal()
    await pickCandidate()

    fireEvent.click(await screen.findByRole('button', { name: 'Choose a different book' }))

    // Back to the search step, still nothing committed.
    expect(screen.getByPlaceholderText(/Search your library/)).toBeInTheDocument()
    expect(api.reassignFile).not.toHaveBeenCalled()
  })

  it('says only the metadata changes when the file is already at the templated path', async () => {
    vi.mocked(api.reassignFilePreview).mockResolvedValue({
      source: SRC,
      destination: SRC,
      format: 'ebook',
      status: 'noop',
    })
    renderModal()
    await pickCandidate()

    expect(await screen.findByText(/only the metadata link changes/)).toBeInTheDocument()
    expect(screen.queryByText(/This moves and renames the file on disk/)).not.toBeInTheDocument()
  })

  it('still warns about the move when the destination cannot be computed', async () => {
    vi.mocked(api.reassignFilePreview).mockRejectedValue(new Error('book not found'))
    renderModal()
    await pickCandidate()

    expect(await screen.findByText(/could not work out the destination/i)).toBeInTheDocument()
    expect(screen.getByText(/This moves and renames the file on disk/)).toBeInTheDocument()
    expect(api.reassignFile).not.toHaveBeenCalled()
  })

  // A fixed 5rem top offset plus an uncapped panel pushed the footer under
  // the browser toolbar on a phone.
  it('caps the panel on a phone and scrolls its body', () => {
    renderModal()
    const dialog = screen.getByRole('dialog')
    expect(dialog.className).toContain('modal-max-h')
    expect(dialog.className).toContain('flex flex-col')
    const overlay = dialog.parentElement!
    expect(overlay.className).toContain('sm:pt-20')
    expect(overlay.className.split(/\s+/)).not.toContain('pt-20')
    const body = dialog.children[1] as HTMLElement
    expect(body.className).toContain('overflow-y-auto')
    expect(body.className).toContain('min-h-0')
  })
})
