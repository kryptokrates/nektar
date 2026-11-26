<script lang="ts">
  import { onMount } from 'svelte'
  import type { DealFeedEntry, DockClient, DrawerTarget, FeedFragment, MarketTapeEvent, NavTab, PeerInfo, PulseSignal, Receipt, State } from './lib/types'
  import { fmtSats } from './lib/format'
  import { operatorFetch, postJSON, registerOperatorPrompter } from './lib/api'
  import AuthGate from './components/AuthGate.svelte'
  import Sidebar from './components/Sidebar.svelte'
  import MarketHome from './components/MarketHome.svelte'
  import AgentsView from './components/AgentsView.svelte'
  import WalletView from './components/WalletView.svelte'
  import NodeView from './components/NodeView.svelte'
  import DetailDrawer from './components/DetailDrawer.svelte'
  import CommandPalette from './components/CommandPalette.svelte'
  import OperatorModal from './components/OperatorModal.svelte'
  import DockModal from './components/DockModal.svelte'

  let s = $state<State | null>(null)
  let dockedApps = $state<DockClient[]>([])
  let needsAuth = $state(false)
  let live = $state(false)
  let tab = $state<NavTab>('home')
  let searchQuery = $state('')
  let sellInputEl = $state<HTMLTextAreaElement | null>(null)
  let buyInputEl = $state<HTMLTextAreaElement | null>(null)
  let prefillBuy = $state<{ domain: string; sats: number; seq: number } | null>(null)
  let prefillSeq = 0
  let drawer = $state<DrawerTarget>(null)
  let paletteOpen = $state(false)
  let dockOpen = $state(false)
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
      document.title = s && s.receipts.earned > 0 ? `${fmtSats(s.receipts.earned)} sats · nektar` : 'nektar'
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

  async function runQuickRecall() {
    const rec = await postJSON<{ items?: Array<{ shard: string; seq: number; text?: string }> }>('/v1/tools/recall', { limit: 5 })
    if (!rec.ok) {
      showToast(rec.data.error ?? 'Recall failed', true)
      return
    }
    const items = rec.data.items ?? []
    if (items.length > 0 && items[0].text) {
      const g = await postJSON('/v1/tools/groundedAnswer', {
        claims: [{ text: items[0].text.slice(0, 140), cites: [{ shard: items[0].shard, seq: items[0].seq }] }]
      })
      if (g.ok) showToast('Issued first verifiable receipt ✓')
      else showToast(g.data.error ?? 'Grounding failed', true)
    } else {
      showToast('No memories yet — sell or save a fact first.', true)
    }
    await load()
  }

  async function requestFaucet() {
    faucetBusy = true
    try {
      const r = await fetch('/v1/onboard/faucet', { method: 'POST' })
      const j = (await r.json().catch(() => ({}))) as { sats?: number; error?: string }
      if (!r.ok) {
        showToast(j.error ?? `Faucet failed (${r.status})`, true)
        return
      }
      showToast(`Received +${fmtSats(j.sats ?? 2000)} test sats ✓`)
      await load()
    } finally {
      faucetBusy = false
    }
  }

  async function approveStarterPolicy() {
    const body: Record<string, unknown> = { preset: 'both' }
    if (s?.network === 'main') {
      const ok = await askConfirm('Mainnet Trading Policy', 'Approve starter Buyer + Seller policy using real BSV?')
      if (!ok) return
      body.confirm = 'I_UNDERSTAND'
    }
    const r = await operatorFetch('POST', '/v1/onboard/policy-preset', body, 'Approve starter Buyer + Seller policy')
    if (!r) return
    const j = (await r.json().catch(() => ({}))) as { policyId?: string; error?: string }
    if (!r.ok) showToast(j.error ?? `Approval failed (${r.status})`, true)
    else {
      showToast(`Enabled ${j.policyId ?? 'starter'} policy ✓`)
      await load()
    }
  }

  async function forgetMemory(shard: string, seq: number) {
    const ok = await askConfirm('Forget & Shred Memory', `Shred local plaintext of #${shard}:${seq}? On-chain hash stays intact.`)
    if (!ok) return
    const r = await postJSON('/v1/tools/forget', { shard, seq })
    if (!r.ok) showToast(r.data.error ?? `Forget failed (${r.status})`, true)
    else {
      showToast(`Shredded #${shard}:${seq}`)
      await load()
    }
  }

  async function decideApproval(id: string, approve: boolean) {
    const r = await operatorFetch(
      'POST',
      `/v1/automation/approvals/${id}`,
      { approve },
      approve ? 'Approve pending market deal' : 'Deny pending market deal'
    )
    if (!r) return
    if (!r.ok) showToast(`Decision rejected (${r.status})`, true)
    else {
      showToast(approve ? 'Deal approved ✓' : 'Deal denied')
      await load()
    }
  }

  async function shareUrl(url: string) {
    await navigator.clipboard.writeText(url).catch(() => {})
    showToast('Verification link copied ✓')
  }

  const commands = $derived([
    { id: 'buy', label: 'Buy verified data', hint: 'Market · Left rail', run: focusBuy },
    { id: 'sell', label: 'Anchor & sell knowledge', hint: 'Market · Right rail', run: focusSell },
    { id: 'home', label: 'Go to Market', hint: 'Home · Live Pulse', run: () => (tab = 'home') },
    { id: 'agents', label: 'Go to Agents', hint: 'Fleet & What They Know', run: () => (tab = 'agents') },
    { id: 'wallet', label: 'Go to Wallet', hint: 'Balance & Ledger', run: () => (tab = 'wallet') },
    { id: 'settings', label: 'Go to Settings', hint: 'Policies · Hubs · System', run: () => (tab = 'settings') },
    { id: 'dock', label: 'Dock AI agent (Claude / Cursor / Proxy / MCP)', hint: 'Agents', run: () => (dockOpen = true) },
    { id: 'recall', label: 'Run verified recall + receipt', hint: 'Grounding check', run: () => void runQuickRecall() },
    ...(s?.network === 'testnet'
      ? [{ id: 'faucet', label: 'Request Testnet Faucet sats', hint: 'Free test BSV', run: () => void requestFaucet() }]
      : []),
    { id: 'policy', label: 'Approve Starter Trading Policy', hint: 'Buyer + Seller', run: () => void approveStarterPolicy() },
    ...(s?.share.enabled
      ? [{ id: 'share', label: "Copy agent's public record link", hint: s.share.url, run: () => void shareUrl(s!.share.url) }]
      : [])
  ])

  let es: EventSource | null = null
  function connectEvents() {
    es?.close()
    es = new EventSource('/events')
    es.onopen = () => (live = true)
    es.onmessage = (ev) => {
      try {
        const msg = JSON.parse(ev.data) as { event?: string; tape?: MarketTapeEvent }
        if (msg.event && msg.event !== 'connected') {
          emitPulse(msg.event, msg.tape)
          scheduleLoad()
        }
      } catch {
        scheduleLoad()
      }
    }
    es.onerror = () => (live = false)
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
      const hash = window.location.hash
      if (hash.startsWith('#token=')) {
        const tok = decodeURIComponent(hash.slice('#token='.length))
        history.replaceState(null, '', window.location.pathname + window.location.search)
        await fetch('/v1/auth/session', {
          method: 'POST',
          headers: { 'content-type': 'application/json' },
          body: JSON.stringify({ token: tok })
        }).catch(() => {})
      }
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

<svelte:window onkeydown={onGlobalKey} />

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
      pendingApprovals={s.automation?.pending.length ?? 0}
      onSelect={(t) => (tab = t)}
      onOpenPalette={() => (paletteOpen = true)}
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
          onRefresh={load}
          onToast={showToast}
          onLocalPulse={emitPulse}
          onNavigate={(t) => (tab = t)}
          onRequestFaucet={() => void requestFaucet()}
          onDecideApproval={(id, ok) => void decideApproval(id, ok)}
          onInspectDeal={(d: DealFeedEntry) => (drawer = { kind: 'deal', item: d })}
          onInspectPeer={(p: PeerInfo) => (drawer = { kind: 'peer', item: p })}
          onInspectReceipt={(r: Receipt) => (drawer = { kind: 'receipt', item: r })}
        />
      {:else if tab === 'agents'}
        <AgentsView
          {s}
          {live}
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
          onForgetMemory={(sh, seq) => void forgetMemory(sh, seq)}
          onDecideApproval={(id, ok) => void decideApproval(id, ok)}
          onShareUrl={(u) => void shareUrl(u)}
        />
      {:else if tab === 'wallet'}
        <WalletView {s} onRefresh={load} onToast={showToast} onConfirm={askConfirm} />
      {:else}
        <NodeView {s} {live} onRefresh={load} onToast={showToast} onConfirm={askConfirm} onOpenDock={() => (dockOpen = true)} />
      {/if}
    </main>
  </div>

  <DetailDrawer target={drawer} onClose={() => (drawer = null)} />
  <CommandPalette open={paletteOpen} {commands} onClose={() => (paletteOpen = false)} />
  <DockModal
    open={dockOpen}
    onClose={() => {
      dockOpen = false
      void loadDock()
    }}
  />
  <OperatorModal
    mode={modalMode}
    title={modalTitle}
    detail={modalDetail}
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
  />

  {#if toast}
    <div class="toast" class:err={toast.isErr} role="status">{toast.msg}</div>
  {/if}
{/if}

<style>
  :global(html) {
    scrollbar-gutter: stable;
  }
  :global(*) {
    box-sizing: border-box;
  }
  :global(:root) {
    color-scheme: dark;
    --bg: #090807;
    --surf: #11100e;
    --hover: #171513;
    --line: #23201d;
    --line2: #342f2a;
    --ink: #f1ede9;
    --mut: #9a9087;
    --mut2: #6d655e;
    --amber: #f7991b;
    --ok: #4cc26b;
    --bad: #e5534b;
  }
  :global(body) {
    margin: 0;
    background: var(--bg);
    color: var(--ink);
    font-family: -apple-system, BlinkMacSystemFont, 'Inter', 'Segoe UI', system-ui, sans-serif;
    font-size: 14px;
    -webkit-font-smoothing: antialiased;
  }
  :global(.mono) {
    font-family: ui-monospace, 'Cascadia Mono', 'SFMono-Regular', Consolas, monospace;
  }
  :global(input[type='number']) {
    -moz-appearance: textfield;
    appearance: textfield;
  }
  :global(input[type='number']::-webkit-outer-spin-button),
  :global(input[type='number']::-webkit-inner-spin-button) {
    -webkit-appearance: none;
    margin: 0;
  }
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
  .toast {
    position: fixed;
    left: 50%;
    bottom: 1.5rem;
    transform: translateX(-50%);
    z-index: 100;
    background: #1b1815;
    border: 1px solid var(--line2);
    color: var(--ink);
    padding: 0.55rem 1.05rem;
    border-radius: 999px;
    font-size: 0.82rem;
    font-weight: 600;
    box-shadow: 0 12px 34px rgb(0 0 0 / 65%);
  }
  .toast.err {
    border-color: var(--bad);
    color: #ffd5d2;
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
    .toast {
      bottom: 4.5rem;
    }
  }
</style>
