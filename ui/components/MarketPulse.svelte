<script lang="ts">
  import { onDestroy, onMount } from 'svelte'
  import type { MarketTapeEvent, PulseSignal, State } from '../lib/types'
  import { fmtSats } from '../lib/format'

  interface Props {
    s: State
    live: boolean
    pulse: PulseSignal | null
    onPickBuy: (domain: string, priceSats: number, hint?: string) => void
    onPickSell: (domain: string, rewardSats: number, hint?: string) => void
  }

  let { s, live, pulse, onPickBuy, onPickSell }: Props = $props()

  interface WingBar {
    id: string
    topic: string
    normTopic: string
    sats: number
    kind: string
    owner?: string
    note: string
    isSelf?: boolean
  }

  interface Comet {
    id: number
    x: number
    y: number
    tail: string
    opacity: number
    color: string
  }

  function cleanTopic(raw: string): string {
    const t = (raw || 'general').trim()
    if (t.startsWith('cluster:')) return t.slice('cluster:'.length)
    return t.replace(/^#/, '') || 'general'
  }

  function parseSats(offers: string): number {
    const m = String(offers || '').match(/(\d+)/)
    return m ? Number(m[1]) : 100
  }

  // Left Wing: Real Demand (Open Bounties + Unmatched Wants + Live Wants)
  const buyBars = $derived.by<WingBar[]>(() => {
    const out: WingBar[] = []
    const seen = new Set<string>()

    for (const b of s.net.bounties ?? []) {
      const nt = cleanTopic(b.domain)
      const key = `bounty:${b.id}`
      if (seen.has(key)) continue
      seen.add(key)
      out.push({
        id: key,
        topic: `#${nt}`,
        normTopic: nt.toLowerCase(),
        sats: b.rewardSats || 0,
        kind: 'Bounty',
        note: b.question || ''
      })
    }

    for (const uw of s.net.unmatchedWants ?? []) {
      const nt = cleanTopic(uw.domain)
      const key = `want:${uw.wantId}`
      if (seen.has(key)) continue
      seen.add(key)
      out.push({
        id: key,
        topic: `#${nt}`,
        normTopic: nt.toLowerCase(),
        sats: uw.maxSats || 0,
        kind: 'Bid',
        note: uw.query || ''
      })
    }

    for (const ev of s.marketTape ?? []) {
      if (ev.side !== 'buy') continue
      const nt = cleanTopic(ev.domains?.[0] || ev.shard || 'general')
      const key = `tape-b:${ev.id || nt}`
      if (seen.has(key)) continue
      seen.add(key)
      out.push({
        id: key,
        topic: `#${nt}`,
        normTopic: nt.toLowerCase(),
        sats: ev.sats || 0,
        kind: 'Buy',
        isSelf: ev.isSelf,
        note: ev.summary || ''
      })
    }

    return out.sort((a, b) => b.sats - a.sats).slice(0, 5)
  })

  // Right Wing: Real Supply (Your Listed Shards + Live Offers + Peer Sellers)
  const sellBars = $derived.by<WingBar[]>(() => {
    const out: WingBar[] = []
    const seen = new Set<string>()

    for (const m of s.market ?? []) {
      if (m.active === false) continue
      const nt = cleanTopic(m.domains?.[0] || m.shard)
      const key = `self:${m.shard}`
      seen.add(key)
      out.push({
        id: key,
        topic: `#${nt}`,
        normTopic: nt.toLowerCase(),
        sats: parseSats(m.offers),
        kind: 'Your shard',
        owner: 'you',
        isSelf: true,
        note: `${m.records} facts · ${m.works} sold`
      })
    }

    for (const ev of s.marketTape ?? []) {
      if (ev.side !== 'sell') continue
      const nt = cleanTopic(ev.domains?.[0] || ev.shard || 'general')
      const key = `tape-s:${ev.id || nt}`
      if (seen.has(key)) continue
      seen.add(key)
      out.push({
        id: key,
        topic: `#${nt}`,
        normTopic: nt.toLowerCase(),
        sats: ev.sats || 0,
        kind: 'Offer',
        owner: ev.isSelf ? 'you' : ev.pub ? `${ev.pub.slice(0, 8)}…` : 'peer',
        isSelf: ev.isSelf,
        note: ev.summary || ''
      })
    }

    for (const p of s.net.peers ?? []) {
      for (const dm of p.sellDomains ?? []) {
        const nt = cleanTopic(dm)
        const key = `peer:${p.pub}:${nt}`
        if (seen.has(key)) continue
        seen.add(key)
        out.push({
          id: key,
          topic: `#${nt}`,
          normTopic: nt.toLowerCase(),
          sats: 0,
          kind: p.isHub ? 'Hub' : 'Seller',
          owner: p.handle || `${p.pub.slice(0, 8)}…`,
          note: p.handle || p.pub.slice(0, 12)
        })
        if (out.length >= 8) break
      }
      if (out.length >= 8) break
    }

    return out.slice(0, 5)
  })

  const maxBuySats = $derived(Math.max(...buyBars.map((b) => b.sats), 100))
  const maxSellSats = $derived(Math.max(...sellBars.map((b) => b.sats), 100))
  const totalBuySats = $derived(buyBars.reduce((acc, b) => acc + b.sats, 0))
  const totalSellSats = $derived(sellBars.reduce((acc, b) => acc + b.sats, 0))

  const settledDeals = $derived((s.automation?.deals ?? []).filter((d) => d.to === 'settled'))
  const clearedSats = $derived.by(() => {
    const dealSum = settledDeals.reduce((acc, d) => acc + (d.priceSats ?? 0), 0)
    return Math.max(s.receipts.earned, dealSum)
  })

  const buyShare = $derived.by(() => {
    const b = totalBuySats || buyBars.length * 100
    const sl = totalSellSats || sellBars.length * 100
    if (b + sl === 0) return 0.5
    return Math.min(0.85, Math.max(0.15, b / (b + sl)))
  })

  const ringR = 68
  const ringCirc = 2 * Math.PI * ringR
  const buyArcLen = $derived(ringCirc * buyShare)
  const sellArcLen = $derived(ringCirc * (1 - buyShare))

  // Compact, airy geometry (viewBox 920 x 268)
  const coreX = 460
  const coreY = 114
  const rowY = (i: number) => 46 + i * 33
  const calcBuyW = (b: WingBar) => 116 + Math.round((Math.max(b.sats, maxBuySats * 0.25) / maxBuySats) * 144)
  const calcSellW = (b: WingBar) =>
    b.sats > 0 ? 116 + Math.round((Math.max(b.sats, maxSellSats * 0.25) / maxSellSats) * 144) : 140

  const bridges = $derived.by(() => {
    const list: Array<{ topic: string; d: string }> = []
    buyBars.forEach((b, bi) => {
      const si = sellBars.findIndex((sl) => sl.normTopic === b.normTopic)
      if (si === -1) return
      const x1 = 24 + calcBuyW(b)
      const y1 = rowY(bi) + 13
      const x2 = 896 - calcSellW(sellBars[si])
      const y2 = rowY(si) + 13
      list.push({
        topic: b.normTopic,
        d: `M ${x1} ${y1} C ${x1 + 90} ${y1}, 396 ${coreY}, ${coreX} ${coreY} C 524 ${coreY}, ${x2 - 90} ${y2}, ${x2} ${y2}`
      })
    })
    return list
  })

  interface HeartbeatSlot {
    buy: number
    sell: number
    settled: boolean
  }
  let liveBeats = $state<HeartbeatSlot[]>([])

  const heartbeatSlots = $derived.by<HeartbeatSlot[]>(() => {
    const base: HeartbeatSlot[] = []
    const tape = (s.marketTape ?? []).slice(0, 44).reverse()
    for (const ev of tape) {
      const mag = Math.min(16, Math.max(4, Math.round(Math.log10(Math.max(10, ev.sats || 25)) * 5.2)))
      const isSettled = ev.kind === 'deal' && ev.status === 'settled'
      base.push({
        buy: ev.side === 'buy' || isSettled ? mag : 0,
        sell: ev.side === 'sell' || isSettled ? mag : 0,
        settled: isSettled
      })
    }
    const merged = [...base, ...liveBeats].slice(-44)
    while (merged.length < 44) {
      merged.unshift({ buy: 0, sell: 0, settled: false })
    }
    return merged
  })

  let activeTopic = $state<string | null>(null)
  let hoverLabel = $state<string | null>(null)

  let comets = $state<Comet[]>([])
  let shockwaveR = $state(58)
  let shockwaveOpacity = $state(0)
  let flashText = $state('')
  let flashVisible = $state(false)
  let flashTimer: ReturnType<typeof setTimeout> | null = null

  let nextCometId = 1
  let rafId: number | null = null
  interface ActiveComet {
    id: number
    x0: number
    y0: number
    cx1: number
    cx2: number
    start: number
    dur: number
    color: string
    pts: Array<[number, number]>
    onArrive?: () => void
  }
  let activeComets: ActiveComet[] = []
  let activeShockStart: number | null = null
  let reducedMotion = false

  function stepAnim(now: number) {
    let keepRunning = false

    if (activeComets.length > 0) {
      const nextRendered: Comet[] = []
      const surviving: ActiveComet[] = []
      for (const c of activeComets) {
        const t = Math.min(1, (now - c.start) / c.dur)
        const ease = 1 - Math.pow(1 - t, 2.2)
        const u = 1 - ease
        const x = u * u * u * c.x0 + 3 * u * u * ease * c.cx1 + 3 * u * ease * ease * c.cx2 + ease * ease * ease * coreX
        const y = u * u * u * c.y0 + 3 * u * u * ease * c.y0 + 3 * u * ease * ease * coreY + ease * ease * ease * coreY
        c.pts.push([x, y])
        if (c.pts.length > 10) c.pts.shift()
        if (t < 1) {
          surviving.push(c)
          nextRendered.push({
            id: c.id,
            x,
            y,
            tail: c.pts.length > 1 ? c.pts.map((p, k) => `${k ? 'L' : 'M'}${p[0].toFixed(1)},${p[1].toFixed(1)}`).join(' ') : '',
            opacity: 1 - t * 0.3,
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
      const t = Math.min(1, (now - activeShockStart) / 700)
      shockwaveR = 58 + t * 48
      shockwaveOpacity = 0.9 * (1 - t)
      if (t < 1) {
        keepRunning = true
      } else {
        activeShockStart = null
        shockwaveOpacity = 0
      }
    }

    if (keepRunning) {
      rafId = requestAnimationFrame(stepAnim)
    } else {
      rafId = null
    }
  }

  function ensureAnimLoop() {
    if (reducedMotion || rafId !== null) return
    rafId = requestAnimationFrame(stepAnim)
  }

  function launchComet(side: 'buy' | 'sell', rowIdx?: number, onArrive?: () => void) {
    if (reducedMotion) {
      onArrive?.()
      return
    }
    const isBuy = side === 'buy'
    const bars = isBuy ? buyBars : sellBars
    const idx = rowIdx !== undefined ? Math.min(Math.max(0, rowIdx), Math.max(0, bars.length - 1)) : Math.floor(Math.random() * Math.max(1, bars.length))
    const bar = bars[idx]
    const x0 = isBuy ? (bar ? 24 + calcBuyW(bar) : 130) : (bar ? 896 - calcSellW(bar) : 790)
    const y0 = rowY(bar ? idx : 1) + 13
    activeComets.push({
      id: nextCometId++,
      x0,
      y0,
      cx1: isBuy ? x0 + 90 : x0 - 90,
      cx2: isBuy ? 396 : 524,
      start: performance.now(),
      dur: 600,
      color: isBuy ? '#4cc26b' : '#f7991b',
      pts: [],
      onArrive
    })
    ensureAnimLoop()
  }

  function triggerShockwave(label: string) {
    if (!reducedMotion) {
      activeShockStart = performance.now()
      ensureAnimLoop()
    }
    flashText = label
    flashVisible = true
    if (flashTimer) clearTimeout(flashTimer)
    flashTimer = setTimeout(() => (flashVisible = false), 2400)
  }

  function pushLiveBeat(side: 'buy' | 'sell' | 'clear', sats: number) {
    const mag = Math.min(16, Math.max(5, Math.round(Math.log10(Math.max(10, sats || 50)) * 5.2)))
    liveBeats = [
      ...liveBeats.slice(-24),
      {
        buy: side === 'buy' || side === 'clear' ? mag : 0,
        sell: side === 'sell' || side === 'clear' ? mag : 0,
        settled: side === 'clear'
      }
    ]
  }

  let lastPulseSeq = 0
  $effect(() => {
    if (!pulse || pulse.seq === lastPulseSeq) return
    lastPulseSeq = pulse.seq
    const ev = pulse.event
    const tape: MarketTapeEvent | undefined = pulse.tape
    const topic = tape?.domains?.[0] ? `#${cleanTopic(tape.domains[0])}` : tape?.shard ? `#${cleanTopic(tape.shard)}` : ''

    if (ev === 'deal_settled' || ev === 'receipt' || (tape?.kind === 'deal' && tape.status === 'settled')) {
      let hit = 0
      const onHit = () => {
        hit++
        if (hit === 2) {
          const amt = tape?.sats ? `+${fmtSats(tape.sats)} sats` : 'Cleared'
          triggerShockwave(`${amt}${topic ? ` · ${topic}` : ''}`)
          pushLiveBeat('clear', tape?.sats || 100)
        }
      }
      launchComet('buy', undefined, onHit)
      launchComet('sell', undefined, onHit)
    } else if (ev === 'market_want' || ev === 'tool:want' || tape?.side === 'buy') {
      launchComet('buy', 0)
      pushLiveBeat('buy', tape?.sats || 100)
    } else if (ev === 'market_offer' || ev === 'tool:sell' || ev === 'listing_updated' || ev === 'remember' || ev === 'anchor' || tape?.side === 'sell') {
      launchComet('sell', 0)
      pushLiveBeat('sell', tape?.sats || 100)
    }
  })

  onMount(() => {
    reducedMotion = typeof window !== 'undefined' && window.matchMedia?.('(prefers-reduced-motion: reduce)').matches
  })

  onDestroy(() => {
    if (rafId !== null) cancelAnimationFrame(rafId)
    if (flashTimer) clearTimeout(flashTimer)
  })
</script>

<section class="pulse-stage" aria-label="Live Market Pulse">
  <svg viewBox="0 0 920 268" class="svg" xmlns="http://www.w3.org/2000/svg">
    <defs>
      <radialGradient id="np-glow-buy" cx="14%" cy="42%" r="40%">
        <stop offset="0%" stop-color="#4cc26b" stop-opacity="0.12" />
        <stop offset="100%" stop-color="#4cc26b" stop-opacity="0" />
      </radialGradient>
      <radialGradient id="np-glow-sell" cx="86%" cy="42%" r="40%">
        <stop offset="0%" stop-color="#f7991b" stop-opacity="0.13" />
        <stop offset="100%" stop-color="#f7991b" stop-opacity="0" />
      </radialGradient>
      <radialGradient id="np-glow-core" cx="50%" cy="42%" r="20%">
        <stop offset="0%" stop-color="#f7991b" stop-opacity="0.18" />
        <stop offset="60%" stop-color="#4cc26b" stop-opacity="0.05" />
        <stop offset="100%" stop-color="#090807" stop-opacity="0" />
      </radialGradient>
      <linearGradient id="np-bar-buy" x1="0%" y1="0%" x2="100%" y2="0%">
        <stop offset="0%" stop-color="#4cc26b" stop-opacity="0.08" />
        <stop offset="100%" stop-color="#4cc26b" stop-opacity="0.42" />
      </linearGradient>
      <linearGradient id="np-bar-sell" x1="100%" y1="0%" x2="0%" y2="0%">
        <stop offset="0%" stop-color="#f7991b" stop-opacity="0.08" />
        <stop offset="100%" stop-color="#f7991b" stop-opacity="0.44" />
      </linearGradient>
      <linearGradient id="np-bridge" x1="0%" y1="0%" x2="100%" y2="0%">
        <stop offset="0%" stop-color="#4cc26b" />
        <stop offset="50%" stop-color="#ffd479" />
        <stop offset="100%" stop-color="#f7991b" />
      </linearGradient>
      <filter id="np-neon" x="-40%" y="-40%" width="180%" height="180%">
        <feGaussianBlur stdDeviation="2.8" result="blur" />
        <feMerge>
          <feMergeNode in="blur" />
          <feMergeNode in="SourceGraphic" />
        </feMerge>
      </filter>
    </defs>

    <!-- Pure Obsidian Canvas (no noisy grid or header bars) -->
    <rect width="920" height="268" fill="#090807" />
    <rect width="920" height="218" fill="url(#np-glow-buy)" />
    <rect width="920" height="218" fill="url(#np-glow-sell)" />
    <rect width="920" height="218" fill="url(#np-glow-core)" />

    <!-- Minimal Wing Titles (generous vertical clearance) -->
    <text x="24" y="28" fill="#4cc26b" font-size="10.5" font-weight="800" letter-spacing="0.1em" font-family="system-ui, sans-serif">
      BUYS
    </text>
    <text x="896" y="28" text-anchor="end" fill="#f7991b" font-size="10.5" font-weight="800" letter-spacing="0.1em" font-family="system-ui, sans-serif">
      SELLS
    </text>

    <!-- Center-Top Ephemeral Hover / Settlement Pill (open sky, zero line collision) -->
    {#if hoverLabel || (flashVisible && flashText)}
      <g transform="translate(460, 22)">
        <rect x="-150" y="-11" width="300" height="22" rx="11" fill="#171412" stroke={flashVisible ? '#f7991b' : '#2f2a25'} />
        <text x="0" y="3.5" text-anchor="middle" fill={flashVisible ? '#ffd479' : '#f1ede9'} font-size="10.5" font-weight="600" font-family="ui-monospace, monospace">
          {flashVisible ? flashText : hoverLabel}
        </text>
      </g>
    {/if}

    <!-- Topic Match Bridges -->
    {#each bridges as br}
      <path
        d={br.d}
        fill="none"
        stroke="url(#np-bridge)"
        stroke-width={activeTopic === br.topic ? 2.2 : 1.1}
        stroke-opacity={activeTopic ? (activeTopic === br.topic ? 0.92 : 0.08) : 0.25}
        class="bridge"
      />
    {/each}

    <!-- Left Wing (Buys) -->
    {#each buyBars as b, i}
      {@const y = rowY(i)}
      {@const w = calcBuyW(b)}
      {@const dim = activeTopic !== null && activeTopic !== b.normTopic}
      <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
      <g
        class="bar"
        opacity={dim ? 0.28 : 1}
        onmouseenter={() => {
          activeTopic = b.normTopic
          hoverLabel = `${b.topic} · ${b.sats ? `${fmtSats(b.sats)} sats` : b.kind} · click to sell`
        }}
        onmouseleave={() => {
          activeTopic = null
          hoverLabel = null
        }}
        onclick={() => {
          launchComet('buy', i)
          onPickSell(b.normTopic, b.sats || 100, b.note)
        }}
      >
        <rect
          x="24"
          {y}
          width={w}
          height="26"
          rx="6"
          fill="url(#np-bar-buy)"
          stroke={activeTopic === b.normTopic ? '#4cc26b' : '#1f3325'}
        />
        <circle cx={24 + w} cy={y + 13} r="3" fill="#4cc26b" />
        <text x="34" y={y + 17} fill="#f1ede9" font-size="11.5" font-weight="700" font-family="system-ui, sans-serif">
          {b.topic.slice(0, 16)}
        </text>
        <text x={24 + w - 10} y={y + 17} text-anchor="end" fill="#4cc26b" font-size="11" font-weight="800" font-family="ui-monospace, monospace">
          {b.sats > 0 ? `${fmtSats(b.sats)}s` : ''}
        </text>
      </g>
    {/each}

    <!-- Right Wing (Sells) -->
    {#each sellBars as b, i}
      {@const y = rowY(i)}
      {@const w = calcSellW(b)}
      {@const x = 896 - w}
      {@const dim = activeTopic !== null && activeTopic !== b.normTopic}
      <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
      <g
        class="bar"
        opacity={dim ? 0.28 : 1}
        onmouseenter={() => {
          activeTopic = b.normTopic
          hoverLabel = `${b.topic} · ${b.sats ? `${fmtSats(b.sats)} sats` : b.kind} · click to buy`
        }}
        onmouseleave={() => {
          activeTopic = null
          hoverLabel = null
        }}
        onclick={() => {
          launchComet('sell', i)
          onPickBuy(b.normTopic, b.sats || 100, b.note)
        }}
      >
        <rect
          {x}
          {y}
          width={w}
          height="26"
          rx="6"
          fill="url(#np-bar-sell)"
          stroke={activeTopic === b.normTopic ? '#f7991b' : '#382917'}
        />
        <circle cx={x} cy={y + 13} r="3" fill="#f7991b" />
        <text x={x + 10} y={y + 17} fill="#f7991b" font-size="11" font-weight="800" font-family="ui-monospace, monospace">
          {b.sats > 0 ? `${fmtSats(b.sats)}s` : ''}
        </text>
        <text x="886" y={y + 17} text-anchor="end" fill="#f1ede9" font-size="11.5" font-weight="700" font-family="system-ui, sans-serif">
          {b.isSelf ? `${b.topic.slice(0, 14)} ★` : b.topic.slice(0, 16)}
        </text>
      </g>
    {/each}

    <!-- Kinetic Comets -->
    {#each comets as c (c.id)}
      {#if c.tail}
        <path d={c.tail} fill="none" stroke={c.color} stroke-width="2.4" stroke-linecap="round" opacity={c.opacity * 0.75} />
      {/if}
      <circle cx={c.x} cy={c.y} r="4.5" fill={c.color} opacity={c.opacity} filter="url(#np-neon)" />
    {/each}

    <!-- Center Hexagonal Clearing Core (Ultra-Clean) -->
    <g transform={`translate(${coreX}, ${coreY})`}>
      <circle r={ringR} fill="none" stroke="#1e1b18" stroke-width="3.2" />
      <circle
        r={ringR}
        fill="none"
        stroke="#4cc26b"
        stroke-width="3.2"
        stroke-linecap="round"
        stroke-dasharray={`${buyArcLen.toFixed(1)} ${ringCirc.toFixed(1)}`}
        transform="rotate(90)"
        opacity="0.86"
      />
      <circle
        r={ringR}
        fill="none"
        stroke="#f7991b"
        stroke-width="3.2"
        stroke-linecap="round"
        stroke-dasharray={`${sellArcLen.toFixed(1)} ${ringCirc.toFixed(1)}`}
        stroke-dashoffset={`-${buyArcLen.toFixed(1)}`}
        transform="rotate(90)"
        opacity="0.9"
      />

      {#if shockwaveOpacity > 0.01}
        <circle r={shockwaveR} fill="none" stroke="#ffd479" stroke-width="2.2" opacity={shockwaveOpacity} />
      {/if}

      <polygon points="0,-58 50.2,-29 50.2,29 0,58 -50.2,29 -50.2,-29" fill="#11100e" stroke="#302b26" stroke-width="1.4" />
      <path
        fill="#f7991b"
        transform="translate(-7,-38) scale(0.036)"
        d="M 195 55 C 178 55 164 63 157 80 C 153 100 149 112 143 124 C 136 139 115 158 93 186 C 80 206 73 226 72 250 C 71 277 79 306 98 329 C 117 352 150 372 190 377 C 232 380 255 356 259 322 C 261 296 274 273 296 251 C 310 234 318 210 318 186 C 318 158 312 137 302 120 C 292 102 276 84 260 74 C 245 64 218 55 195 55 Z"
      />
      <text x="0" y="9" text-anchor="middle" fill="#f1ede9" font-size="22" font-weight="800" font-family="ui-monospace, monospace">
        {fmtSats(clearedSats)}
      </text>
      <text x="0" y="27" text-anchor="middle" fill="#7d746b" font-size="10" font-weight="700" letter-spacing="0.08em" font-family="system-ui, sans-serif">
        SATS CLEARED
      </text>
      <circle cx="0" cy="42" r="2.8" fill={live ? '#4cc26b' : '#e5534b'} class:live-dot={live} />
    </g>

    <!-- Silent Bottom Dual-Polarity Pulse Strip (Zero text clutter!) -->
    <line x1="0" y1="218" x2="920" y2="218" stroke="#1e1b18" stroke-width="1" />
    <rect x="0" y="219" width="920" height="49" fill="#0c0b09" />
    <line x1="24" y1="243" x2="896" y2="243" stroke="#1f1c19" stroke-width="1" />

    {#each heartbeatSlots as slot, i}
      {@const x = 24 + i * 20}
      {@const fade = 0.28 + (i / heartbeatSlots.length) * 0.68}
      {#if slot.buy > 0}
        <rect {x} y={242 - slot.buy} width="9" height={slot.buy} rx="2" fill="#4cc26b" opacity={fade} />
      {/if}
      {#if slot.sell > 0}
        <rect {x} y="244" width="9" height={slot.sell} rx="2" fill="#f7991b" opacity={fade} />
      {/if}
      {#if slot.settled}
        <polygon points={`${x + 4.5},238 ${x + 9},243 ${x + 4.5},248 ${x},243`} fill="#ffd479" filter="url(#np-neon)" />
      {/if}
    {/each}
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
  .bridge {
    transition: stroke-opacity 0.18s ease, stroke-width 0.18s ease;
  }
  @keyframes np-pulse {
    0%, 100% { opacity: 1; }
    50% { opacity: 0.35; }
  }
  .live-dot {
    animation: np-pulse 1.9s ease-in-out infinite;
  }
  @media (prefers-reduced-motion: reduce) {
    .live-dot { animation: none; }
  }
</style>
