<script lang="ts">
  import type { State } from '../lib/types'
  import { operatorFetch } from '../lib/api'
  import { fmtSats, shortHash } from '../lib/format'

  interface Props {
    s: State
    sheet: 'none' | 'receive' | 'send' | 'desktop'
    busy: boolean
    onCloseSheet: () => void
    onSetBusy: (v: boolean) => void
    onSync: () => Promise<void>
    onRefreshAll: () => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
    onConfirm: (title: string, detail: string) => Promise<boolean>
  }

  let { s, sheet, busy, onCloseSheet, onSetBusy, onSync, onRefreshAll, onToast, onConfirm }: Props = $props()

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

  async function copyAddr() {
    if (!s.treasury.address) return
    await navigator.clipboard.writeText(s.treasury.address).catch(() => {})
    copiedAddr = true
    setTimeout(() => (copiedAddr = false), 1500)
  }

  async function fundDesktop() {
    const amt = Math.floor(Number(desktopSats) || 0)
    if (amt < 100) return onToast('Enter at least 100 sats', true)
    onSetBusy(true)
    try {
      const r = await operatorFetch('POST', '/v1/wallet/fund-desktop', { sats: amt }, 'Fund from BSV Desktop')
      if (!r) return
      const j = (await r.json().catch(() => ({}))) as { satoshis?: number; error?: string }
      if (!r.ok) onToast(j.error ?? `Desktop fund failed (${r.status})`, true)
      else {
        onCloseSheet()
        onToast(`+${fmtSats(j.satoshis ?? amt)} sats funded ✓`)
        await onRefreshAll()
      }
    } finally {
      onSetBusy(false)
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
    onSetBusy(true)
    try {
      const r = await operatorFetch('POST', '/v1/wallet/send', payload, 'Authorize outgoing payment')
      if (!r) return
      const j = (await r.json().catch(() => ({}))) as { error?: string }
      if (!r.ok) onToast(j.error ?? `Send failed (${r.status})`, true)
      else {
        sendSats = ''
        onCloseSheet()
        onToast(`Sent ${fmtSats(amt)} sats ✓`)
        await onRefreshAll()
      }
    } finally {
      onSetBusy(false)
    }
  }

  async function sweepAll(toDesktop: boolean) {
    const dest = sendTo.trim()
    if (!toDesktop && !dest) return onToast('Enter destination address', true)
    const ok = await onConfirm(
      'Sweep Wallet',
      `Move all ${fmtSats(s.treasury.sats)} sats to ${toDesktop ? 'BSV Desktop' : shortHash(dest, 10, 6)}?`
    )
    if (!ok) return
    const payload: Record<string, unknown> = toDesktop ? { toDesktop: true } : { to: dest }
    if (s.network === 'main') payload.confirm = 'I_UNDERSTAND'
    onSetBusy(true)
    try {
      const r = await operatorFetch('POST', '/v1/wallet/sweep', payload, 'Authorize full wallet sweep')
      if (!r) return
      const j = (await r.json().catch(() => ({}))) as { sweptSats?: number; error?: string }
      if (!r.ok) onToast(j.error ?? `Sweep failed (${r.status})`, true)
      else {
        onCloseSheet()
        onToast(`Swept ${fmtSats(j.sweptSats ?? 0)} sats ✓`)
        await onRefreshAll()
      }
    } finally {
      onSetBusy(false)
    }
  }
</script>

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
      <button class="mini" onclick={onSync} disabled={busy}>{busy ? 'Checking…' : 'Check Deposit'}</button>
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

<style>
  .sheet {
    padding: 1.1rem 1.25rem; border-bottom: 1px solid var(--line); background: var(--surf);
    display: grid; gap: 0.65rem;
  }
  .receive-sheet { display: grid; gap: 0.7rem; }
  .qr-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 0.6rem; flex-wrap: wrap; }
  .qr-tip { font-size: 0.76rem; color: var(--mut); margin-top: 0.15rem; }
  .qr-tip b { color: var(--ink); }
  .qr-tabs { display: flex; gap: 0.35rem; }
  .mini {
    flex-shrink: 0;
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.28rem 0.75rem; font: inherit; font-size: 0.76rem; font-weight: 800; cursor: pointer;
  }
  .mini.on { border-color: var(--amber); color: var(--amber); background: rgb(247 153 27 / 12%); }
  .qr-wrap { display: flex; justify-content: center; padding: 0.25rem 0; }
  .qr-box {
    width: 196px; height: 196px; background: #ffffff; padding: 8px;
    border-radius: 12px; border: 1px solid var(--line2); box-shadow: 0 8px 24px rgb(0 0 0 / 45%);
  }
  .qr-box :global(svg) { width: 100%; height: 100%; display: block; }
  .slbl { font-size: 0.72rem; font-weight: 700; color: var(--mut2); text-transform: uppercase; letter-spacing: 0.09em; }
  .addr-row {
    display: flex; align-items: center; gap: 0.5rem; min-width: 0;
    background: var(--bg); border: 1px solid var(--line); border-radius: 11px; padding: 0.55rem 0.8rem;
  }
  .addr-row code { flex: 1; min-width: 0; font-size: 0.82rem; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
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
</style>
