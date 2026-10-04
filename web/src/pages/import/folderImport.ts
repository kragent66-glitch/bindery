import type { Book, ScanItem } from '../../api/client'

// Shared by FolderImportView and FolderImportRow.

// RowState is the per-unit resolution the user is building, keyed by unit path.
export interface RowState {
  // chosen is the catalogue book the unit will import against (a confident
  // match, a picked candidate, or a searched existing book). null until resolved.
  chosen: Book | null
  // format override: '' = auto-detect, or 'ebook' / 'audiobook'. Defaults to the
  // scan's detected format.
  format: string
  // selected marks the unit for the next bulk import. Only meaningful when chosen.
  selected: boolean
}

export function groupHeading(match: ScanItem['match']): string {
  switch (match) {
    case 'confident': return 'Matched'
    case 'ambiguous': return 'Needs a choice'
    default: return 'Unmatched'
  }
}

// AUDIO_FILE matches the audio extensions the importer treats as audiobook
// tracks (internal/importer/mediatype.go).
const AUDIO_FILE = /\.(mp3|m4a|m4b|aac|flac|ogg|opus)$/i

function parentDir(path: string): string {
  const cut = Math.max(path.lastIndexOf('/'), path.lastIndexOf('\\'))
  return cut < 0 ? '' : path.slice(0, cut)
}

// trackFolders maps every audio file row to all the audio file rows in its
// folder, itself included: the tracks of one audiobook when the scan was
// pointed at the audiobook's own folder and listed each track as a row
// (#2935). Rows that are not audio files are absent.
export function trackFolders(items: ScanItem[]): Map<string, string[]> {
  const byDir = new Map<string, string[]>()
  for (const it of items) {
    if (!AUDIO_FILE.test(it.path)) continue
    const dir = parentDir(it.path)
    const group = byDir.get(dir)
    if (group) group.push(it.path)
    else byDir.set(dir, [it.path])
  }
  const out = new Map<string, string[]>()
  for (const group of byDir.values()) {
    for (const p of group) out.set(p, group)
  }
  return out
}
