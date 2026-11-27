export function fmtSats(n: number): string {
  return Math.round(n).toLocaleString('en-US')
}

export function fmtCompactSats(n: number): string {
  const sign = n < 0 ? '-' : ''
  const abs = Math.abs(n)
  if (abs < 10_000) return `${sign}${Math.round(abs).toLocaleString('en-US')}`
  if (abs < 1_000_000) return `${sign}${(abs / 1_000).toFixed(abs >= 100_000 ? 0 : 1)}K`
  if (abs < 1_000_000_000) return `${sign}${(abs / 1_000_000).toFixed(abs >= 100_000_000 ? 0 : 1)}M`
  return `${sign}${(abs / 1_000_000_000).toFixed(1)}B`
}

export function fmtTokens(n: number): string {
  const abs = Math.abs(n)
  if (abs < 1_000) return `${Math.round(n)}`
  if (abs < 1_000_000) return `${(n / 1_000).toFixed(abs >= 100_000 ? 0 : 1)}k`
  return `${(n / 1_000_000).toFixed(2)}M`
}

export function fmtUsdCents(cents: number): string {
  const sign = cents < 0 ? '-' : ''
  const abs = Math.abs(cents) / 100
  return `${sign}$${abs.toFixed(2)}`
}

export function fmtSatsUsd(sats: number, bsvUsd = 50): string {
  const sign = sats < 0 ? '-' : ''
  const usd = (Math.abs(sats) / 100_000_000) * bsvUsd
  if (usd === 0) return '$0.00'
  if (usd < 0.01) return `${sign}$${usd.toFixed(4)}`
  return `${sign}$${usd.toFixed(2)}`
}

export function fmtBpPct(bp: number): string {
  const clamped = Math.max(0, Math.min(10000, bp))
  return `${Math.round(clamped / 100)}%`
}

export function fmtSatsPerTriple(satsPer1kUnits: number): string {
  if (!Number.isFinite(satsPer1kUnits) || satsPer1kUnits <= 0) return '0'
  const whole = Math.floor(satsPer1kUnits / 1000)
  const frac = Math.round(satsPer1kUnits % 1000)
  if (frac === 0) return `${whole}`
  const pad = String(frac).padStart(3, '0').replace(/0+$/, '')
  return `${whole}.${pad}`
}

export function shortHost(u: string): string {
  return u.replace(/^https?:\/\//, '').replace(/\/.*$/, '')
}

export function shortHash(h: string, head = 10, tail = 6): string {
  if (!h || h.length <= head + tail + 1) return h
  return `${h.slice(0, head)}…${h.slice(-tail)}`
}

function agoSec(sec: number): string {
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
