<script lang="ts">
  import { onMount } from 'svelte'
  import type { State } from '../lib/types'
  import { fmtSats, sparklinePath } from '../lib/format'
  import WalletViewBaskets from './WalletViewBaskets.svelte'
  import WalletViewSheets from './WalletViewSheets.svelte'
  import WalletViewActivity, { type WalletAction } from './WalletViewActivity.svelte'

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
  let actions = $state<WalletAction[]>([])

  const spark = $derived(sparklinePath(s.receipts.amounts, 320, 44))
  const isDesktopCustody = $derived((s.treasury.walletMode ?? 'embedded') === 'client')
  const baskets = $derived(s.treasury.baskets ?? [])
  const totalBasketItems = $derived(s.treasury.basketItems ?? baskets.reduce((acc, b) => acc + b.count, 0))
  const totalBasketSats = $derived(s.treasury.basketLockedSats ?? baskets.reduce((acc, b) => acc + b.totalSats, 0))

  async function loadActions() {
    try {
      const r = await fetch('/v1/wallet/actions?limit=40')
      if (r.ok) {
        const j = (await r.json()) as { actions?: WalletAction[] }
        actions = j.actions ?? []
      }
    } catch {}
  }

  async function refreshAll() {
    await onRefresh()
    await loadActions()
  }

  onMount(() => {
    void loadActions()
  })

  async function syncWallet() {
    busy = true
    try {
      const r = await fetch('/v1/wallet/sync', { method: 'POST' })
      const j = (await r.json().catch(() => ({}))) as { added?: number; error?: string }
      if (!r.ok) onToast(j.error ?? `Sync failed (${r.status})`, true)
      else {
        onToast(`Wallet synced ✓`)
        await refreshAll()
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
        await refreshAll()
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

      <WalletViewBaskets {baskets} />

      <WalletViewSheets
        {s}
        {sheet}
        {busy}
        onCloseSheet={() => (sheet = 'none')}
        onSetBusy={(v) => (busy = v)}
        onSync={syncWallet}
        onRefreshAll={refreshAll}
        {onToast}
        {onConfirm}
      />
    </div>

    <WalletViewActivity {actions} network={s.network} {isDesktopCustody} />
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
  .spark { display: block; width: 100%; max-width: 300px; height: 40px; margin: 0.9rem auto 0; }
  @media (max-width: 900px) {
    .wallet-grid { grid-template-columns: 1fr; }
    .wallet-left { border-right: 0; }
  }
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
</style>
