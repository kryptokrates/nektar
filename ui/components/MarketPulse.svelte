<script lang="ts">
  import type { MarketTapeEvent, PulseSignal, State } from '../lib/types'
  import { fmtSats } from '../lib/format'
  import { selectCoreRepVector } from '../lib/reputationHex'
  import HoneycombStage, { type StageBar, type StageHeartbeat } from './HoneycombStage.svelte'

  interface Props {
    s: State
    live: boolean
    pulse: PulseSignal | null
    onPickBuy: (domain: string, priceSats: number, hint?: string) => void
    onPickSell: (domain: string, rewardSats: number, hint?: string) => void
  }

  let { s, live, pulse, onPickBuy, onPickSell }: Props = $props()

  const coreRep = $derived(selectCoreRepVector(s.reputation, s.pub))

  interface WingBar {
    id: string
    topic: string
    normTopic: string
    sats: number
    kind: string
    note: string
    isSelf?: boolean
  }

  let stageRef: {
    launchComet: (side: 'left' | 'right', rowIdx?: number, onArrive?: () => void) => void
    triggerShockwave: (label: string) => void
  } | null = null

  function cleanTopic(raw: string): string {
    const t = (raw || 'general').trim()
    if (t.startsWith('cluster:')) return t.slice('cluster:'.length)
    return t.replace(/^#/, '') || 'general'
  }

  function parseSats(offers: string): number {
    const m = String(offers || '').match(/(\d+)/)
    return m ? Number(m[1]) : 100
  }

  const buyBars = $derived.by<WingBar[]>(() => {
    const out: WingBar[] = []
    const seen = new Set<string>()
    for (const b of s.net.bounties ?? []) {
      const nt = cleanTopic(b.domain)
      const key = `bounty:${b.id}`
      if (seen.has(key)) continue
      seen.add(key)
      out.push({ id: key, topic: `#${nt}`, normTopic: nt.toLowerCase(), sats: b.rewardSats || 0, kind: 'Bounty', note: b.question || '' })
    }
    for (const uw of s.net.unmatchedWants ?? []) {
      const nt = cleanTopic(uw.domain)
      const key = `want:${uw.wantId}`
      if (seen.has(key)) continue
      seen.add(key)
      out.push({ id: key, topic: `#${nt}`, normTopic: nt.toLowerCase(), sats: uw.maxSats || 0, kind: 'Bid', note: uw.query || '' })
    }
    for (const ev of s.marketTape ?? []) {
      if (ev.side !== 'buy') continue
      const nt = cleanTopic(ev.domains?.[0] || ev.shard || 'general')
      const key = `tape-b:${ev.id || nt}`
      if (seen.has(key)) continue
      seen.add(key)
      out.push({ id: key, topic: `#${nt}`, normTopic: nt.toLowerCase(), sats: ev.sats || 0, kind: 'Buy', isSelf: ev.isSelf, note: ev.summary || '' })
    }
    return out.sort((a, b) => b.sats - a.sats).slice(0, 5)
  })

  const sellBars = $derived.by<WingBar[]>(() => {
    const out: WingBar[] = []
    const seen = new Set<string>()
    for (const m of s.market ?? []) {
      if (m.active === false) continue
      const nt = cleanTopic(m.domains?.[0] || m.shard)
      const key = `self:${m.shard}`
      seen.add(key)
      out.push({ id: key, topic: `#${nt}`, normTopic: nt.toLowerCase(), sats: parseSats(m.offers), kind: 'Your shard', isSelf: true, note: `${m.records} facts · ${m.works} sold` })
    }
    for (const ev of s.marketTape ?? []) {
      if (ev.side !== 'sell') continue
      const nt = cleanTopic(ev.domains?.[0] || ev.shard || 'general')
      const key = `tape-s:${ev.id || nt}`
      if (seen.has(key)) continue
      seen.add(key)
      out.push({ id: key, topic: `#${nt}`, normTopic: nt.toLowerCase(), sats: ev.sats || 0, kind: 'Offer', isSelf: ev.isSelf, note: ev.summary || '' })
    }
    for (const p of s.net.peers ?? []) {
      for (const dm of p.sellDomains ?? []) {
        const nt = cleanTopic(dm)
        const key = `peer:${p.pub}:${nt}`
        if (seen.has(key)) continue
        seen.add(key)
        out.push({ id: key, topic: `#${nt}`, normTopic: nt.toLowerCase(), sats: 0, kind: p.isHub ? 'Hub' : 'Seller', note: p.handle || p.pub.slice(0, 12) })
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

  let activeTopic = $state<string | null>(null)
  let liveBeats = $state<StageHeartbeat[]>([])

  const heartbeatSlots = $derived.by<StageHeartbeat[]>(() => {
    const base: StageHeartbeat[] = []
    const tape = (s.marketTape ?? []).slice(0, 44).reverse()
    for (const ev of tape) {
      const mag = Math.min(16, Math.max(4, Math.round(Math.log10(Math.max(10, ev.sats || 25)) * 5.2)))
      const isSettled = ev.kind === 'deal' && ev.status === 'settled'
      base.push({ buy: ev.side === 'buy' || isSettled ? mag : 0, sell: ev.side === 'sell' || isSettled ? mag : 0, settled: isSettled })
    }
    const merged = [...base, ...liveBeats].slice(-44)
    while (merged.length < 44) merged.unshift({ buy: 0, sell: 0, settled: false })
    return merged
  })

  const leftStageBars = $derived<StageBar[]>(
    buyBars.map((b) => ({
      id: b.id,
      label: b.topic,
      badge: b.sats > 0 ? `${fmtSats(b.sats)}s` : undefined,
      width: 116 + Math.round((Math.max(b.sats, maxBuySats * 0.25) / maxBuySats) * 138),
      hoverText: `${b.topic} · ${b.sats ? `${fmtSats(b.sats)} sats` : b.kind} · click to sell`,
      matched: sellBars.some((sl) => sl.normTopic === b.normTopic),
      highlighted: activeTopic === b.normTopic,
      dimmed: activeTopic !== null && activeTopic !== b.normTopic,
      onClick: () => onPickSell(b.normTopic, b.sats || 100, b.note)
    }))
  )

  const rightStageBars = $derived<StageBar[]>(
    sellBars.map((b) => ({
      id: b.id,
      label: b.isSelf ? `${b.topic.slice(0, 14)} ★` : b.topic,
      badge: b.sats > 0 ? `${fmtSats(b.sats)}s` : undefined,
      width: b.sats > 0 ? 116 + Math.round((Math.max(b.sats, maxSellSats * 0.25) / maxSellSats) * 138) : 138,
      hoverText: `${b.topic} · ${b.sats ? `${fmtSats(b.sats)} sats` : b.kind} · click to buy`,
      matched: buyBars.some((bl) => bl.normTopic === b.normTopic),
      highlighted: activeTopic === b.normTopic,
      dimmed: activeTopic !== null && activeTopic !== b.normTopic,
      onClick: () => onPickBuy(b.normTopic, b.sats || 100, b.note)
    }))
  )

  function pushLiveBeat(side: 'buy' | 'sell' | 'clear', sats: number) {
    const mag = Math.min(16, Math.max(5, Math.round(Math.log10(Math.max(10, sats || 50)) * 5.2)))
    liveBeats = [...liveBeats.slice(-24), { buy: side === 'buy' || side === 'clear' ? mag : 0, sell: side === 'sell' || side === 'clear' ? mag : 0, settled: side === 'clear' }]
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
          stageRef?.triggerShockwave(`${amt}${topic ? ` · ${topic}` : ''}`)
          pushLiveBeat('clear', tape?.sats || 100)
        }
      }
      stageRef?.launchComet('left', undefined, onHit)
      stageRef?.launchComet('right', undefined, onHit)
    } else if (ev === 'market_want' || ev === 'tool:want' || tape?.side === 'buy') {
      stageRef?.launchComet('left', 0)
      pushLiveBeat('buy', tape?.sats || 100)
    } else if (ev === 'market_offer' || ev === 'tool:sell' || ev === 'listing_updated' || ev === 'remember' || ev === 'anchor' || tape?.side === 'sell') {
      stageRef?.launchComet('right', 0)
      pushLiveBeat('sell', tape?.sats || 100)
    }
  })

  function handleHover(side: 'left' | 'right' | null, id: string | null) {
    if (!side || !id) {
      activeTopic = null
      return
    }
    const found = (side === 'left' ? buyBars : sellBars).find((b) => b.id === id)
    activeTopic = found ? found.normTopic : null
  }
</script>

<HoneycombStage
  bind:this={stageRef}
  ariaLabel="Live Market Pulse"
  leftTitle="BUYS"
  rightTitle="SELLS"
  leftBars={leftStageBars}
  rightBars={rightStageBars}
  flowMode="inward"
  coreValue={fmtSats(clearedSats)}
  coreLabel="SATS CLEARED"
  ringLeftShare={buyShare}
  {live}
  heartbeats={heartbeatSlots}
  repVector={coreRep.vector}
  repTrusted={coreRep.trusted}
  repUntrusted={coreRep.untrusted}
  onHoverChange={handleHover}
/>
