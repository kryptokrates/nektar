<script lang="ts">
  import { onDestroy, onMount } from 'svelte'
  import { fmtCompactSats } from '../lib/format'

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
    onHoverChange,
    onToast
  }: Props = $props()

  let svgEl: SVGSVGElement | null = null

  // Pixel-locked shared geometry (920 x 268 across Market & Agents — zero shift between tabs)
  const coreX = 460
  const coreY = 114
  const ringR = 70
  const ringCirc = 2 * Math.PI * ringR
  const rowY = (i: number) => 46 + i * 33

  const leftArcLen = $derived(ringCirc * Math.min(0.92, Math.max(0.08, ringLeftShare)))
  const rightArcLen = $derived(ringCirc - leftArcLen)

  // Smooth C1 rounded-corner hexagon path (radius 60, corner fillet t=0.24)
  const smoothHexPath =
    'M -12.47,-52.80 Q 0,-60 12.47,-52.80 L 39.49,-37.20 Q 51.96,-30 51.96,-15.60 L 51.96,15.60 Q 51.96,30 39.49,37.20 L 12.47,52.80 Q 0,60 -12.47,52.80 L -39.49,37.20 Q -51.96,30 -51.96,15.60 L -51.96,-15.60 Q -51.96,-30 -39.49,-37.20 Z'

  const leftArrows = $derived.by(() =>
    leftBars.map((b, i) => {
      const x1 = 24 + b.width + 6
      const y1 = rowY(i) + 13
      const tx = coreX - ringR - 12
      const ty = coreY + (i - (leftBars.length - 1) / 2) * 11
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
  )

  const rightArrows = $derived.by(() =>
    rightBars.map((b, i) => {
      const barEdgeX = 896 - b.width - 6
      const barY = rowY(i) + 13
      const coreEdgeX = coreX + ringR + 12
      const coreEdgeY = coreY + (i - (rightBars.length - 1) / 2) * 11

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
  )

  let hoverLabel = $state<string | null>(null)
  let hoverSide = $state<'left' | 'right' | null>(null)
  let hoverId = $state<string | null>(null)

  // Shared kinetic comet & shockwave engine
  interface Comet {
    id: number
    x: number
    y: number
    tail: string
    opacity: number
    color: string
  }
  interface ActiveComet {
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
      const nextRendered: Comet[] = []
      const surviving: ActiveComet[] = []
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
          nextRendered.push({
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
      activeComets = surviving
      comets = nextRendered
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
    const isLeft = side === 'left'
    const bars = isLeft ? leftBars : rightBars
    const idx = rowIdx !== undefined ? Math.min(Math.max(0, rowIdx), Math.max(0, bars.length - 1)) : 0
    const bar = bars[idx]
    const yBar = rowY(bar ? idx : 0) + 13
    const color = isLeft ? '#4cc26b' : '#f7991b'

    if (isLeft) {
      const x0 = bar ? 24 + bar.width : 160
      activeComets.push({
        id: nextCometId++,
        x0,
        y0: yBar,
        cx1: x0 + 96,
        cx2: 390,
        x1: coreX,
        y1: coreY,
        start: performance.now(),
        dur: 620,
        color,
        pts: [],
        onArrive
      })
    } else if (flowMode === 'through') {
      const x1 = bar ? 896 - bar.width : 760
      activeComets.push({
        id: nextCometId++,
        x0: coreX,
        y0: coreY,
        cx1: 530,
        cx2: x1 - 96,
        x1,
        y1: yBar,
        start: performance.now(),
        dur: 620,
        color,
        pts: [],
        onArrive
      })
    } else {
      const x0 = bar ? 896 - bar.width : 760
      activeComets.push({
        id: nextCometId++,
        x0,
        y0: yBar,
        cx1: x0 - 96,
        cx2: 530,
        x1: coreX,
        y1: coreY,
        start: performance.now(),
        dur: 620,
        color,
        pts: [],
        onArrive
      })
    }
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

  // Unique SVG ID prefix per component instance so multiple stages never collide
  const uid = `hc-${Math.random().toString(36).slice(2, 7)}`

  onMount(() => {
    reducedMotion = typeof window !== 'undefined' && window.matchMedia?.('(prefers-reduced-motion: reduce)').matches
  })

  onDestroy(() => {
    if (rafId !== null) cancelAnimationFrame(rafId)
    if (flashTimer) clearTimeout(flashTimer)
  })

  // Keep helper referenced cleanly
  void fmtCompactSats
</script>

<section class="pulse-stage" aria-label={ariaLabel}>
  <svg bind:this={svgEl} viewBox="0 0 920 268" class="svg" xmlns="http://www.w3.org/2000/svg">
    <defs>
      <radialGradient id={`${uid}-glow-l`} cx="14%" cy="42%" r="40%">
        <stop offset="0%" stop-color="#4cc26b" stop-opacity="0.12" />
        <stop offset="100%" stop-color="#4cc26b" stop-opacity="0" />
      </radialGradient>
      <radialGradient id={`${uid}-glow-r`} cx="86%" cy="42%" r="40%">
        <stop offset="0%" stop-color="#f7991b" stop-opacity="0.13" />
        <stop offset="100%" stop-color="#f7991b" stop-opacity="0" />
      </radialGradient>
      <radialGradient id={`${uid}-glow-c`} cx="50%" cy="42%" r="22%">
        <stop offset="0%" stop-color="#f7991b" stop-opacity="0.18" />
        <stop offset="60%" stop-color="#4cc26b" stop-opacity="0.05" />
        <stop offset="100%" stop-color="#090807" stop-opacity="0" />
      </radialGradient>
      <linearGradient id={`${uid}-hex`} x1="0%" y1="0%" x2="0%" y2="100%">
        <stop offset="0%" stop-color="#191613" />
        <stop offset="100%" stop-color="#0f0e0c" />
      </linearGradient>
      <linearGradient id={`${uid}-bar-l`} x1="0%" y1="0%" x2="100%" y2="0%">
        <stop offset="0%" stop-color="#4cc26b" stop-opacity="0.08" />
        <stop offset="100%" stop-color="#4cc26b" stop-opacity="0.42" />
      </linearGradient>
      <linearGradient id={`${uid}-bar-r`} x1="100%" y1="0%" x2="0%" y2="0%">
        <stop offset="0%" stop-color="#f7991b" stop-opacity="0.08" />
        <stop offset="100%" stop-color="#f7991b" stop-opacity="0.44" />
      </linearGradient>
      <!-- userSpaceOnUse guarantees crisp strokes even on purely horizontal single-row paths -->
      <linearGradient id={`${uid}-flow-l`} gradientUnits="userSpaceOnUse" x1="250" y1="0" x2="380" y2="0">
        <stop offset="0%" stop-color="#4cc26b" stop-opacity="0.24" />
        <stop offset="100%" stop-color="#4cc26b" stop-opacity="0.94" />
      </linearGradient>
      <linearGradient id={`${uid}-flow-r`} gradientUnits="userSpaceOnUse" x1="540" y1="0" x2="670" y2="0">
        <stop offset="0%" stop-color="#f7991b" stop-opacity="0.94" />
        <stop offset="100%" stop-color="#f7991b" stop-opacity="0.26" />
      </linearGradient>

      <marker id={`${uid}-arr-l`} viewBox="0 0 10 10" refX="7" refY="5" markerWidth="6.5" markerHeight="6.5" orient="auto">
        <path d="M 2 2 Q 5.2 4.3 7.5 5 Q 5.2 5.7 2 8" fill="none" stroke="#4cc26b" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" />
      </marker>
      <marker id={`${uid}-arr-r`} viewBox="0 0 10 10" refX="7" refY="5" markerWidth="6.5" markerHeight="6.5" orient="auto">
        <path d="M 2 2 Q 5.2 4.3 7.5 5 Q 5.2 5.7 2 8" fill="none" stroke="#f7991b" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" />
      </marker>
      <marker id={`${uid}-arr-l-dim`} viewBox="0 0 10 10" refX="7" refY="5" markerWidth="6" markerHeight="6" orient="auto">
        <path d="M 2 2 Q 5.2 4.3 7.5 5 Q 5.2 5.7 2 8" fill="none" stroke="#4cc26b" stroke-opacity="0.48" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" />
      </marker>
      <marker id={`${uid}-arr-r-dim`} viewBox="0 0 10 10" refX="7" refY="5" markerWidth="6" markerHeight="6" orient="auto">
        <path d="M 2 2 Q 5.2 4.3 7.5 5 Q 5.2 5.7 2 8" fill="none" stroke="#f7991b" stroke-opacity="0.48" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" />
      </marker>

      <filter id={`${uid}-neon`} x="-40%" y="-40%" width="180%" height="180%">
        <feGaussianBlur stdDeviation="2.8" result="blur" />
        <feMerge>
          <feMergeNode in="blur" />
          <feMergeNode in="SourceGraphic" />
        </feMerge>
      </filter>
    </defs>

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

    <!-- Left Curved Flow Arrows -->
    {#each leftArrows as a (a.id)}
      {@const focused = a.highlighted || (hoverSide === 'left' && hoverId === a.id)}
      {@const dimmed = a.dimmed || (hoverSide === 'left' && hoverId !== a.id)}
      <path
        d={a.d}
        fill="none"
        stroke={`url(#${uid}-flow-l)`}
        stroke-width={focused ? 2.1 : a.matched ? 1.6 : 1.35}
        stroke-linecap="round"
        stroke-opacity={dimmed ? 0.14 : focused ? 0.96 : a.matched ? 0.78 : 0.62}
        marker-end={focused || a.matched || flowMode === 'through' ? `url(#${uid}-arr-l)` : `url(#${uid}-arr-l-dim)`}
        class="flow-arrow"
      />
    {/each}

    <!-- Right Curved Flow Arrows -->
    {#each rightArrows as a (a.id)}
      {@const focused = a.highlighted || (hoverSide === 'right' && hoverId === a.id)}
      {@const dimmed = a.dimmed || (hoverSide === 'right' && hoverId !== a.id)}
      <path
        d={a.d}
        fill="none"
        stroke={`url(#${uid}-flow-r)`}
        stroke-width={focused ? 2.1 : a.matched ? 1.6 : 1.35}
        stroke-linecap="round"
        stroke-opacity={dimmed ? 0.14 : focused ? 0.96 : a.matched ? 0.78 : 0.65}
        marker-end={focused || a.matched || flowMode === 'through' ? `url(#${uid}-arr-r)` : `url(#${uid}-arr-r-dim)`}
        class="flow-arrow"
      />
    {/each}

    <!-- Left Wing Bars -->
    {#each leftBars as b, i (b.id)}
      {@const y = rowY(i)}
      {@const focused = b.highlighted || (hoverSide === 'left' && hoverId === b.id)}
      {@const dim = b.dimmed || (hoverSide === 'left' && hoverId !== b.id)}
      <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
      <g
        class="bar"
        opacity={dim ? 0.28 : 1}
        onmouseenter={() => {
          hoverSide = 'left'
          hoverId = b.id
          hoverLabel = b.hoverText
          onHoverChange?.('left', b.id)
        }}
        onmouseleave={() => {
          hoverSide = null
          hoverId = null
          hoverLabel = null
          onHoverChange?.(null, null)
        }}
        onclick={() => {
          launchComet('left', i)
          b.onClick?.()
        }}
      >
        <rect
          x="24"
          {y}
          width={b.width}
          height="26"
          rx="8"
          fill={`url(#${uid}-bar-l)`}
          stroke={focused ? '#4cc26b' : '#1f3325'}
          stroke-width={focused ? 1.3 : 1}
        />
        {#if b.dotColor}
          <circle cx="36" cy={y + 13} r="3.2" fill={b.dotColor} />
        {/if}
        <text x={b.dotColor ? 46 : 34} y={y + 17} fill="#f1ede9" font-size="11.5" font-weight="700" font-family="system-ui, sans-serif">
          {b.label.slice(0, 16)}
        </text>
        {#if b.badge}
          <text x={24 + b.width - 10} y={y + 17} text-anchor="end" fill={b.badgeColor || '#4cc26b'} font-size="11" font-weight="800" font-family="ui-monospace, monospace">
            {b.badge}
          </text>
        {/if}
      </g>
    {/each}

    <!-- Right Wing Bars -->
    {#if rightBars.length === 0 && rightEmptyText}
      <text x="760" y="118" text-anchor="middle" fill="#6d655d" font-size="11" font-family="system-ui, sans-serif">
        {rightEmptyText}
      </text>
    {:else}
      {#each rightBars as b, i (b.id)}
        {@const y = rowY(i)}
        {@const x = 896 - b.width}
        {@const focused = b.highlighted || (hoverSide === 'right' && hoverId === b.id)}
        {@const dim = b.dimmed || (hoverSide === 'right' && hoverId !== b.id)}
        <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
        <g
          class="bar"
          opacity={dim ? 0.28 : 1}
          onmouseenter={() => {
            hoverSide = 'right'
            hoverId = b.id
            hoverLabel = b.hoverText
            onHoverChange?.('right', b.id)
          }}
          onmouseleave={() => {
            hoverSide = null
            hoverId = null
            hoverLabel = null
            onHoverChange?.(null, null)
          }}
          onclick={() => {
            launchComet('right', i)
            b.onClick?.()
          }}
        >
          <rect
            {x}
            {y}
            width={b.width}
            height="26"
            rx="8"
            fill={`url(#${uid}-bar-r)`}
            stroke={focused ? '#f7991b' : '#382917'}
            stroke-width={focused ? 1.3 : 1}
          />
          {#if b.badge}
            <text x={x + 10} y={y + 17} fill={b.badgeColor || '#f7991b'} font-size="11" font-weight="800" font-family="ui-monospace, monospace">
              {b.badge}
            </text>
          {/if}
          <text x="886" y={y + 17} text-anchor="end" fill="#f1ede9" font-size="11.5" font-weight="700" font-family="system-ui, sans-serif">
            {b.label.slice(0, 16)}
          </text>
        </g>
      {/each}
    {/if}

    <!-- Kinetic Comets -->
    {#each comets as c (c.id)}
      {#if c.tail}
        <path d={c.tail} fill="none" stroke={c.color} stroke-width="2.4" stroke-linecap="round" opacity={c.opacity * 0.72} />
      {/if}
      <circle cx={c.x} cy={c.y} r="4.4" fill={c.color} opacity={c.opacity} filter={`url(#${uid}-neon)`} />
    {/each}

    <!-- Center Smooth Rounded Hexagon Core (Pixel-Locked at 460, 114) -->
    <g transform={`translate(${coreX}, ${coreY})`}>
      <circle r={ringR} fill="none" stroke="#1e1b18" stroke-width="3.2" />
      <circle
        r={ringR}
        fill="none"
        stroke="#4cc26b"
        stroke-width="3.2"
        stroke-linecap="round"
        stroke-dasharray={`${leftArcLen.toFixed(1)} ${ringCirc.toFixed(1)}`}
        transform="rotate(90)"
        opacity={ringMuted ? 0.45 : 0.86}
      />
      <circle
        r={ringR}
        fill="none"
        stroke="#f7991b"
        stroke-width="3.2"
        stroke-linecap="round"
        stroke-dasharray={`${rightArcLen.toFixed(1)} ${ringCirc.toFixed(1)}`}
        stroke-dashoffset={`-${leftArcLen.toFixed(1)}`}
        transform="rotate(90)"
        opacity={ringMuted ? 0.45 : 0.9}
      />

      {#if shockwaveOpacity > 0.01}
        <circle r={shockwaveR} fill="none" stroke="#ffd479" stroke-width="2.2" opacity={shockwaveOpacity} />
      {/if}

      <path d={smoothHexPath} fill={`url(#${uid}-hex)`} stroke="#332e29" stroke-width="1.4" stroke-linejoin="round" />
      <path d={smoothHexPath} transform="scale(0.92)" fill="none" stroke="#f7991b" stroke-opacity="0.14" stroke-width="1" />

      <path
        fill="#f7991b"
        transform="translate(-7,-38) scale(0.036)"
        d="M 195 55 C 178 55 164 63 157 80 C 153 100 149 112 143 124 C 136 139 115 158 93 186 C 80 206 73 226 72 250 C 71 277 79 306 98 329 C 117 352 150 372 190 377 C 232 380 255 356 259 322 C 261 296 274 273 296 251 C 310 234 318 210 318 186 C 318 158 312 137 302 120 C 292 102 276 84 260 74 C 245 64 218 55 195 55 Z"
      />
      <text x="0" y="9" text-anchor="middle" fill="#f1ede9" font-size="22" font-weight="800" font-family="ui-monospace, monospace">
        {coreValue}
      </text>
      <text x="0" y="27" text-anchor="middle" fill="#7d746b" font-size="10" font-weight="700" letter-spacing="0.08em" font-family="system-ui, sans-serif">
        {coreLabel}
      </text>
      <circle cx="0" cy="42" r="2.8" fill={live ? '#4cc26b' : '#e5534b'} class:live-dot={live} />
    </g>

    <!-- Bottom 49px Strip (identical height across Market & Agents) -->
    <line x1="0" y1="218" x2="920" y2="218" stroke="#1e1b18" stroke-width="1" />
    <rect x="0" y="219" width="920" height="49" fill="#0c0b09" />

    {#if heartbeats && heartbeats.length > 0}
      <line x1="24" y1="243" x2="896" y2="243" stroke="#1f1c19" stroke-width="1" />
      {#each heartbeats as slot, i}
        {@const x = 24 + i * 20}
        {@const fade = 0.28 + (i / heartbeats.length) * 0.68}
        {#if slot.buy > 0}
          <rect {x} y={242 - slot.buy} width="9" height={slot.buy} rx="2.5" fill="#4cc26b" opacity={fade} />
        {/if}
        {#if slot.sell > 0}
          <rect {x} y="244" width="9" height={slot.sell} rx="2.5" fill="#f7991b" opacity={fade} />
        {/if}
        {#if slot.settled}
          <circle cx={x + 4.5} cy="243" r="3.6" fill="#ffd479" filter={`url(#${uid}-neon)`} />
        {/if}
      {/each}
    {:else if footerStats && footerStats.length > 0}
      {#each footerStats as st, i}
        {@const x = 24 + i * 162}
        <text {x} y="247" fill="#7d746b" font-size="10.5" font-weight="700" letter-spacing="0.06em" font-family="system-ui, sans-serif">
          {st.label}
        </text>
        <text x={x + 56} y="247" fill={st.color || '#f1ede9'} font-size="13" font-weight="800" font-family="ui-monospace, monospace">
          {st.value}
        </text>
      {/each}
      <text x="896" y="247" text-anchor="end" fill="#5e5750" font-size="10.5" font-weight="700" letter-spacing="0.08em" font-family="system-ui, sans-serif">
        {footerBrand}
      </text>
    {/if}
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
  .bar {
    cursor: pointer;
    transition: opacity 0.16s ease;
  }
  .flow-arrow {
    transition: stroke-opacity 0.18s ease, stroke-width 0.18s ease;
  }
  @keyframes hc-pulse {
    0%, 100% { opacity: 1; }
    50% { opacity: 0.35; }
  }
  .live-dot {
    animation: hc-pulse 1.9s ease-in-out infinite;
  }
  @media (prefers-reduced-motion: reduce) {
    .live-dot { animation: none; }
  }
</style>
