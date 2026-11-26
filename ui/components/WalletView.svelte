<script lang="ts">
  import { onMount } from 'svelte'
  import type { State } from '../lib/types'
  import { operatorFetch } from '../lib/api'
  import { fmtSats, shortHash, sparklinePath } from '../lib/format'

  interface Props {
    s: State
    onRefresh: () => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
    onConfirm: (title: string, detail: string) => Promise<boolean>
  }

  let { s, onRefresh, onToast, onConfirm }: Props = $props()

  type Sheet = 'none' | 'receive' | 'send' | 'desktop'
  let sheet = $state<Sheet>('none')
  let busy = $state(false)
  let sendTo = $state('')
  let sendSats = $state('')
  let desktopSats = $state('10000')
  let copiedAddr = $state(false)
  let qrMode = $state<'uri' | 'raw'>('uri')
  let qrSvg = $state('')
  let qrPayload = $state('')

  async function loadQR(mode: 'uri' | 'raw') {
    try {
      const r = await fetch(`/v1/wallet/qr?mode=${mode}&format=json`)
      if (r.ok) {
        const j = (await r.json()) as { svg?: string; payload?: string }
        qrSvg = j.svg ?? ''
        qrPayload = j.payload ?? ''
      }
    } catch {}
  }

  $effect(() => {
    if (sheet === 'receive') {
      void loadQR(qrMode)
    }
  })

  type WalletAction = {
    txid?: string
    tx_id?: string
    satoshis?: number
    status?: string
    description?: string
    isOutgoing?: boolean
    labels?: string[]
  }
  let actions = $state<WalletAction[]>([])
  let agentFilter = $state<string>('all')
  let expandedBasket = $state<string>('')
  const spark = $derived(sparklinePath(s.receipts.amounts, 320, 44))
  const isDesktopCustody = $derived((s.treasury.walletMode ?? 'embedded') === 'client')
  const baskets = $derived(s.treasury.baskets ?? [])
  const totalBasketItems = $derived(s.treasury.basketItems ?? baskets.reduce((acc, b) => acc + b.count, 0))
  const totalBasketSats = $derived(s.treasury.basketLockedSats ?? baskets.reduce((acc, b) => acc + b.totalSats, 0))

  const availableAgents = $derived.by(() => {
    const set = new Set<string>()
    for (const a of actions) {
      for (const lbl of a.labels ?? []) {
        if (lbl.startsWith('agent:') && lbl.length > 6) {
          set.add(lbl.slice(6))
        }
      }
    }
    return Array.from(set).sort()
  })

  const filteredActions = $derived.by(() => {
    if (agentFilter === 'all') return actions
    const want = `agent:${agentFilter}`
    return actions.filter((a) => (a.labels ?? []).includes(want))
  })

  function explorerTxUrl(txid: string): string {
    if (!txid) return ''
    if (s.network === 'main') return `https://whatsonchain.com/tx/${txid}`
    if (s.network === 'testnet') return `https://test.whatsonchain.com/tx/${txid}`
    return ''
  }

  async function loadActions() {
    try {
      const r = await fetch('/v1/wallet/actions?limit=40')
      if (r.ok) {
        const j = (await r.json()) as { actions?: WalletAction[] }
        actions = j.actions ?? []
      }
    } catch {}
  }

  onMount(() => {
    void loadActions()
  })

  async function copyAddr() {
    if (!s.treasury.address) return
    await navigator.clipboard.writeText(s.treasury.address).catch(() => {})
    copiedAddr = true
    setTimeout(() => (copiedAddr = false), 1500)
  }

  async function syncWallet() {
    busy = true
    try {
      const r = await fetch('/v1/wallet/sync', { method: 'POST' })
      const j = (await r.json().catch(() => ({}))) as { added?: number; error?: string }
      if (!r.ok) onToast(j.error ?? `Sync failed (${r.status})`, true)
      else {
        onToast(`Wallet synced ✓`)
        await onRefresh()
        await loadActions()
      }
    } finally {
      busy = false
    }
  }

  async function requestFaucet() {
    busy = true
    try {
      const r = await fetch('/v1/onboard/faucet', { method: 'POST' })
      const j = (await r.json().catch(() => ({}))) as { sats?: number; error?: string }
      if (!r.ok) onToast(j.error ?? `Faucet failed (${r.status})`, true)
      else {
        onToast(`+${fmtSats(j.sats ?? 2000)} test sats received ✓`)
        await onRefresh()
        await loadActions()
      }
    } finally {
      busy = false
    }
  }

  async function fundDesktop() {
    const amt = Math.floor(Number(desktopSats) || 0)
    if (amt < 100) return onToast('Enter at least 100 sats', true)
    busy = true
    try {
      const r = await operatorFetch('POST', '/v1/wallet/fund-desktop', { sats: amt }, 'Fund from BSV Desktop')
      if (!r) return
      const j = (await r.json().catch(() => ({}))) as { satoshis?: number; error?: string }
      if (!r.ok) onToast(j.error ?? `Desktop fund failed (${r.status})`, true)
      else {
        sheet = 'none'
        onToast(`+${fmtSats(j.satoshis ?? amt)} sats funded ✓`)
        await onRefresh()
        await loadActions()
      }
    } finally {
      busy = false
    }
  }

  async function sendFunds() {
    const dest = sendTo.trim()
    const amt = Math.floor(Number(sendSats) || 0)
    if (!dest || amt <= 0) return onToast('Enter destination and amount', true)
    const payload: Record<string, unknown> = { to: dest, sats: amt }
    if (s.network === 'main') {
      const ok = await onConfirm('Confirm Mainnet Send', `Send ${fmtSats(amt)} real sats to ${shortHash(dest, 10, 6)}?`)
      if (!ok) return
      payload.confirm = 'I_UNDERSTAND'
    }
    busy = true
    try {
      const r = await operatorFetch('POST', '/v1/wallet/send', payload, 'Authorize outgoing payment')
      if (!r) return
      const j = (await r.json().catch(() => ({}))) as { error?: string }
      if (!r.ok) onToast(j.error ?? `Send failed (${r.status})`, true)
      else {
        sendSats = ''
        sheet = 'none'
        onToast(`Sent ${fmtSats(amt)} sats ✓`)
        await onRefresh()
        await loadActions()
      }
    } finally {
      busy = false
    }
  }

  async function sweepAll(toDesktop: boolean) {
    const dest = sendTo.trim()
    if (!toDesktop && !dest) return onToast('Enter destination address', true)
    const ok = await onConfirm('Sweep Wallet', `Move all ${fmtSats(s.treasury.sats)} sats to ${toDesktop ? 'BSV Desktop' : shortHash(dest, 10, 6)}?`)
    if (!ok) return
    const payload: Record<string, unknown> = toDesktop ? { toDesktop: true } : { to: dest }
    if (s.network === 'main') payload.confirm = 'I_UNDERSTAND'
    busy = true
    try {
      const r = await operatorFetch('POST', '/v1/wallet/sweep', payload, 'Authorize full wallet sweep')
      if (!r) return
      const j = (await r.json().catch(() => ({}))) as { sweptSats?: number; error?: string }
      if (!r.ok) onToast(j.error ?? `Sweep failed (${r.status})`, true)
      else {
        sheet = 'none'
        onToast(`Swept ${fmtSats(j.sweptSats ?? 0)} sats ✓`)
        await onRefresh()
        await loadActions()
      }
    } finally {
      busy = false
    }
  }
</script>

<div class="view">
  <header class="sticky-head">
    <h1>Wallet</h1>
  </header>

  <div class="wallet-grid">
    <div class="wallet-left">
      <section class="hero">
        {#if isDesktopCustody}
          <div class="custody-pill">
            <span class="dot"></span>
            <span>BSV Desktop Custody · Zero Local Keys</span>
          </div>
          <div class="bal">{totalBasketItems}</div>
          <div class="unit">
            on-chain basket {totalBasketItems === 1 ? 'asset' : 'assets'} · {fmtSats(totalBasketSats)} locked sats
          </div>
          <div class="custody-note">
            Spendable BSV balance &amp; Receive live privately inside <b>BSV Desktop</b> (:3321).
            Nektar only reads its own labeled actions &amp; covenant baskets over BRC-100.
          </div>
        {:else}
          <div class="bal">{fmtSats(s.treasury.sats)}</div>
          <div class="unit">satoshis {#if s.receipts.earned > 0}· <span class="ok">+{fmtSats(s.receipts.earned)} earned</span>{/if}</div>
        {/if}

        {#if spark.line}
          <svg viewBox="0 0 320 44" class="spark" preserveAspectRatio="none">
            <path d={spark.fill} fill="rgb(76 194 107 / 14%)" />
            <path d={spark.line} fill="none" stroke="#4cc26b" stroke-width="2" stroke-linejoin="round" />
          </svg>
        {/if}

        <div class="circle-bar">
          {#if !isDesktopCustody && s.network === 'testnet'}
            <button class="cact" onclick={requestFaucet} disabled={busy}>
              <span class="ring pri">⚡</span>
              <span>Faucet</span>
            </button>
          {/if}
          {#if !isDesktopCustody}
            <button class="cact" onclick={() => (sheet = sheet === 'receive' ? 'none' : 'receive')}>
              <span class="ring" class:on={sheet === 'receive'}>↓</span>
              <span>Receive</span>
            </button>
            <button class="cact" onclick={() => (sheet = sheet === 'send' ? 'none' : 'send')}>
              <span class="ring" class:on={sheet === 'send'}>↑</span>
              <span>Send</span>
            </button>
            <button class="cact" onclick={() => (sheet = sheet === 'desktop' ? 'none' : 'desktop')}>
              <span class="ring" class:on={sheet === 'desktop'}>◈</span>
              <span>Desktop</span>
            </button>
          {/if}
          <button class="cact" onclick={syncWallet} disabled={busy}>
            <span class="ring">↻</span>
            <span>{busy ? '…' : 'Sync'}</span>
          </button>
        </div>
      </section>

      {#if baskets.length > 0}
        <section class="baskets-sec">
          <div class="section-lbl">BRC-100 Asset Baskets</div>
          <div class="basket-list">
            {#each baskets as b}
              <button
                class="basket-card"
                class:open={expandedBasket === b.basket}
                onclick={() => (expandedBasket = expandedBasket === b.basket ? '' : b.basket)}
              >
                <div class="brow">
                  <div class="bmeta">
                    <b>{b.title}</b>
                    <code class="bcode">{b.basket}</code>
                  </div>
                  <div class="bstat">
                    <span class="bcnt">{b.count}</span>
                    <span class="bsats">{fmtSats(b.totalSats)} sats</span>
                  </div>
                </div>
                <div class="bdesc">{b.description}</div>
              </button>
              {#if expandedBasket === b.basket && b.items.length > 0}
                <div class="bitems">
                  {#each b.items as item}
                    <div class="bitem">
                      <div class="bitem-l">
                        {#if item.meta?.shard}
                          <b>#{String(item.meta.shard)}</b>
                          <span class="mut">batch #{String(item.meta.n ?? 0)} · {String(item.meta.size ?? 0)} frags</span>
                        {:else if item.meta?.kind}
                          <b>{String(item.meta.kind)}</b>
                          <span class="mut">{shortHash(item.outpoint, 10, 6)}</span>
                        {:else}
                          <code class="bcode">{shortHash(item.outpoint, 10, 6)}</code>
                        {/if}
                      </div>
                      <div class="bitem-r">{fmtSats(item.satoshis)} sats</div>
                    </div>
                  {/each}
                </div>
              {/if}
            {/each}
          </div>
        </section>
      {/if}

      {#if sheet === 'receive'}
        <section class="sheet receive-sheet">
          <div class="qr-head">
            <div>
              <div class="slbl">Deposit Address ({s.network === 'main' ? 'BSV Mainnet · real sats' : s.network})</div>
              {#if s.network === 'main'}
                <div class="qr-tip">Recommended starter float: <b>5,000–10,000 sats</b> (~$0.003)</div>
              {/if}
            </div>
            <div class="qr-tabs">
              <button class="mini" class:on={qrMode === 'uri'} onclick={() => (qrMode = 'uri')}>bitcoin: URI</button>
              <button class="mini" class:on={qrMode === 'raw'} onclick={() => (qrMode = 'raw')}>Raw Address</button>
            </div>
          </div>

          {#if qrSvg}
            <div class="qr-wrap">
              <div class="qr-box">{@html qrSvg}</div>
            </div>
          {/if}

          <div class="addr-row">
            <code class="mono">{qrPayload || s.treasury.address || 'Initializing…'}</code>
            <button class="mini" onclick={copyAddr}>{copiedAddr ? 'Copied ✓' : 'Copy'}</button>
            <button class="mini" onclick={syncWallet} disabled={busy}>{busy ? 'Checking…' : 'Check Deposit'}</button>
          </div>
        </section>
      {:else if sheet === 'desktop'}
        <section class="sheet">
          <div class="slbl">Top Up from BSV Desktop</div>
          <div class="srow">
            <input class="in" type="number" min="100" bind:value={desktopSats} placeholder="Satoshis" />
            <button class="pri-btn" onclick={fundDesktop} disabled={busy}>Transfer</button>
          </div>
        </section>
      {:else if sheet === 'send'}
        <section class="sheet">
          <div class="slbl">Send Satoshis</div>
          <input class="in" bind:value={sendTo} placeholder="BSV address or identity key" />
          <div class="srow">
            <input class="in" type="number" min="1" bind:value={sendSats} placeholder="Amount (sats)" />
            <button class="pri-btn" onclick={sendFunds} disabled={busy}>Send</button>
          </div>
          <div class="sweep-links">
            <button class="txt-btn" onclick={() => sweepAll(false)} disabled={busy}>Sweep all to address</button>
            <span>·</span>
            <button class="txt-btn" onclick={() => sweepAll(true)} disabled={busy}>Sweep to BSV Desktop</button>
          </div>
        </section>
      {/if}
    </div>

    <div class="wallet-right">
      <div class="act-head">
        <div class="section-lbl">
          Activity {#if isDesktopCustody}<span class="sync-tag">· synced with BSV Desktop</span>{/if}
        </div>
        {#if availableAgents.length > 0}
          <div class="agent-pills">
            <button class="apill" class:on={agentFilter === 'all'} onclick={() => (agentFilter = 'all')}>All</button>
            {#each availableAgents as ag}
              <button class="apill" class:on={agentFilter === ag} onclick={() => (agentFilter = ag)}>{ag}</button>
            {/each}
          </div>
        {/if}
      </div>
      {#each filteredActions as a}
        {@const tx = a.txid || a.tx_id || ''}
        {@const url = explorerTxUrl(tx)}
        <div class="txrow">
          <div class="txico" class:out={a.isOutgoing}>{a.isOutgoing ? '↑' : '↓'}</div>
          <div class="txl">
            <b>{a.description || (a.isOutgoing ? 'Sent BSV' : 'Received BSV')}</b>
            <span class="mut">
              {a.status || 'settled'}
              {#if tx}
                · {#if url}<a class="txlink" href={url} target="_blank" rel="noreferrer">{shortHash(tx, 8, 6)}</a>{:else}{shortHash(tx, 8, 6)}{/if}
              {/if}
            </span>
          </div>
          <div class="amt" class:out={a.isOutgoing}>
            {a.isOutgoing ? '-' : '+'}{fmtSats(Math.abs(a.satoshis ?? 0))} sats
          </div>
        </div>
      {:else}
        <div class="empty">
          <p>No Nektar transactions yet</p>
          <small>
            {#if isDesktopCustody}
              Anchor a memory shard or trade knowledge — transactions signed by BSV Desktop appear here live.
            {:else}
              Tap <b>Faucet</b> or <b>Receive</b> to fund automatic anchoring &amp; trades.
            {/if}
          </small>
        </div>
      {/each}
    </div>
  </div>
</div>

<style>
  .view { display: flex; flex-direction: column; min-height: 100vh; }
  .wallet-grid {
    display: grid;
    grid-template-columns: minmax(320px, 420px) minmax(0, 1fr);
    flex: 1;
  }
  .wallet-left {
    border-right: 1px solid var(--line);
    display: flex;
    flex-direction: column;
  }
  .wallet-right {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }
  .spark { display: block; width: 100%; max-width: 300px; height: 40px; margin: 0.9rem auto 0; }
  @media (max-width: 900px) {
    .wallet-grid { grid-template-columns: 1fr; }
    .wallet-left { border-right: 0; }
  }
  .receive-sheet { display: grid; gap: 0.7rem; }
  .qr-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 0.6rem; flex-wrap: wrap; }
  .qr-tip { font-size: 0.76rem; color: var(--mut); margin-top: 0.15rem; }
  .qr-tip b { color: var(--ink); }
  .qr-tabs { display: flex; gap: 0.35rem; }
  .mini.on { border-color: var(--amber); color: var(--amber); background: rgb(247 153 27 / 12%); }
  .qr-wrap { display: flex; justify-content: center; padding: 0.25rem 0; }
  .qr-box {
    width: 196px; height: 196px; background: #ffffff; padding: 8px;
    border-radius: 12px; border: 1px solid var(--line2); box-shadow: 0 8px 24px rgb(0 0 0 / 45%);
  }
  .qr-box :global(svg) { width: 100%; height: 100%; display: block; }
  .sticky-head {
    position: sticky; top: 0; z-index: 15;
    padding: 0.9rem 1.25rem;
    background: rgb(9 8 7 / 92%); backdrop-filter: blur(12px);
    border-bottom: 1px solid var(--line);
  }
  h1 { margin: 0; font-size: 1.15rem; font-weight: 800; letter-spacing: -0.02em; }
  .hero {
    padding: 2.2rem 1.25rem 1.7rem; text-align: center;
    border-bottom: 1px solid var(--line);
    background: radial-gradient(ellipse 65% 75% at 50% 0%, rgb(247 153 27 / 9%), transparent 75%);
  }
  .bal {
    font-size: 3.4rem; font-weight: 800; letter-spacing: -0.04em;
    font-variant-numeric: tabular-nums; line-height: 1; color: var(--ink);
  }
  .unit { margin-top: 0.45rem; font-size: 0.86rem; color: var(--mut); font-weight: 600; }
  .ok { color: var(--ok); }
  .circle-bar {
    margin-top: 1.65rem; display: flex; justify-content: center; gap: 1.35rem; flex-wrap: wrap;
  }
  .cact {
    display: flex; flex-direction: column; align-items: center; gap: 0.4rem;
    background: none; border: 0; color: var(--mut); font: inherit; font-size: 0.76rem; font-weight: 600; cursor: pointer;
  }
  .cact:hover { color: var(--ink); }
  .ring {
    width: 46px; height: 46px; border-radius: 50%;
    display: grid; place-items: center; font-size: 1.05rem; font-weight: 700;
    background: var(--surf); border: 1px solid var(--line2); color: var(--ink);
    transition: border-color 0.14s, transform 0.1s;
  }
  .ring.pri { background: var(--amber); border-color: var(--amber); color: #120e0c; }
  .ring.on, .cact:hover .ring:not(.pri) { border-color: var(--amber); color: var(--amber); }
  .cact:active .ring { transform: scale(0.95); }
  .sheet {
    padding: 1.1rem 1.25rem; border-bottom: 1px solid var(--line); background: var(--surf);
    display: grid; gap: 0.65rem;
  }
  .slbl { font-size: 0.72rem; font-weight: 700; color: var(--mut2); text-transform: uppercase; letter-spacing: 0.09em; }
  .addr-row {
    display: flex; align-items: center; gap: 0.5rem; min-width: 0;
    background: var(--bg); border: 1px solid var(--line); border-radius: 11px; padding: 0.55rem 0.8rem;
  }
  .addr-row code { flex: 1; min-width: 0; font-size: 0.82rem; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .mini {
    flex-shrink: 0;
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.28rem 0.75rem; font: inherit; font-size: 0.76rem; font-weight: 800; cursor: pointer;
  }
  .srow { display: flex; gap: 0.5rem; }
  .in {
    flex: 1; min-width: 120px; background: var(--bg); border: 1px solid var(--line); border-radius: 10px;
    color: var(--ink); padding: 0.52rem 0.75rem; font: inherit; font-size: 0.84rem; outline: none;
  }
  .in:focus { border-color: var(--amber); }
  .pri-btn {
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.5rem 1.1rem; font: inherit; font-size: 0.82rem; font-weight: 800; cursor: pointer;
  }
  .sweep-links { display: flex; gap: 0.5rem; align-items: center; font-size: 0.76rem; color: var(--mut2); }
  .txt-btn { background: none; border: 0; color: var(--mut); font: inherit; font-size: 0.76rem; cursor: pointer; padding: 0; }
  .txt-btn:hover { color: var(--amber); }
  .section-lbl {
    padding: 0.95rem 1.25rem 0.45rem; font-size: 0.7rem; font-weight: 700;
    letter-spacing: 0.1em; text-transform: uppercase; color: var(--mut2);
  }
  .txrow {
    display: flex; align-items: center; gap: 0.85rem; padding: 0.85rem 1.25rem;
    border-bottom: 1px solid var(--line);
  }
  .txico {
    width: 34px; height: 34px; border-radius: 50%; flex-shrink: 0;
    display: grid; place-items: center; font-size: 0.9rem; font-weight: 800;
    background: rgb(76 194 107 / 12%); color: var(--ok);
  }
  .txico.out { background: var(--surf); color: var(--mut); border: 1px solid var(--line); }
  .txl { flex: 1; min-width: 0; display: grid; gap: 0.12rem; }
  .txl b { font-size: 0.88rem; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .amt { font-weight: 800; font-size: 0.88rem; color: var(--ok); font-variant-numeric: tabular-nums; white-space: nowrap; }
  .amt.out { color: var(--ink); }
  .mut { color: var(--mut); font-size: 0.76rem; }
  .empty { padding: 3.5rem 1.5rem; text-align: center; color: var(--mut); }
  .empty p { margin: 0 0 0.3rem; color: var(--ink); font-size: 0.96rem; font-weight: 600; }
  .custody-pill {
    display: inline-flex; align-items: center; gap: 0.45rem;
    padding: 0.28rem 0.75rem; margin-bottom: 0.85rem; border-radius: 999px;
    font-size: 0.72rem; font-weight: 700; color: var(--ok);
    background: rgb(76 194 107 / 12%); border: 1px solid rgb(76 194 107 / 32%);
  }
  .custody-pill .dot { width: 7px; height: 7px; border-radius: 50%; background: var(--ok); }
  .custody-note {
    margin: 0.75rem auto 0; max-width: 320px; font-size: 0.76rem; line-height: 1.45; color: var(--mut);
  }
  .custody-note b { color: var(--ink); }
  .baskets-sec { border-bottom: 1px solid var(--line); padding-bottom: 0.85rem; }
  .basket-list { display: grid; gap: 0.45rem; padding: 0 1.1rem; }
  .basket-card {
    text-align: left; background: var(--surf); border: 1px solid var(--line);
    border-radius: 12px; padding: 0.65rem 0.8rem; cursor: pointer; color: inherit; font: inherit;
    transition: border-color 0.14s;
  }
  .basket-card:hover, .basket-card.open { border-color: var(--amber); }
  .brow { display: flex; align-items: center; justify-content: space-between; gap: 0.5rem; }
  .bmeta { display: flex; flex-direction: column; gap: 0.12rem; min-width: 0; }
  .bmeta b { font-size: 0.84rem; color: var(--ink); }
  .bcode { font-size: 0.7rem; color: var(--amber); font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
  .bstat { text-align: right; display: flex; flex-direction: column; align-items: flex-end; }
  .bcnt { font-size: 0.92rem; font-weight: 800; color: var(--ink); font-variant-numeric: tabular-nums; }
  .bsats { font-size: 0.7rem; color: var(--mut); }
  .bdesc { margin-top: 0.3rem; font-size: 0.73rem; color: var(--mut); }
  .bitems {
    margin: -0.2rem 0 0.25rem; padding: 0.45rem 0.75rem; border-radius: 10px;
    background: var(--bg); border: 1px solid var(--line); display: grid; gap: 0.4rem;
  }
  .bitem { display: flex; align-items: center; justify-content: space-between; gap: 0.5rem; font-size: 0.76rem; }
  .bitem-l { display: flex; flex-direction: column; min-width: 0; }
  .bitem-r { font-weight: 700; color: var(--ink); white-space: nowrap; }
  .act-head { display: flex; align-items: center; justify-content: space-between; gap: 0.6rem; flex-wrap: wrap; padding-right: 1rem; }
  .sync-tag { color: var(--ok); text-transform: none; letter-spacing: 0; font-weight: 600; }
  .agent-pills { display: flex; gap: 0.3rem; padding-top: 0.45rem; }
  .apill {
    background: var(--surf); border: 1px solid var(--line); color: var(--mut);
    border-radius: 999px; padding: 0.2rem 0.6rem; font: inherit; font-size: 0.72rem; font-weight: 700; cursor: pointer;
  }
  .apill.on { border-color: var(--amber); color: var(--amber); background: rgb(247 153 27 / 12%); }
  .txlink { color: var(--amber); text-decoration: none; }
  .txlink:hover { text-decoration: underline; }
</style>
