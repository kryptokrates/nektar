import type { RepoTreeEntry } from '../lib/types'

export const shortSha = (h?: string, n = 7) => (h ? h.slice(0, n) : '—')

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

export function bytesToBase64(bytes: Uint8Array): string {
  let bin = ''
  for (let i = 0; i < bytes.length; i += 0x8000) {
    bin += String.fromCharCode(...bytes.subarray(i, i + 0x8000))
  }
  return btoa(bin)
}

export function decodeInline(inline?: string): string {
  if (!inline) return ''
  try {
    const bin = atob(inline)
    const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0))
    return new TextDecoder().decode(bytes)
  } catch {
    return inline
  }
}

export function prettyContent(entry: RepoTreeEntry, text: string): string {
  if (entry.mediaType?.includes('json') || entry.path.endsWith('.json')) {
    try {
      return JSON.stringify(JSON.parse(text), null, 2)
    } catch {
      return text
    }
  }
  return text
}
