<script lang="ts">
  import type { DealFeedEntry, DockClient, MarketTapeEvent, NavTab, PeerInfo, PulseSignal, Receipt, State } from '../lib/types'
  import MarketPulse from './MarketPulse.svelte'
  import MarketHomeTopBar from './MarketHomeTopBar.svelte'
  import QuickstartStrip from './QuickstartStrip.svelte'
  import MarketHomeBuyCol from './MarketHomeBuyCol.svelte'
  import MarketHomeSellCol from './MarketHomeSellCol.svelte'
  import MarketHomeTape from './MarketHomeTape.svelte'
  import { buildHomeAgentPills } from './MarketHomeHelpers'

  interface Props {
    s: State
    live: boolean
    pulse: PulseSignal | null
    dockedApps: DockClient[]
    searchQuery: string
    faucetBusy: boolean
    sellInputEl?: HTMLTextAreaElement | null
    buyInputEl?: HTMLTextAreaElement | null
    prefillBuy?: { domain: string; sats: number; seq: number } | null
    onSearch: (q: string) => void
    onOpenPalette: () => void
    onOpenDock: () => void
    onApproveStarter: () => void
    onRefresh: () => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
    onLocalPulse: (event: string, tape?: MarketTapeEvent) => void
    onNavigate: (tab: NavTab) => void
    onRequestFaucet: () => void
    onDecideApproval: (id: string, approve: boolean) => void
    onInspectDeal: (d: DealFeedEntry) => void
    onInspectPeer: (p: PeerInfo) => void
    onInspectReceipt: (r: Receipt) => void
  }

  let {
    s,
    live,
    pulse,
    dockedApps,
    searchQuery,
    faucetBusy,
    sellInputEl = $bindable(null),
    buyInputEl = $bindable(null),
    prefillBuy = null,
    onSearch,
    onOpenPalette,
    onOpenDock,
    onApproveStarter,
    onRefresh,
    onToast,
    onLocalPulse,
    onNavigate,
    onRequestFaucet,
    onDecideApproval,
    onInspectDeal,
    onInspectPeer,
    onInspectReceipt
  }: Props = $props()

  let buyText = $state('')
  let buyDomain = $state('')
  let buyMaxSats = $state('100')

  let sellText = $state('')
  let sellDomain = $state('')
  let sellPrice = $state('50')

  let lastPrefillSeq = 0
  $effect(() => {
    if (prefillBuy && prefillBuy.seq !== lastPrefillSeq) {
      lastPrefillSeq = prefillBuy.seq
      pickBuy(prefillBuy.domain, prefillBuy.sats)
    }
  })

  const homeAgentPills = $derived(buildHomeAgentPills(s, dockedApps))

  function pickBuy(domain: string, priceSats: number, hint?: string) {
    buyDomain = domain.replace(/^#/, '')
    if (priceSats > 0) buyMaxSats = String(priceSats)
    if (!buyText.trim() && hint) buyText = `Latest verified facts on #${buyDomain}`
    setTimeout(() => buyInputEl?.focus(), 20)
  }

  function pickSell(domain: string, rewardSats: number) {
    sellDomain = domain.replace(/^#/, '')
    if (rewardSats > 0) sellPrice = String(rewardSats)
    setTimeout(() => sellInputEl?.focus(), 20)
  }

  const q = $derived(searchQuery.trim().toLowerCase())
  const pendingApprovals = $derived(s.automation?.pending ?? [])
  const sellerPolicyReady = $derived(
    (s.settings?.policies ?? []).some((p) => p.active && !p.revokedAt && (p.policy?.roles ?? []).includes('seller'))
  )

  const bounties = $derived(
    (s.net.bounties ?? []).filter((b) => !q || `${b.domain} ${b.question}`.toLowerCase().includes(q))
  )
  const demand = $derived(
    (s.net.unmatchedWants ?? []).filter((u) => !q || `${u.domain} ${u.query}`.toLowerCase().includes(q))
  )
  const liveWants = $derived(
    (s.marketTape ?? [])
      .filter((e) => e.kind === 'want' && !demand.some((u) => u.wantId === e.id))
      .filter((e) => !q || `${(e.domains ?? []).join(' ')} ${e.summary ?? ''}`.toLowerCase().includes(q))
      .slice(0, 8)
  )
  const listings = $derived(
    (s.market ?? []).filter((m) => !q || `${m.shard} ${m.domains.join(' ')}`.toLowerCase().includes(q))
  )
  const peers = $derived(
    (s.net.peers ?? []).filter((p) => !q || `${p.handle ?? ''} ${p.pub} ${(p.sellDomains ?? []).join(' ')}`.toLowerCase().includes(q))
  )
  const deals = $derived(
    (s.automation?.deals ?? []).filter((d) => !q || `${d.shard ?? ''} ${d.cause} ${d.deliveredText ?? ''} ${d.to}`.toLowerCase().includes(q))
  )
  const paidReceipts = $derived(
    (s.receipts.list ?? [])
      .filter((r) => (r.sats ?? 0) > 0)
      .filter((r) => !q || `${r.shards.join(' ')} ${r.answer ?? ''}`.toLowerCase().includes(q))
      .slice(0, 6)
  )
</script>

<div class="mhome">
  <MarketHomeTopBar
    {searchQuery}
    sats={s.treasury.sats}
    network={s.network}
    {faucetBusy}
    {pendingApprovals}
    {onSearch}
    {onOpenPalette}
    {onRequestFaucet}
    {onNavigate}
    {onDecideApproval}
  />

  <QuickstartStrip
    {s}
    {dockedApps}
    {onOpenDock}
    {onNavigate}
    {onApproveStarter}
    {onRequestFaucet}
  />

  <!-- 1. VIRAL HERO: CLEAN NEKTAR CLEARING PULSE -->
  <MarketPulse {s} {live} {pulse} onPickBuy={pickBuy} onPickSell={pickSell} />

  <!-- 2. TABLESS SIDE-BY-SIDE BUY & SELL -->
  <section class="dual-deck">
    <MarketHomeBuyCol
      bind:buyText
      bind:buyDomain
      bind:buyMaxSats
      bind:buyInputEl
      {bounties}
      {demand}
      {liveWants}
      {onRefresh}
      {onToast}
      {onLocalPulse}
      onPickSell={pickSell}
    />

    <MarketHomeSellCol
      funded={s.treasury.funded}
      {sellerPolicyReady}
      bind:sellText
      bind:sellDomain
      bind:sellPrice
      bind:sellInputEl
      {listings}
      {peers}
      {onRefresh}
      {onToast}
      {onLocalPulse}
      onPickBuy={pickBuy}
      {onInspectPeer}
    />
  </section>

  <MarketHomeTape
    {homeAgentPills}
    {deals}
    {paidReceipts}
    {onNavigate}
    {onInspectDeal}
    {onInspectReceipt}
  />
</div>

<style>
  .mhome {
    display: flex;
    flex-direction: column;
    min-height: 100vh;
  }

  /* Tabless Side-by-Side Dual Deck */
  .dual-deck {
    display: grid;
    grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
    flex: 1;
  }

  @media (max-width: 820px) {
    .dual-deck { grid-template-columns: 1fr; }
  }
</style>
