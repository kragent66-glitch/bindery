// Non-component helpers shared between Settings tabs. Kept out of the
// component files so React Fast Refresh stays happy.

import type { DiagnoseResult } from '../../api/client'

// parseCats parses a comma-separated list of Newznab category IDs.
export function parseCats(s: string): number[] {
  return s.split(',').map(t => parseInt(t.trim(), 10)).filter(n => !isNaN(n))
}

// parsePriority parses an indexer priority (any integer; higher wins ties in
// release ranking). Blank or non-numeric falls back to 0.
export function parsePriority(s: string): number {
  const n = parseInt(s.trim(), 10)
  return isNaN(n) ? 0 : n
}

// rtorrentScgiIgnoredFields lists the fields the form is showing that the SCGI
// transport cannot use, or [] when none apply.
//
// rTorrent's own SCGI listener speaks neither TLS nor any authentication — that
// is the protocol, not a gap in Bindery. But the form still shows Use SSL, a
// username and a password, and Bindery silently drops all three when the URL
// base selects SCGI. An operator who fills them in gets a client that appears
// configured and secured and is neither, with nothing said at save time.
export function rtorrentScgiIgnoredFields(
  type: string,
  urlBase: string,
  useSSL: boolean,
  username: string,
  credential: string,
): string[] {
  if (type !== 'rtorrent') return []
  if (!urlBase.trim().toLowerCase().startsWith('scgi')) return []
  const ignored: string[] = []
  if (useSSL) ignored.push('Use SSL')
  if (username.trim()) ignored.push('Username')
  if (credential) ignored.push('Password')
  return ignored
}

// downloadClientPathRemapHelp returns the help text for the path-remap field
// of a given download-client type.
export function downloadClientPathRemapHelp(type: string) {
  if (type === 'qbittorrent') {
    return "Map the path qBittorrent reports to the path Bindery can read. Example: if qBittorrent shows /downloads/books but Bindery sees that folder at /media/books, use /downloads:/media/books. Bindery also uses this in reverse when sending new torrents."
  }
  if (type === 'rtorrent') {
    return "Map the path rTorrent reports to the path Bindery can read. Seedbox installs almost always need this — e.g. rTorrent writes to /home/user/downloads while Bindery mounts that share at /media/books, so use /home/user/downloads:/media/books. Bindery also uses this in reverse when setting a new torrent's download directory, and to locate files when you remove a download with its data."
  }
  return "Map the path this client reports to the path Bindery can read: the client's path, a colon, then Bindery's. Example: if the client shows /downloads/books but Bindery sees that folder at /media/books, use /downloads:/media/books. A Windows path works on the client side, such as S:\\Downloads:/downloads. Leave empty when both already see the same mount. Diagnose checks the mapping end to end."
}

// buildDiagnoseReport is the text behind "Copy report". It is built from an
// allow list of fields (client type, check codes and statuses, the three
// paths, the hardlink rows) so a report pasted into a public issue or Discord
// never carries the host, port, username or URL base, and never the server's
// sentences, which can quote the address.
export function buildDiagnoseReport(result: DiagnoseResult): string {
  const lines = [
    'Bindery download client diagnosis',
    `client type: ${result.clientType}`,
    ...result.checks.map(c => `${c.code}${c.mediaType ? ` (${c.mediaType})` : ''}: ${c.status}`),
    ...result.paths.flatMap(p => {
      const media = p.mediaType ? ` (${p.mediaType})` : ''
      return [
        `client path${media}: ${p.clientPath || '(none)'}${p.source ? ` from ${p.source}` : ''}`,
        `remap rule${media}: ${p.remapRule || '(none)'}`,
        `local path${media}: ${p.localPath || '(none)'}`,
      ]
    }),
    ...result.hardlinks.map(h => `hardlink ${h.downloadPath} to ${h.root}: ${h.result ?? (h.linkable ? 'yes' : 'no')}`),
  ]
  return lines.join('\n')
}
