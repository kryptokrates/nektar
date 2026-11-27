<script lang="ts">
  import { onDestroy, onMount } from 'svelte'
  import type { Simulation } from 'd3-force'
  import type { HubCatalogEntry } from '../lib/types'
  import HubMeshNodesLayer from './HubMeshNodesLayer.svelte'
  import HubMeshCoreFooter from './HubMeshCoreFooter.svelte'
  import {
    computeMeshMetrics,
    createMeshSimulation,
    exportMeshSnapshot,
    MESH_CX,
    MESH_CY,
    MESH_H,
    MESH_W,
    snapshotMeshPositions,
    type RenderLink,
    type RenderNode,
    type SimLink,
    type SimNode
  } from './HubMeshModel'

  interface Props {
    hubs: HubCatalogEntry[]
    network: string
    selectedURL: string
    activeTopic: string
    onSelectHub: (url: string) => void
    onToast?: (msg: string, isErr?: boolean) => void
  }

  let { hubs, network, selectedURL, activeTopic, onSelectHub, onToast }: Props = $props()

  let svgEl: SVGSVGElement | null = null

  let renderNodes = $state<RenderNode[]>([])
  let renderLinks = $state<RenderLink[]>([])
  let hoverURL = $state<string | null>(null)
  let draggingId = $state<string | null>(null)
  let reducedMotion = $state(false)

  let sim: Simulation<SimNode, SimLink> | null = null
  let simNodes: SimNode[] = []
  let simLinks: SimLink[] = []
  const posMemory = new Map<string, { x: number; y: number }>()

  const metrics = $derived(computeMeshMetrics(hubs))

  function syncPositions() {
    const snap = snapshotMeshPositions(simNodes, simLinks, posMemory)
    renderNodes = snap.renderNodes
    renderLinks = snap.renderLinks
  }

  function rebuildSimulation(list: HubCatalogEntry[]) {
    sim?.stop()
    const created = createMeshSimulation(list, posMemory, syncPositions)
    sim = created.sim
    simNodes = created.simNodes
    simLinks = created.simLinks
    // Pre-settle 32 ticks synchronously so the graph opens calm & crisp with zero chaotic bounce
    for (let k = 0; k < 32; k++) sim.tick()
    syncPositions()
  }

  // Rebuild physics whenever hub URLs or statuses update
  const hubSig = $derived(
    hubs.map((h) => `${h.url}:${h.connected}:${h.reachable}:${h.latencyMs}:${h.feePreset ?? ''}`).join('|')
  )
  $effect(() => {
    void hubSig
    rebuildSimulation(hubs)
  })

  // Smooth SVG Pointer Dragging
  function svgPoint(e: PointerEvent): { x: number; y: number } {
    if (!svgEl) return { x: MESH_CX, y: MESH_CY }
    const rect = svgEl.getBoundingClientRect()
    return {
      x: ((e.clientX - rect.left) / rect.width) * MESH_W,
      y: ((e.clientY - rect.top) / rect.height) * MESH_H
    }
  }

  function startDrag(e: PointerEvent, id: string) {
    const node = simNodes.find((n) => n.id === id)
    if (!node || node.kind === 'self') return
    draggingId = id
    onSelectHub(id)
    const pt = svgPoint(e)
    node.fx = pt.x
    node.fy = pt.y
    sim?.alphaTarget(0.25).restart()
    ;(e.currentTarget as Element).setPointerCapture?.(e.pointerId)
  }

  function moveDrag(e: PointerEvent) {
    if (!draggingId) return
    const node = simNodes.find((n) => n.id === draggingId)
    if (!node) return
    const pt = svgPoint(e)
    node.fx = Math.max(96, Math.min(MESH_W - 96, pt.x))
    node.fy = Math.max(66, Math.min(334, pt.y))
  }

  function endDrag() {
    if (!draggingId) return
    const node = simNodes.find((n) => n.id === draggingId)
    if (node) {
      node.fx = null
      node.fy = null
    }
    draggingId = null
    sim?.alphaTarget(0)
  }

  // 1-Click Share Card 2x PNG Export
  export async function shareSnapshot(filename = 'nektar-hub-mesh.png') {
    await exportMeshSnapshot(svgEl, filename, onToast)
  }

  const uid = `hm-${Math.random().toString(36).slice(2, 7)}`

  onMount(() => {
    reducedMotion = typeof window !== 'undefined' && window.matchMedia?.('(prefers-reduced-motion: reduce)').matches
  })

  onDestroy(() => {
    sim?.stop()
  })
</script>

<section class="mesh-stage" aria-label="Interactive LightBSV Hub Mesh">
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <svg
    bind:this={svgEl}
    viewBox={`0 0 ${MESH_W} ${MESH_H}`}
    class="svg"
    class:grabbing={draggingId !== null}
    xmlns="http://www.w3.org/2000/svg"
    onpointermove={moveDrag}
    onpointerup={endDrag}
    onpointerleave={endDrag}
  >
    <defs>
      <radialGradient id={`${uid}-glow-c`} cx="50%" cy="45%" r="36%">
        <stop offset="0%" stop-color="#f7991b" stop-opacity="0.16" />
        <stop offset="55%" stop-color="#4cc26b" stop-opacity="0.06" />
        <stop offset="100%" stop-color="#090807" stop-opacity="0" />
      </radialGradient>
      <radialGradient id={`${uid}-glow-l`} cx="16%" cy="35%" r="34%">
        <stop offset="0%" stop-color="#4cc26b" stop-opacity="0.09" />
        <stop offset="100%" stop-color="#4cc26b" stop-opacity="0" />
      </radialGradient>
      <radialGradient id={`${uid}-glow-r`} cx="84%" cy="55%" r="34%">
        <stop offset="0%" stop-color="#f7991b" stop-opacity="0.1" />
        <stop offset="100%" stop-color="#f7991b" stop-opacity="0" />
      </radialGradient>
      <linearGradient id={`${uid}-hex`} x1="0%" y1="0%" x2="0%" y2="100%">
        <stop offset="0%" stop-color="#1b1815" />
        <stop offset="100%" stop-color="#0e0d0b" />
      </linearGradient>
      <filter id={`${uid}-neon`} x="-50%" y="-50%" width="200%" height="200%">
        <feGaussianBlur stdDeviation="3" result="blur" />
        <feMerge>
          <feMergeNode in="blur" />
          <feMergeNode in="SourceGraphic" />
        </feMerge>
      </filter>
    </defs>

    <!-- Obsidian Canvas + Subtle Radar Rings -->
    <rect width={MESH_W} height={MESH_H} fill="#090807" />
    <rect width={MESH_W} height={382} fill={`url(#${uid}-glow-c)`} />
    <rect width={MESH_W} height={382} fill={`url(#${uid}-glow-l)`} />
    <rect width={MESH_W} height={382} fill={`url(#${uid}-glow-r)`} />

    <ellipse cx={MESH_CX} cy={MESH_CY} rx="140" ry="92" fill="none" stroke="#f1ede9" stroke-opacity="0.045" stroke-dasharray="3 6" />
    <ellipse cx={MESH_CX} cy={MESH_CY} rx="275" ry="150" fill="none" stroke="#f1ede9" stroke-opacity="0.05" stroke-dasharray="2 7" />

    <!-- Subtle Top-Left Legend -->
    <text x="24" y="26" fill="#4cc26b" font-size="10" font-weight="800" letter-spacing="0.11em" font-family="system-ui, sans-serif">
      LIVE MESH TOPOLOGY · DRAG NODES
    </text>
    <text x={MESH_W - 24} y="26" text-anchor="end" fill="#7d746b" font-size="10" font-weight="700" letter-spacing="0.08em" font-family="system-ui, sans-serif">
      {metrics.quorumOK ? 'MULTI-HUB QUORUM ACTIVE' : 'CONNECT 2+ HUBS FOR QUORUM'}
    </text>

    <HubMeshNodesLayer
      {uid}
      {hubs}
      {renderNodes}
      {renderLinks}
      {selectedURL}
      {hoverURL}
      {activeTopic}
      {reducedMotion}
      onStartDrag={startDrag}
      onHoverChange={(u) => (hoverURL = u)}
      {onSelectHub}
    />

    <HubMeshCoreFooter
      {uid}
      {network}
      connectedCount={metrics.connectedCount}
      totalHubs={hubs.length}
      quorumOK={metrics.quorumOK}
      avgPingMs={metrics.avgPingMs}
      totalTopics={metrics.totalTopics}
    />
  </svg>
</section>

<style>
  .mesh-stage {
    border-bottom: 1px solid var(--line);
    background: #090807;
  }
  .svg {
    display: block;
    width: 100%;
    height: auto;
    user-select: none;
    touch-action: none;
  }
  .svg.grabbing {
    cursor: grabbing;
  }
</style>
