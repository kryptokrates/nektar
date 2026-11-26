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
  }
  let actions = $state<WalletAction[]>([])
  const spark = $derived(sparklinePath(s.receipts.amounts, 320, 44))

  async function loadActions() {
    try {
      const r = await fetch('/v1/wallet/actions?limit=25')
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
        <div class="bal">{fmtSats(s.treasury.sats)}</div>
        <div class="unit">satoshis {#if s.receipts.earned > 0}· <span class="ok">+{fmtSats(s.receipts.earned)} earned</span>{/if}</div>

        {#if spark.line}
          <svg viewBox="0 0 320 44" class="spark" preserveAspectRatio="none">
            <path d={spark.fill} fill="rgb(76 194 107 / 14%)" />
            <path d={spark.line} fill="none" stroke="#4cc26b" stroke-width="2" stroke-linejoin="round" />
          </svg>
        {/if}

        <div class="circle-bar">
          {#if s.network === 'testnet'}
            <button class="cact" onclick={requestFaucet} disabled={busy}>
              <span class="ring pri">⚡</span>
              <span>Faucet</span>
            </button>
          {/if}
          <button class="cact" onclick={() => (sheet = sheet === 'receive' ? 'none' : 'receive')}>
            <span class="ring" class:on={sheet === 'receive'}>↓</span>
            <span>Receive</span>
          </button>
          {#if (s.treasury.walletMode ?? 'embedded') === 'embedded'}
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
      <div class="section-lbl">Activity</div>
      {#each actions as a}
        <div class="txrow">
          <div class="txico" class:out={a.isOutgoing}>{a.isOutgoing ? '↑' : '↓'}</div>
          <div class="txl">
            <b>{a.description || (a.isOutgoing ? 'Sent BSV' : 'Received BSV')}</b>
            <span class="mut">{a.status || 'settled'}</span>
          </div>
          <div class="amt" class:out={a.isOutgoing}>
            {a.isOutgoing ? '-' : '+'}{fmtSats(Math.abs(a.satoshis ?? 0))} sats
          </div>
        </div>
      {:else}
        <div class="empty">
          <p>No transactions yet</p>
          <small>Tap <b>Faucet</b> or <b>Receive</b> to fund automatic anchoring &amp; trades.</small>
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
</style>
