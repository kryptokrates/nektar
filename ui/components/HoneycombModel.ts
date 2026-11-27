export interface StageBar {
  id: string
  label: string
  badge?: string
  badgeColor?: string
  width: number
  hoverText: string
  dotColor?: string
  dimmed?: boolean
  highlighted?: boolean
  matched?: boolean
  onClick?: () => void
}

export interface StageHeartbeat {
  buy: number
  sell: number
  settled: boolean
}

export interface StageStatItem {
  label: string
  value: string
  color?: string
}

export interface StageArrow {
  id: string
  matched: boolean
  highlighted: boolean
  dimmed: boolean
  d: string
}

export interface Comet {
  id: number
  x: number
  y: number
  tail: string
  opacity: number
  color: string
}

export interface ActiveComet {
  id: number
  x0: number
  y0: number
  cx1: number
  cx2: number
  x1: number
  y1: number
  start: number
  dur: number
  color: string
  pts: Array<[number, number]>
  onArrive?: () => void
}

// Pixel-locked shared geometry (920 x 268 across Market & Agents — zero shift between tabs)
export const CORE_X = 460
export const CORE_Y = 114
export const RING_R = 70
export const RING_CIRC = 2 * Math.PI * RING_R

export function rowY(i: number): number {
  return 46 + i * 33
}

// Smooth C1 rounded-corner hexagon path (radius 60, corner fillet t=0.24)
export const SMOOTH_HEX_PATH =
  'M -12.47,-52.80 Q 0,-60 12.47,-52.80 L 39.49,-37.20 Q 51.96,-30 51.96,-15.60 L 51.96,15.60 Q 51.96,30 39.49,37.20 L 12.47,52.80 Q 0,60 -12.47,52.80 L -39.49,37.20 Q -51.96,30 -51.96,15.60 L -51.96,-15.60 Q -51.96,-30 -39.49,-37.20 Z'

export const NEKTAR_DROPLET_PATH =
  'M 195 55 C 178 55 164 63 157 80 C 153 100 149 112 143 124 C 136 139 115 158 93 186 C 80 206 73 226 72 250 C 71 277 79 306 98 329 C 117 352 150 372 190 377 C 232 380 255 356 259 322 C 261 296 274 273 296 251 C 310 234 318 210 318 186 C 318 158 312 137 302 120 C 292 102 276 84 260 74 C 245 64 218 55 195 55 Z'

export function buildLeftArrows(leftBars: StageBar[]): StageArrow[] {
  return leftBars.map((b, i) => {
    const x1 = 24 + b.width + 6
    const y1 = rowY(i) + 13
    const tx = CORE_X - RING_R - 12
    const ty = CORE_Y + (i - (leftBars.length - 1) / 2) * 11
    const c1x = x1 + (tx - x1) * 0.48
    const c2x = x1 + (tx - x1) * 0.62
    return {
      id: b.id,
      matched: Boolean(b.matched),
      highlighted: Boolean(b.highlighted),
      dimmed: Boolean(b.dimmed),
      d: `M ${x1.toFixed(1)} ${y1.toFixed(1)} C ${c1x.toFixed(1)} ${y1.toFixed(1)}, ${c2x.toFixed(1)} ${ty.toFixed(1)}, ${tx.toFixed(1)} ${ty.toFixed(1)}`
    }
  })
}

export function buildRightArrows(rightBars: StageBar[], flowMode: 'inward' | 'through'): StageArrow[] {
  return rightBars.map((b, i) => {
    const barEdgeX = 896 - b.width - 6
    const barY = rowY(i) + 13
    const coreEdgeX = CORE_X + RING_R + 12
    const coreEdgeY = CORE_Y + (i - (rightBars.length - 1) / 2) * 11

    if (flowMode === 'through') {
      const c1x = coreEdgeX + (barEdgeX - coreEdgeX) * 0.38
      const c2x = coreEdgeX + (barEdgeX - coreEdgeX) * 0.56
      return {
        id: b.id,
        matched: Boolean(b.matched),
        highlighted: Boolean(b.highlighted),
        dimmed: Boolean(b.dimmed),
        d: `M ${coreEdgeX.toFixed(1)} ${coreEdgeY.toFixed(1)} C ${c1x.toFixed(1)} ${coreEdgeY.toFixed(1)}, ${c2x.toFixed(1)} ${barY.toFixed(1)}, ${barEdgeX.toFixed(1)} ${barY.toFixed(1)}`
      }
    }
    const c1x = barEdgeX - (barEdgeX - coreEdgeX) * 0.48
    const c2x = barEdgeX - (barEdgeX - coreEdgeX) * 0.62
    return {
      id: b.id,
      matched: Boolean(b.matched),
      highlighted: Boolean(b.highlighted),
      dimmed: Boolean(b.dimmed),
      d: `M ${barEdgeX.toFixed(1)} ${barY.toFixed(1)} C ${c1x.toFixed(1)} ${barY.toFixed(1)}, ${c2x.toFixed(1)} ${coreEdgeY.toFixed(1)}, ${coreEdgeX.toFixed(1)} ${coreEdgeY.toFixed(1)}`
    }
  })
}

export function createActiveComet(
  id: number,
  side: 'left' | 'right',
  bars: StageBar[],
  flowMode: 'inward' | 'through',
  rowIdx: number | undefined,
  now: number,
  onArrive?: () => void
): ActiveComet {
  const isLeft = side === 'left'
  const idx = rowIdx !== undefined ? Math.min(Math.max(0, rowIdx), Math.max(0, bars.length - 1)) : 0
  const bar = bars[idx]
  const yBar = rowY(bar ? idx : 0) + 13
  const color = isLeft ? '#4cc26b' : '#f7991b'

  if (isLeft) {
    const x0 = bar ? 24 + bar.width : 160
    return { id, x0, y0: yBar, cx1: x0 + 96, cx2: 390, x1: CORE_X, y1: CORE_Y, start: now, dur: 620, color, pts: [], onArrive }
  }
  if (flowMode === 'through') {
    const x1 = bar ? 896 - bar.width : 760
    return { id, x0: CORE_X, y0: CORE_Y, cx1: 530, cx2: x1 - 96, x1, y1: yBar, start: now, dur: 620, color, pts: [], onArrive }
  }
  const x0 = bar ? 896 - bar.width : 760
  return { id, x0, y0: yBar, cx1: x0 - 96, cx2: 530, x1: CORE_X, y1: CORE_Y, start: now, dur: 620, color, pts: [], onArrive }
}

export function stepActiveComets(
  activeComets: ActiveComet[],
  now: number
): { surviving: ActiveComet[]; rendered: Comet[]; keepRunning: boolean } {
  const rendered: Comet[] = []
  const surviving: ActiveComet[] = []
  let keepRunning = false
  for (const c of activeComets) {
    const t = Math.min(1, (now - c.start) / c.dur)
    const ease = 1 - Math.pow(1 - t, 2.4)
    const u = 1 - ease
    const x = u * u * u * c.x0 + 3 * u * u * ease * c.cx1 + 3 * u * ease * ease * c.cx2 + ease * ease * ease * c.x1
    const y = u * u * u * c.y0 + 3 * u * u * ease * c.y0 + 3 * u * ease * ease * c.y1 + ease * ease * ease * c.y1
    c.pts.push([x, y])
    if (c.pts.length > 12) c.pts.shift()
    if (t < 1) {
      surviving.push(c)
      rendered.push({
        id: c.id,
        x,
        y,
        tail: c.pts.length > 1 ? c.pts.map((p, k) => `${k ? 'L' : 'M'}${p[0].toFixed(1)},${p[1].toFixed(1)}`).join(' ') : '',
        opacity: 1 - t * 0.28,
        color: c.color
      })
      keepRunning = true
    } else {
      c.onArrive?.()
    }
  }
  return { surviving, rendered, keepRunning }
}

export async function exportStageSnapshot(
  svgEl: SVGSVGElement | null,
  filename: string,
  onToast?: (msg: string, isErr?: boolean) => void
): Promise<void> {
  if (!svgEl) return
  try {
    const xml = new XMLSerializer().serializeToString(svgEl)
    const blob = new Blob([xml], { type: 'image/svg+xml;charset=utf-8' })
    const url = URL.createObjectURL(blob)
    const img = new Image()
    await new Promise<void>((resolve, reject) => {
      img.onload = () => resolve()
      img.onerror = reject
      img.src = url
    })
    const canvas = document.createElement('canvas')
    canvas.width = 1840
    canvas.height = 536
    const ctx = canvas.getContext('2d')
    if (!ctx) {
      URL.revokeObjectURL(url)
      return
    }
    ctx.fillStyle = '#090807'
    ctx.fillRect(0, 0, canvas.width, canvas.height)
    ctx.drawImage(img, 0, 0, canvas.width, canvas.height)
    URL.revokeObjectURL(url)

    const pngBlob = await new Promise<Blob | null>((resolve) => canvas.toBlob(resolve, 'image/png'))
    if (!pngBlob) return

    if (navigator.clipboard && 'ClipboardItem' in window) {
      try {
        await navigator.clipboard.write([new ClipboardItem({ 'image/png': pngBlob })])
        onToast?.('Share card copied as PNG ✓')
        return
      } catch {}
    }
    const dlUrl = URL.createObjectURL(pngBlob)
    const a = document.createElement('a')
    a.href = dlUrl
    a.download = filename
    a.click()
    URL.revokeObjectURL(dlUrl)
    onToast?.(`Share card saved (${filename}) ✓`)
  } catch {
    onToast?.('Could not export share card', true)
  }
}
