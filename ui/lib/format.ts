export function fmtSats(n: number): string {
  return Math.round(n).toLocaleString('en-US')
}

export function shortHost(u: string): string {
  return u.replace(/^https?:\/\//, '').replace(/\/.*$/, '')
}

export function shortHash(h: string, head = 10, tail = 6): string {
  if (!h || h.length <= head + tail + 1) return h
  return `${h.slice(0, head)}…${h.slice(-tail)}`
}

export function agoSec(sec: number): string {
  if (sec < 60) return `${Math.max(0, Math.floor(sec))}s`
  const m = Math.floor(sec / 60)
  if (m < 60) return `${m}m`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h`
  return `${Math.floor(h / 24)}d`
}

export function agoMs(ms: number | null | undefined): string {
  if (!ms) return ''
  const sec = Math.max(0, Math.floor((Date.now() - ms) / 1000))
  return agoSec(sec)
}

export function agoUnix(unixSec: number | null | undefined): string {
  if (!unixSec) return ''
  return agoMs(unixSec * 1000)
}

export function sparklinePath(amounts: number[], w = 220, h = 44): { line: string; fill: string } {
  if (!amounts.length) return { line: '', fill: '' }
  const cum = amounts
    .slice()
    .reverse()
    .reduce<number[]>((acc, v, i) => [...acc, (acc[i - 1] ?? 0) + v], [])
  const max = Math.max(...cum, 1)
  const pts = cum.map((v, i) => [
    cum.length === 1 ? w / 2 : (i / (cum.length - 1)) * w,
    h - 4 - (v / max) * (h - 10)
  ] as const)
  const line = pts.map((p, i) => `${i ? 'L' : 'M'}${p[0].toFixed(1)},${p[1].toFixed(1)}`).join(' ')
  return { line, fill: `${line} L${w},${h} L0,${h} Z` }
}
