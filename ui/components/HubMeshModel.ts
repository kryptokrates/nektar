import {
  forceCollide,
  forceLink,
  forceManyBody,
  forceRadial,
  forceSimulation,
  type Simulation,
  type SimulationLinkDatum,
  type SimulationNodeDatum
} from 'd3-force'
import type { HubCatalogEntry } from '../lib/types'

export const MESH_W = 920
export const MESH_H = 432
export const MESH_CX = 460
export const MESH_CY = 186

export const MESH_HEX_PATH =
  'M -8.73,-36.96 Q 0,-42 8.73,-36.96 L 27.64,-26.04 Q 36.37,-21 36.37,-10.92 L 36.37,10.92 Q 36.37,21 27.64,26.04 L 8.73,36.96 Q 0,42 -8.73,36.96 L -27.64,26.04 Q -36.37,21 -36.37,10.92 L -36.37,-10.92 Q -36.37,-21 -27.64,-26.04 Z'

export const MESH_DROPLET_PATH =
  'M 195 55 C 178 55 164 63 157 80 C 153 100 149 112 143 124 C 136 139 115 158 93 186 C 80 206 73 226 72 250 C 71 277 79 306 98 329 C 117 352 150 372 190 377 C 232 380 255 356 259 322 C 261 296 274 273 296 251 C 310 234 318 210 318 186 C 318 158 312 137 302 120 C 292 102 276 84 260 74 C 245 64 218 55 195 55 Z'

export interface SimNode extends SimulationNodeDatum {
  id: string
  kind: 'self' | 'hub'
  hub?: HubCatalogEntry
  x: number
  y: number
  fx?: number | null
  fy?: number | null
}

export interface SimLink extends SimulationLinkDatum<SimNode> {
  id: string
  kind: 'mesh' | 'federation'
  source: string | SimNode
  target: string | SimNode
  connected: boolean
  latencyMs: number
}

export interface RenderNode {
  id: string
  kind: 'self' | 'hub'
  x: number
  y: number
  hub?: HubCatalogEntry
}

export interface RenderLink {
  id: string
  kind: 'mesh' | 'federation'
  x1: number
  y1: number
  x2: number
  y2: number
  d: string
  connected: boolean
  latencyMs: number
  sourceId: string
  targetId: string
}

export interface MeshMetrics {
  connectedCount: number
  quorumOK: boolean
  avgPingMs: number
  totalTopics: number
}

export function computeMeshMetrics(hubs: HubCatalogEntry[]): MeshMetrics {
  const connectedCount = hubs.filter((h) => h.connected && h.reachable).length
  const pings = hubs.filter((h) => h.reachable && h.latencyMs > 0).map((h) => h.latencyMs)
  const avgPingMs = pings.length ? Math.round(pings.reduce((a, b) => a + b, 0) / pings.length) : 0
  const topicSet = new Set<string>()
  for (const h of hubs) {
    for (const t of h.topics ?? []) topicSet.add(t.toLowerCase())
  }
  return {
    connectedCount,
    quorumOK: connectedCount >= 2,
    avgPingMs,
    totalTopics: topicSet.size
  }
}

export function feeChipText(h: HubCatalogEntry): string {
  if (h.fees) {
    const f = h.fees
    if (f.wantSats === 0 && f.offerSats === 0 && f.bboxSats === 0 && f.egressSatsPer100 === 0) return 'Free'
    if (f.wantSats > 0 || f.offerSats > 0) return `${f.wantSats}s/${f.offerSats}s`
    if (f.egressSatsPer100 > 0) return `${f.egressSatsPer100}s/100`
  }
  if (h.feePreset === 'free') return 'Free'
  if (h.feePreset === 'lightbsv_egress') return 'Egress'
  if (h.feePreset === 'hybrid') return '5s/2s'
  return ''
}

export function hubMatchesTopic(h: HubCatalogEntry | undefined, activeTopic: string): boolean {
  if (!activeTopic || activeTopic === '__all__' || !h) return true
  const want = activeTopic.toLowerCase()
  return (h.topics ?? []).some((t) => t.replace(/^cluster:/, '').toLowerCase() === want)
}

export function curvePath(x1: number, y1: number, x2: number, y2: number, bend: number): string {
  const mx = (x1 + x2) / 2
  const my = (y1 + y2) / 2
  const dx = x2 - x1
  const dy = y2 - y1
  const len = Math.hypot(dx, dy) || 1
  const nx = (-dy / len) * bend
  const ny = (dx / len) * bend
  return `M ${x1.toFixed(1)} ${y1.toFixed(1)} Q ${(mx + nx).toFixed(1)} ${(my + ny).toFixed(1)}, ${x2.toFixed(1)} ${y2.toFixed(1)}`
}

export function clampSimNode(n: SimNode): void {
  if (n.kind === 'self') {
    n.x = MESH_CX
    n.y = MESH_CY
    return
  }
  n.x = Math.max(96, Math.min(MESH_W - 96, n.x))
  n.y = Math.max(64, Math.min(314, n.y))
}

export function snapshotMeshPositions(
  simNodes: SimNode[],
  simLinks: SimLink[],
  posMemory: Map<string, { x: number; y: number }>
): { renderNodes: RenderNode[]; renderLinks: RenderLink[] } {
  for (const n of simNodes) {
    clampSimNode(n)
    if (n.kind === 'hub') posMemory.set(n.id, { x: n.x, y: n.y })
  }
  const renderNodes: RenderNode[] = simNodes.map((n) => ({
    id: n.id,
    kind: n.kind,
    x: n.x,
    y: n.y,
    hub: n.hub
  }))
  const renderLinks: RenderLink[] = simLinks.map((l, idx) => {
    const s = l.source as SimNode
    const t = l.target as SimNode
    const bend = l.kind === 'federation' ? (idx % 2 === 0 ? 28 : -28) : (idx % 2 === 0 ? 12 : -12)
    return {
      id: l.id,
      kind: l.kind,
      x1: s.x,
      y1: s.y,
      x2: t.x,
      y2: t.y,
      d: curvePath(s.x, s.y, t.x, t.y, bend),
      connected: l.connected,
      latencyMs: l.latencyMs,
      sourceId: s.id,
      targetId: t.id
    }
  })
  return { renderNodes, renderLinks }
}

export function createMeshSimulation(
  list: HubCatalogEntry[],
  posMemory: Map<string, { x: number; y: number }>,
  onTick: () => void
): { sim: Simulation<SimNode, SimLink>; simNodes: SimNode[]; simLinks: SimLink[] } {
  const total = Math.max(1, list.length)
  const nodes: SimNode[] = [{ id: '__self__', kind: 'self', x: MESH_CX, y: MESH_CY, fx: MESH_CX, fy: MESH_CY }]
  const hubByURL = new Map<string, HubCatalogEntry>()

  list.forEach((h, i) => {
    hubByURL.set(h.url, h)
    const prev = posMemory.get(h.url)
    // Start symmetrically on a spacious golden-ratio ellipse so even Frame 1 looks balanced
    const angle = (i / total) * Math.PI * 2 - Math.PI / 2 + (total === 6 ? Math.PI / 6 : 0)
    const rx = 252
    const ry = 114
    const x = prev ? prev.x : MESH_CX + Math.cos(angle) * rx
    const y = prev ? prev.y : MESH_CY + Math.sin(angle) * ry
    nodes.push({ id: h.url, kind: 'hub', hub: h, x, y })
  })

  const links: SimLink[] = []
  const fedSeen = new Set<string>()
  for (const h of list) {
    links.push({
      id: `self->${h.url}`,
      kind: 'mesh',
      source: '__self__',
      target: h.url,
      connected: h.connected && h.reachable,
      latencyMs: h.latencyMs || 40
    })
    for (const peerURL of h.linkedHubs ?? []) {
      if (!hubByURL.has(peerURL) || peerURL === h.url) continue
      const pairKey = h.url < peerURL ? `${h.url}|${peerURL}` : `${peerURL}|${h.url}`
      if (fedSeen.has(pairKey)) continue
      fedSeen.add(pairKey)
      links.push({
        id: `fed:${pairKey}`,
        kind: 'federation',
        source: h.url,
        target: peerURL,
        connected: h.reachable && Boolean(hubByURL.get(peerURL)?.reachable),
        latencyMs: 0
      })
    }
  }

  const sim = forceSimulation<SimNode>(nodes)
    .force(
      'link',
      forceLink<SimNode, SimLink>(links)
        .id((d) => d.id)
        .distance((l) => (l.kind === 'mesh' ? 182 : 210))
        .strength((l) => (l.kind === 'mesh' ? 0.3 : 0.08))
    )
    .force('charge', forceManyBody<SimNode>().strength(-280))
    .force('collide', forceCollide<SimNode>().radius((d) => (d.kind === 'self' ? 74 : 68)).iterations(3))
    .force(
      'radial',
      forceRadial<SimNode>(168, MESH_CX, MESH_CY).strength((d) => (d.kind === 'self' ? 0 : 0.16))
    )
    .alphaDecay(0.05)
    .on('tick', onTick)

  return { sim, simNodes: nodes, simLinks: links }
}

export async function exportMeshSnapshot(
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
    canvas.height = 864
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
        onToast?.('Mesh graph copied as PNG ✓')
        return
      } catch {}
    }
    const dlUrl = URL.createObjectURL(pngBlob)
    const a = document.createElement('a')
    a.href = dlUrl
    a.download = filename
    a.click()
    URL.revokeObjectURL(dlUrl)
    onToast?.(`Mesh graph saved (${filename}) ✓`)
  } catch {
    onToast?.('Could not export mesh card', true)
  }
}
