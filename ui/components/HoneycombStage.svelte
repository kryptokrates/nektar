<script module lang="ts">
  export type { StageBar, StageHeartbeat, StageStatItem } from './HoneycombModel'
</script>

<script lang="ts">
  import { onDestroy, onMount } from 'svelte'
  import type { ReputationVector6D } from '../lib/types'
  import HoneycombDefs from './HoneycombDefs.svelte'
  import HoneycombWings from './HoneycombWings.svelte'
  import HoneycombCoreStrip from './HoneycombCoreStrip.svelte'
  import {
    buildLeftArrows,
    buildRightArrows,
    createActiveComet,
    exportStageSnapshot,
    RING_CIRC,
    stepActiveComets,
    type ActiveComet,
    type Comet,
    type StageBar,
    type StageHeartbeat,
    type StageStatItem
  } from './HoneycombModel'

  interface Props {
    ariaLabel: string
    leftTitle: string
    rightTitle: string
    leftBars: StageBar[]
    rightBars: StageBar[]
    rightEmptyText?: string
    /** 'inward' = both wings point into core (Market); 'through' = left->core->right (Agents) */
    flowMode?: 'inward' | 'through'
    coreValue: string
    coreLabel: string
    /** [0..1] emerald share of the outer split ring; remainder is amber */
    ringLeftShare?: number
    ringMuted?: boolean
    live: boolean
    /** Bottom strip mode: either heartbeat bars (Market) or compact P&L stats (Agents) */
    heartbeats?: StageHeartbeat[]
    footerStats?: StageStatItem[]
    footerBrand?: string
    repVector?: ReputationVector6D | null
    repTrusted?: boolean
    repUntrusted?: boolean
    onHoverChange?: (side: 'left' | 'right' | null, id: string | null) => void
    onToast?: (msg: string, isErr?: boolean) => void
  }

  let {
    ariaLabel,
    leftTitle,
    rightTitle,
    leftBars,
    rightBars,
    rightEmptyText = '',
    flowMode = 'inward',
    coreValue,
    coreLabel,
    ringLeftShare = 0.5,
    ringMuted = false,
    live,
    heartbeats,
    footerStats,
    footerBrand = 'NEKTAR · BSV AGENT ECONOMY',
    repVector = null,
    repTrusted = false,
    repUntrusted = false,
    onHoverChange,
    onToast
  }: Props = $props()

  let svgEl: SVGSVGElement | null = null

  const leftArcLen = $derived(RING_CIRC * Math.min(0.92, Math.max(0.08, ringLeftShare)))
  const rightArcLen = $derived(RING_CIRC - leftArcLen)
  const leftArrows = $derived(buildLeftArrows(leftBars))
  const rightArrows = $derived(buildRightArrows(rightBars, flowMode))

  let hoverLabel = $state<string | null>(null)
  let hoverSide = $state<'left' | 'right' | null>(null)
  let hoverId = $state<string | null>(null)

  let comets = $state<Comet[]>([])
  let shockwaveR = $state(62)
  let shockwaveOpacity = $state(0)
  let flashText = $state('')
  let flashVisible = $state(false)

  let nextCometId = 1
  let rafId: number | null = null
  let activeComets: ActiveComet[] = []
  let activeShockStart: number | null = null
  let flashTimer: ReturnType<typeof setTimeout> | null = null
  let reducedMotion = false

  function stepAnim(now: number) {
    let keepRunning = false
    if (activeComets.length > 0) {
      const stepped = stepActiveComets(activeComets, now)
      activeComets = stepped.surviving
      comets = stepped.rendered
      keepRunning = stepped.keepRunning
    } else if (comets.length > 0) {
      comets = []
    }

    if (activeShockStart !== null) {
      const t = Math.min(1, (now - activeShockStart) / 720)
      shockwaveR = 62 + t * 50
      shockwaveOpacity = 0.88 * (1 - t)
      if (t < 1) {
        keepRunning = true
      } else {
        activeShockStart = null
        shockwaveOpacity = 0
      }
    }

    rafId = keepRunning ? requestAnimationFrame(stepAnim) : null
  }

  function ensureAnimLoop() {
    if (reducedMotion || rafId !== null) return
    rafId = requestAnimationFrame(stepAnim)
  }

  export function launchComet(side: 'left' | 'right', rowIdx?: number, onArrive?: () => void) {
    if (reducedMotion) {
      onArrive?.()
      return
    }
    const bars = side === 'left' ? leftBars : rightBars
    activeComets.push(
      createActiveComet(nextCometId++, side, bars, flowMode, rowIdx, performance.now(), onArrive)
    )
    ensureAnimLoop()
  }

  export function triggerShockwave(label: string) {
    if (!reducedMotion) {
      activeShockStart = performance.now()
      ensureAnimLoop()
    }
    flashText = label
    flashVisible = true
    if (flashTimer) clearTimeout(flashTimer)
    flashTimer = setTimeout(() => (flashVisible = false), 2400)
  }

  export async function shareSnapshot(filename = 'nektar-card.png') {
    await exportStageSnapshot(svgEl, filename, onToast)
  }

  function handleHover(side: 'left' | 'right' | null, id: string | null, label: string | null) {
    hoverSide = side
    hoverId = id
    hoverLabel = label
    onHoverChange?.(side, id)
  }

  function handleBarClick(side: 'left' | 'right', rowIdx: number, bar: StageBar) {
    launchComet(side, rowIdx)
    bar.onClick?.()
  }

  // Unique SVG ID prefix per component instance so multiple stages never collide
  const uid = `hc-${Math.random().toString(36).slice(2, 7)}`

  onMount(() => {
    reducedMotion = typeof window !== 'undefined' && window.matchMedia?.('(prefers-reduced-motion: reduce)').matches
  })

  onDestroy(() => {
    if (rafId !== null) cancelAnimationFrame(rafId)
    if (flashTimer) clearTimeout(flashTimer)
  })
</script>

<section class="pulse-stage" aria-label={ariaLabel}>
  <svg bind:this={svgEl} viewBox="0 0 920 268" class="svg" xmlns="http://www.w3.org/2000/svg">
    <HoneycombDefs {uid} />

    <!-- Pure Obsidian Canvas -->
    <rect width="920" height="268" fill="#090807" />
    <rect width="920" height="218" fill={`url(#${uid}-glow-l)`} />
    <rect width="920" height="218" fill={`url(#${uid}-glow-r)`} />
    <rect width="920" height="218" fill={`url(#${uid}-glow-c)`} />

    <!-- Minimal Wing Titles -->
    <text x="24" y="28" fill="#4cc26b" font-size="10.5" font-weight="800" letter-spacing="0.1em" font-family="system-ui, sans-serif">
      {leftTitle}
    </text>
    <text x="896" y="28" text-anchor="end" fill="#f7991b" font-size="10.5" font-weight="800" letter-spacing="0.1em" font-family="system-ui, sans-serif">
      {rightTitle}
    </text>

    <!-- Ephemeral Hover / Settlement Pill -->
    {#if hoverLabel || (flashVisible && flashText)}
      <g transform="translate(460, 22)">
        <rect x="-156" y="-11" width="312" height="22" rx="11" fill="#171412" stroke={flashVisible ? '#f7991b' : '#2f2a25'} />
        <text x="0" y="3.5" text-anchor="middle" fill={flashVisible ? '#ffd479' : '#f1ede9'} font-size="10.5" font-weight="600" font-family="ui-monospace, monospace">
          {flashVisible ? flashText : hoverLabel}
        </text>
      </g>
    {/if}

    <HoneycombWings
      {uid}
      {flowMode}
      {leftBars}
      {rightBars}
      {leftArrows}
      {rightArrows}
      {rightEmptyText}
      {hoverSide}
      {hoverId}
      onHover={handleHover}
      onBarClick={handleBarClick}
    />

    <!-- Kinetic Comets -->
    {#each comets as c (c.id)}
      {#if c.tail}
        <path d={c.tail} fill="none" stroke={c.color} stroke-width="2.4" stroke-linecap="round" opacity={c.opacity * 0.72} />
      {/if}
      <circle cx={c.x} cy={c.y} r="4.4" fill={c.color} opacity={c.opacity} filter={`url(#${uid}-neon)`} />
    {/each}

    <HoneycombCoreStrip
      {uid}
      {leftArcLen}
      {rightArcLen}
      {ringMuted}
      {shockwaveR}
      {shockwaveOpacity}
      {coreValue}
      {coreLabel}
      {live}
      {heartbeats}
      {footerStats}
      {footerBrand}
      {repVector}
      {repTrusted}
      {repUntrusted}
      onCoreHover={(lbl) => (hoverLabel = lbl)}
    />
  </svg>
</section>

<style>
  .pulse-stage {
    border-bottom: 1px solid var(--line);
    background: #090807;
  }
  .svg {
    display: block;
    width: 100%;
    height: auto;
    user-select: none;
  }
</style>
