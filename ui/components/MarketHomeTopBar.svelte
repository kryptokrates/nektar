<script lang="ts">
  import type { NavTab, PendingApproval } from '../lib/types'
  import { fmtSats } from '../lib/format'

  interface Props {
    searchQuery: string
    sats: number
    network: string
    faucetBusy: boolean
    pendingApprovals: PendingApproval[]
    onSearch: (q: string) => void
    onOpenPalette: () => void
    onRequestFaucet: () => void
    onNavigate: (tab: NavTab) => void
    onDecideApproval: (id: string, approve: boolean) => void
  }

  let {
    searchQuery,
    sats,
    network,
    faucetBusy,
    pendingApprovals,
    onSearch,
    onOpenPalette,
    onRequestFaucet,
    onNavigate,
    onDecideApproval
  }: Props = $props()
</script>

<!-- Single Minimal Top Bar -->
<header class="topbar">
  <div class="search-box">
    <svg viewBox="0 0 20 20" width="14" height="14" class="sico">
      <circle cx="9" cy="9" r="5.5" fill="none" stroke="currentColor" stroke-width="1.7" />
      <path d="M13.2 13.2L17 17" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" />
    </svg>
    <input
      value={searchQuery}
      oninput={(e) => onSearch((e.currentTarget as HTMLInputElement).value)}
      placeholder="Search market…"
      aria-label="Search market"
    />
    {#if searchQuery}
      <button class="clr" onclick={() => onSearch('')} aria-label="Clear search">✕</button>
    {:else}
      <button class="kbd" onclick={onOpenPalette} title="Command Palette">⌘K</button>
    {/if}
  </div>

  <div class="tright">
    {#if sats === 0 && network === 'testnet'}
      <button class="faucet-btn" onclick={onRequestFaucet} disabled={faucetBusy}>
        {faucetBusy ? 'Loading…' : '+ Free sats'}
      </button>
    {/if}
    <button class="bal-pill" onclick={() => onNavigate('wallet')} title="Chain & Vault (UTXOs, BRC-29, Covenants)">
      <span class="bval">{fmtSats(sats)}</span>
      <span class="bunit">sats</span>
    </button>
  </div>
</header>

<!-- Only surfaces if an operator approval is actively pending -->
{#if pendingApprovals.length > 0}
  <section class="approvals-strip" aria-label="Pending approvals">
    {#each pendingApprovals as a}
      <div class="appr-row">
        <span class="appr-badge">APPROVE</span>
        <span class="appr-txt">{a.reason} · ≤ {fmtSats(a.maxExtraSats)} sats</span>
        <div class="appr-acts">
          <button class="btn-ok" onclick={() => onDecideApproval(a.id, true)}>Approve</button>
          <button class="btn-ghost" onclick={() => onDecideApproval(a.id, false)}>Deny</button>
        </div>
      </div>
    {/each}
  </section>
{/if}

<style>
  /* Minimal Top Bar (pixel-locked 54px height shared with AgentsView) */
  .topbar {
    position: sticky;
    top: 0;
    z-index: 20;
    height: 54px;
    box-sizing: border-box;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 1rem;
    padding: 0 1.25rem;
    background: rgb(9 8 7 / 92%);
    backdrop-filter: blur(12px);
    border-bottom: 1px solid var(--line);
  }
  .search-box {
    flex: 1;
    max-width: 340px;
    display: flex;
    align-items: center;
    gap: 0.5rem;
    background: var(--surf);
    border: 1px solid var(--line);
    border-radius: 999px;
    padding: 0.36rem 0.78rem;
  }
  .search-box:focus-within { border-color: var(--line2); }
  .sico { color: var(--mut2); flex-shrink: 0; }
  .search-box input {
    flex: 1; min-width: 0; background: none; border: 0;
    color: var(--ink); font: inherit; font-size: 0.82rem; outline: none;
  }
  .search-box input::placeholder { color: var(--mut2); }
  .clr, .kbd {
    background: none; border: 0; color: var(--mut2);
    font-family: ui-monospace, monospace; font-size: 0.7rem; cursor: pointer; padding: 0.08rem 0.2rem;
  }
  .clr:hover, .kbd:hover { color: var(--ink); }
  .tright {
    display: flex; align-items: center; gap: 0.55rem; flex-shrink: 0;
  }
  .faucet-btn {
    background: rgb(247 153 27 / 12%); color: var(--amber);
    border: 1px solid rgb(247 153 27 / 35%); border-radius: 999px;
    padding: 0.3rem 0.75rem; font: inherit; font-size: 0.76rem; font-weight: 700; cursor: pointer;
  }
  .bal-pill {
    display: inline-flex; align-items: baseline; gap: 0.3rem;
    background: var(--surf); border: 1px solid var(--line); border-radius: 999px;
    padding: 0.32rem 0.8rem; color: var(--ink); font: inherit; cursor: pointer;
    transition: border-color 0.14s;
  }
  .bal-pill:hover { border-color: var(--amber); }
  .bval { font-family: ui-monospace, monospace; font-size: 0.86rem; font-weight: 800; font-variant-numeric: tabular-nums; }
  .bunit { font-size: 0.72rem; font-weight: 700; color: var(--amber); }

  /* Pending Approvals */
  .approvals-strip {
    background: rgb(247 153 27 / 8%);
    border-bottom: 1px solid rgb(247 153 27 / 30%);
    padding: 0.6rem 1.25rem;
    display: grid; gap: 0.45rem;
  }
  .appr-row { display: flex; align-items: center; gap: 0.7rem; flex-wrap: wrap; font-size: 0.82rem; }
  .appr-badge {
    background: var(--amber); color: #120e0c; font-size: 0.65rem; font-weight: 800;
    padding: 0.12rem 0.48rem; border-radius: 999px;
  }
  .appr-txt { flex: 1; color: var(--ink); }
  .appr-acts { display: flex; gap: 0.4rem; }
  .btn-ok {
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.3rem 0.8rem; font: inherit; font-size: 0.76rem; font-weight: 800; cursor: pointer;
  }
  .btn-ghost {
    background: transparent; color: var(--mut); border: 1px solid var(--line2);
    border-radius: 999px; padding: 0.3rem 0.75rem; font: inherit; font-size: 0.76rem; cursor: pointer;
  }
</style>
