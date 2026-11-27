<script lang="ts">
  import { onMount } from 'svelte'
  import type { DealFeedEntry, DockClient, DrawerTarget, FeedFragment, MarketTapeEvent, NavTab, PeerInfo, PulseSignal, Receipt, State } from './lib/types'
  import { fmtSats } from './lib/format'
  import { registerOperatorPrompter } from './lib/api'
  import './lib/appTheme.css'
  import {
    approveStarterPolicyAction,
    buildAppCommands,
    createAppEventSource,
    decideApprovalAction,
    forgetMemoryAction,
    requestFaucetAction,
    runQuickRecallAction,
    shareUrlAction
  } from './lib/appActions'
  import AuthGate from './components/AuthGate.svelte'
  import Sidebar from './components/Sidebar.svelte'
  import MarketHome from './components/MarketHome.svelte'
  import AgentsView from './components/AgentsView.svelte'
  import HubsView from './components/HubsView.svelte'
  import WalletView from './components/WalletView.svelte'
  import NodeView from './components/NodeView.svelte'
  import AppOverlays from './components/AppOverlays.svelte'

  let s = $state<State | null>(null)
  let dockedApps = $state<DockClient[]>([])
  let needsAuth = $state(false)
  let live = $state(false)
  let tab = $state<NavTab>('home')
  let settingsSub = $state<'policy' | 'network' | 'system'>('policy')
  let searchQuery = $state('')
  let sellInputEl = $state<HTMLTextAreaElement | null>(null)
  let buyInputEl = $state<HTMLTextAreaElement | null>(null)
  let prefillBuy = $state<{ domain: string; sats: number; seq: number } | null>(null)
  let prefillSeq = 0
  let drawer = $state<DrawerTarget>(null)
  let paletteOpen = $state(false)
  let dockOpen = $state(false)
  let capsuleFile = $state<File | null>(null)
  let overlaysRef = $state<{ openCapsulePicker: () => void } | null>(null)
  let dragDepth = $state(0)
  let openRepoRequest = $state<{ repo: string; seq: number } | null>(null)
  let openRepoSeq = 0
  let faucetBusy = $state(false)
  let toast = $state<{ msg: string; isErr: boolean } | null>(null)
  let toastTimer: ReturnType<typeof setTimeout> | null = null

  // Real-time SSE market pulse signal for kinetic animations
  let pulse = $state<PulseSignal | null>(null)
  let pulseSeq = 0
  let loadDebounce: ReturnType<typeof setTimeout> | null = null

  function emitPulse(event: string, tape?: MarketTapeEvent) {
    pulseSeq += 1
    pulse = { seq: pulseSeq, event, ts: Date.now(), tape }
  }

  // In-app operator token & confirm modal state (zero window.prompt / window.confirm)
  let modalMode = $state<'none' | 'token' | 'confirm'>('none')
  let modalTitle = $state('')
  let modalDetail = $state('')
  let tokenResolver: ((v: string | null) => void) | null = null
  let confirmResolver: ((ok: boolean) => void) | null = null

  function showToast(msg: string, isErr = false) {
    toast = { msg, isErr }
    if (toastTimer) clearTimeout(toastTimer)
    toastTimer = setTimeout(() => (toast = null), 3400)
  }

  function promptOperator(reason: string): Promise<string | null> {
    modalTitle = 'Operator Authorization'
    modalDetail = reason
    modalMode = 'token'
    return new Promise((resolve) => {
      tokenResolver = resolve
    })
  }

  function askConfirm(title: string, detail: string): Promise<boolean> {
    modalTitle = title
    modalDetail = detail
    modalMode = 'confirm'
    return new Promise((resolve) => {
      confirmResolver = resolve
    })
  }

  async function loadDock() {
    try {
      const r = await fetch('/v1/dock/detect')
      if (r.ok) {
        const j = (await r.json()) as { clients?: DockClient[] }
        dockedApps = j.clients ?? []
      }
    } catch {}
  }

  async function load() {
    try {
      const r = await fetch('/state')
      if (r.status === 401) {
        needsAuth = true
        return
      }
      if (!r.ok) throw new Error(`state ${r.status}`)
      needsAuth = false
      s = await r.json()
      document.title =
        s && s.receipts.earned > 0
          ? `${fmtSats(s.receipts.earned)} sats · Nektar`
          : 'Nektar — Verifiable AI Agent Memory & Knowledge Market'
    } catch (e) {
      showToast(String(e), true)
    }
  }

  function scheduleLoad() {
    if (loadDebounce) clearTimeout(loadDebounce)
    loadDebounce = setTimeout(() => {
      loadDebounce = null
      void load()
    }, 180)
  }

  const actionDeps = { showToast, askConfirm, load }

  function focusSell() {
    tab = 'home'
    setTimeout(() => sellInputEl?.focus(), 25)
  }

  function focusBuy() {
    tab = 'home'
    setTimeout(() => buyInputEl?.focus(), 25)
  }

  function quickBuyTopic(domain: string, sats: number) {
    tab = 'home'
    prefillSeq += 1
    prefillBuy = { domain, sats, seq: prefillSeq }
  }

  async function requestFaucet() {
    faucetBusy = true
    try {
      await requestFaucetAction(actionDeps)
    } finally {
      faucetBusy = false
    }
  }

  function openAbout() {
    settingsSub = 'system'
    tab = 'settings'
  }

  function openMemoryRepo(repoName: string) {
    tab = 'agents'
    openRepoSeq += 1
    openRepoRequest = { repo: repoName, seq: openRepoSeq }
  }

  function handleDragEnter(e: DragEvent) {
    if (e.dataTransfer?.types?.includes('Files')) {
      e.preventDefault()
      dragDepth += 1
    }
  }

  function handleDragOver(e: DragEvent) {
    if (e.dataTransfer?.types?.includes('Files')) {
      e.preventDefault()
    }
  }

  function handleDragLeave(e: DragEvent) {
    if (e.dataTransfer?.types?.includes('Files')) {
      e.preventDefault()
      dragDepth = Math.max(0, dragDepth - 1)
    }
  }

  function handleDrop(e: DragEvent) {
    dragDepth = 0
    const file = e.dataTransfer?.files?.[0]
    if (!file) return
    if (file.name.toLowerCase().endsWith('.nkt')) {
      e.preventDefault()
      capsuleFile = file
    }
  }

  const commands = $derived(
    buildAppCommands({
      s,
      focusBuy,
      focusSell,
      setTab: (t) => (tab = t),
      openAbout,
      openDock: () => (dockOpen = true),
      openCapsulePicker: () => overlaysRef?.openCapsulePicker(),
      openMemoryRepo,
      runQuickRecall: () => void runQuickRecallAction(actionDeps),
      requestFaucet: () => void requestFaucet(),
      approveStarterPolicy: () => void approveStarterPolicyAction(s?.network, actionDeps),
      shareUrl: (u) => void shareUrlAction(u, showToast)
    })
  )

  let es: EventSource | null = null
  function connectEvents() {
    es?.close()
    es = createAppEventSource({
      onLiveChange: (v) => (live = v),
      onPulse: emitPulse,
      onScheduleLoad: scheduleLoad
    })
  }

  function onGlobalKey(e: KeyboardEvent) {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
      e.preventDefault()
      paletteOpen = !paletteOpen
    }
  }

  onMount(() => {
    registerOperatorPrompter(promptOperator)
    void (async () => {
      await load()
      if (!needsAuth) {
        connectEvents()
        void loadDock()
      }
    })()
    return () => {
      es?.close()
      if (loadDebounce) clearTimeout(loadDebounce)
    }
  })
</script>

<svelte:window
  onkeydown={onGlobalKey}
  ondragenter={handleDragEnter}
  ondragover={handleDragOver}
  ondragleave={handleDragLeave}
  ondrop={handleDrop}
/>

{#if needsAuth}
  <AuthGate
    onUnlocked={() => {
      needsAuth = false
      void load()
      void loadDock()
      connectEvents()
    }}
  />
{:else if !s}
  <div class="boot">loading nektar…</div>
{:else}
  <div class="shell">
    <Sidebar
      {tab}
      {live}
      aboutActive={tab === 'settings' && settingsSub === 'system'}
      pendingApprovals={s.automation?.pending.length ?? 0}
      connectedHubs={s.net.hosts?.length ?? 0}
      onSelect={(t) => (tab = t)}
      onOpenPalette={() => (paletteOpen = true)}
      onOpenAbout={openAbout}
    />

    <main class="center">
      {#if tab === 'home'}
        <MarketHome
          {s}
          {live}
          {pulse}
          {dockedApps}
          {searchQuery}
          {faucetBusy}
          {prefillBuy}
          bind:sellInputEl
          bind:buyInputEl
          onSearch={(q) => (searchQuery = q)}
          onOpenPalette={() => (paletteOpen = true)}
          onOpenDock={() => (dockOpen = true)}
          onApproveStarter={() => void approveStarterPolicyAction(s?.network, actionDeps)}
          onRefresh={load}
          onToast={showToast}
          onLocalPulse={emitPulse}
          onNavigate={(t) => (tab = t)}
          onRequestFaucet={() => void requestFaucet()}
          onDecideApproval={(id, ok) => void decideApprovalAction(id, ok, actionDeps)}
          onInspectDeal={(d: DealFeedEntry) => (drawer = { kind: 'deal', item: d })}
          onInspectPeer={(p: PeerInfo) => (drawer = { kind: 'peer', item: p })}
          onInspectReceipt={(r: Receipt) => (drawer = { kind: 'receipt', item: r })}
        />
      {:else if tab === 'agents'}
        <AgentsView
          {s}
          {live}
          {pulse}
          {dockedApps}
          {searchQuery}
          onSearch={(q) => (searchQuery = q)}
          onOpenDock={() => (dockOpen = true)}
          onRefresh={load}
          onRefreshDock={loadDock}
          onToast={showToast}
          onQuickBuyTopic={quickBuyTopic}
          onInspectMemory={(m: FeedFragment) => (drawer = { kind: 'memory', item: m })}
          onInspectReceipt={(r: Receipt) => (drawer = { kind: 'receipt', item: r })}
          onInspectDeal={(d: DealFeedEntry) => (drawer = { kind: 'deal', item: d })}
          onInspectPeer={(p: PeerInfo) => (drawer = { kind: 'peer', item: p })}
          onForgetMemory={(sh, seq) => void forgetMemoryAction(sh, seq, actionDeps)}
          onDecideApproval={(id, ok) => void decideApprovalAction(id, ok, actionDeps)}
          onShareUrl={(u) => void shareUrlAction(u, showToast)}
          onInspectCapsuleFile={(f) => (capsuleFile = f)}
          {openRepoRequest}
        />
      {:else if tab === 'hubs'}
        <HubsView {s} onRefresh={load} onToast={showToast} />
      {:else if tab === 'wallet'}
        <WalletView {s} onRefresh={load} onToast={showToast} onConfirm={askConfirm} />
      {:else}
        <NodeView {s} {live} bind:sub={settingsSub} onRefresh={load} onToast={showToast} onConfirm={askConfirm} onOpenDock={() => (dockOpen = true)} />
      {/if}
    </main>
  </div>

  <AppOverlays
    bind:this={overlaysRef}
    {dragDepth}
    {drawer}
    {paletteOpen}
    {commands}
    {capsuleFile}
    {dockOpen}
    {modalMode}
    {modalTitle}
    {modalDetail}
    {toast}
    onPickCapsuleFile={(f) => (capsuleFile = f)}
    onCloseDrawer={() => (drawer = null)}
    onClosePalette={() => (paletteOpen = false)}
    onCloseCapsule={() => (capsuleFile = null)}
    onCloseDock={() => {
      dockOpen = false
      void loadDock()
    }}
    onResolveToken={(v) => {
      modalMode = 'none'
      tokenResolver?.(v)
      tokenResolver = null
    }}
    onResolveConfirm={(ok) => {
      modalMode = 'none'
      confirmResolver?.(ok)
      confirmResolver = null
    }}
    onToast={showToast}
    onRefresh={load}
    onImportedRepo={openMemoryRepo}
  />
{/if}

<style>
  .boot {
    display: grid;
    place-items: center;
    min-height: 100vh;
    color: var(--mut);
    font-size: 0.9rem;
  }
  /* Unified Full-Width Geometry Across Market, Agents, Wallet & Settings */
  .shell {
    display: grid;
    grid-template-columns: 204px minmax(0, 1fr);
    min-height: 100vh;
    max-width: 1420px;
    margin: 0 auto;
    padding: 0 0.75rem;
  }
  .center {
    min-width: 0;
    min-height: 100vh;
    border-left: 1px solid var(--line);
    border-right: 1px solid var(--line);
  }
  @media (max-width: 1100px) and (min-width: 721px) {
    .shell {
      grid-template-columns: 68px minmax(0, 1fr);
      padding: 0 0.5rem;
    }
  }
  @media (max-width: 720px) {
    .shell {
      grid-template-columns: 1fr;
      padding: 0 0 60px;
    }
    .center {
      border-left: 0;
      border-right: 0;
    }
  }
</style>
