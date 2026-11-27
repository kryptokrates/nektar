import type { RepoTreeEntry } from '../lib/types'

export interface DiffChange {
  kind: 'added' | 'modified' | 'deleted' | string
  path: string
  oldBlobHash?: string
  newBlobHash?: string
}

export const shortSha = (h?: string, n = 7) => (h ? h.slice(0, n) : '—')

export function decodeInline(inline?: string): string {
  if (!inline) return ''
  try {
    const bin = atob(inline)
    const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0))
    return new TextDecoder().decode(bytes)
  } catch {
    try {
      return atob(inline)
    } catch {
      return inline
    }
  }
}

export function prettyText(entry: RepoTreeEntry, txt: string): string {
  if (entry.mediaType?.includes('json') || entry.path.endsWith('.json')) {
    try {
      return JSON.stringify(JSON.parse(txt), null, 2)
    } catch {
      return txt
    }
  }
  return txt
}

export async function fileToBase64(file: File): Promise<string> {
  const buf = await file.arrayBuffer()
  const bytes = new Uint8Array(buf)
  let bin = ''
  for (let i = 0; i < bytes.length; i += 0x8000) {
    bin += String.fromCharCode(...bytes.subarray(i, i + 0x8000))
  }
  return btoa(bin)
}

export function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / (1024 * 1024)).toFixed(2)} MB`
}

export function firstLine(msg?: string): string {
  const s = (msg || 'Commit').trim()
  const nl = s.indexOf('\n')
  return nl >= 0 ? s.slice(0, nl) : s
}
