<script lang="ts">
  import type { NavTab, State } from '../lib/types'
  import { fmtSats, sparklinePath } from '../lib/format'

  interface Props {
    s: State
    searchQuery: string
    faucetBusy: boolean
    onSearch: (q: string) => void
    onOpenPalette: () => void
    onOpenDock: () => void
    onQuickRecall: () => void
    onRequestFaucet: () => void
    onApproveStarter: () => void
    onNavigate: (tab: NavTab) => void
  }

  let {
    s,
    searchQuery,
    faucetBusy,
    onSearch,
    onOpenPalette,
    onOpenDock,
    onQuickRecall,
    onRequestFaucet,
    onApproveStarter,
    onNavigate
  }: Props = $props()

  let setupHidden = $state(typeof localStorage !== 'undefined' && localStorage.getItem('nektar-setup-hidden') === '1')

  const hasPolicy = $derived(s.settings.policies.some((p) => !p.revokedAt))
  const setupSteps = $derived([
    { id: 'dock', label: 'Dock your AI agent', done: false, action: onOpenDock },
    { id: 'receipt', label: 'Issue first verified answer', done: s.receipts.count > 0, action: onQuickRecall },
    {
      id: 'fund',
      label: s.network === 'testnet' ? 'Get free test sats' : 'Fund wallet',
      done: s.treasury.sats > 0,
      action: s.network === 'testnet' ? onRequestFaucet : () => onNavigate('wallet')
    },
    { id: 'policy', label: 'Enable autonomous trading', done: hasPolicy, action: onApproveStarter }
  ])
  const doneCount = $derived(setupSteps.filter((st) => st.done).length)
  const spark = $derived(sparklinePath(s.receipts.amounts))

  function hideSetup() {
    setupHidden = true
    try {
      localStorage.setItem('nektar-setup-hidden', '1')
    } catch {}
  }
</script>

<aside class="rail-right">
  <div class="search-box">
    <svg viewBox="0 0 20 20" width="15" height="15" class="sico"><circle cx="9" cy="9" r="5.5" fill="none" stroke="currentColor" stroke-width="1.7" /><path d="M13.2 13.2L17 17" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" /></svg>
    <input
      value={searchQuery}
      oninput={(e) => onSearch((e.currentTarget as HTMLInputElement).value)}
      placeholder="Search…"
      aria-label="Search timeline"
    />
    {#if searchQuery}
      <button class="clr" onclick={() => onSearch('')} aria-label="Clear">✕</button>
    {:else}
      <button class="kbd" onclick={onOpenPalette} title="Command Palette">⌘K</button>
    {/if}
  </div>

  <section class="panel">
    <div class="phead">
      <span>BALANCE</span>
      <button class="link" onclick={() => onNavigate('wallet')}>Wallet →</button>
    </div>
    <div class="bal">{fmtSats(s.treasury.sats)} <small>sats</small></div>
    {#if s.receipts.earned > 0}
      <div class="earned">+{fmtSats(s.receipts.earned)} sats earned</div>
    {/if}

    {#if spark.line}
      <svg viewBox="0 0 220 42" class="spark" preserveAspectRatio="none">
        <path d={spark.fill} fill="rgb(76 194 107 / 14%)" />
        <path d={spark.line} fill="none" stroke="#4cc26b" stroke-width="2" stroke-linejoin="round" />
      </svg>
    {/if}

    {#if s.treasury.sats === 0 && s.network === 'testnet'}
      <button class="wact" onclick={onRequestFaucet} disabled={faucetBusy}>
        {faucetBusy ? 'Getting sats…' : 'Get free test sats'}
      </button>
    {:else}
      <button class="wact" onclick={() => onNavigate('wallet')}>Deposit / Send</button>
    {/if}
  </section>

  {#if !setupHidden && doneCount < setupSteps.length}
    <section class="panel">
      <div class="phead">
        <span>GET STARTED · {doneCount}/{setupSteps.length}</span>
        <button class="link" onclick={hideSetup}>Hide</button>
      </div>
      <div class="checklist">
        {#each setupSteps as st}
          <button class="citem" class:done={st.done} onclick={st.action}>
            <span class="cbox">{st.done ? '✓' : ''}</span>
            <span>{st.label}</span>
          </button>
        {/each}
      </div>
    </section>
  {/if}
</aside>

<style>
  .rail-right {
    position: sticky; top: 0;
    display: flex; flex-direction: column; gap: 1rem;
    padding: 1.05rem 0.5rem 1.5rem 1rem;
  }
  .search-box {
    display: flex; align-items: center; gap: 0.5rem;
    background: var(--surf); border: 1px solid var(--line); border-radius: 999px;
    padding: 0.5rem 0.85rem;
  }
  .search-box:focus-within { border-color: var(--amber); }
  .sico { color: var(--mut2); flex-shrink: 0; }
  .search-box input {
    flex: 1; min-width: 0; background: none; border: 0;
    color: var(--ink); font: inherit; font-size: 0.85rem; outline: none;
  }
  .search-box input::placeholder { color: var(--mut2); }
  .clr, .kbd {
    background: none; border: 0; color: var(--mut2);
    font-family: ui-monospace, monospace; font-size: 0.7rem; cursor: pointer; padding: 0.1rem 0.25rem;
  }
  .kbd:hover, .clr:hover { color: var(--ink); }
  .panel {
    border: 1px solid var(--line); border-radius: 16px;
    padding: 1.05rem 1.1rem; background: var(--surf);
  }
  .phead {
    display: flex; align-items: center; justify-content: space-between;
    font-size: 0.68rem; font-weight: 700; letter-spacing: 0.1em; color: var(--mut2); margin-bottom: 0.55rem;
  }
  .link {
    background: none; border: 0; color: var(--mut); font: inherit;
    font-size: 0.74rem; letter-spacing: 0; font-weight: 600; cursor: pointer; padding: 0;
  }
  .link:hover { color: var(--amber); }
  .bal {
    font-size: 2rem; font-weight: 800; color: var(--ink);
    letter-spacing: -0.025em; font-variant-numeric: tabular-nums; line-height: 1.05;
  }
  .bal small { font-size: 0.88rem; font-weight: 700; color: var(--amber); }
  .earned { margin-top: 0.3rem; font-size: 0.78rem; color: var(--ok); font-weight: 600; font-variant-numeric: tabular-nums; }
  .spark { display: block; width: 100%; height: 38px; margin-top: 0.65rem; }
  .wact {
    margin-top: 0.85rem; width: 100%; background: transparent;
    border: 1px solid var(--line2); color: var(--ink); border-radius: 999px;
    padding: 0.48rem 0.75rem; font: inherit; font-size: 0.82rem; font-weight: 700; cursor: pointer;
    transition: border-color 0.14s, color 0.14s;
  }
  .wact:hover:not(:disabled) { border-color: var(--amber); color: var(--amber); }
  .wact:disabled { opacity: 0.5; cursor: default; }
  .checklist { display: flex; flex-direction: column; gap: 0.2rem; }
  .citem {
    display: flex; align-items: center; gap: 0.6rem;
    background: none; border: 0; color: var(--ink); font: inherit; font-size: 0.83rem;
    padding: 0.44rem 0.45rem; margin: 0 -0.45rem; border-radius: 9px; cursor: pointer; text-align: left;
  }
  .citem:hover { background: var(--hover); }
  .citem.done { color: var(--mut2); text-decoration: line-through; }
  .cbox {
    width: 17px; height: 17px; border-radius: 50%; border: 1px solid var(--line2);
    display: grid; place-items: center; font-size: 0.65rem; flex-shrink: 0;
  }
  .citem.done .cbox { background: rgb(76 194 107 / 16%); border-color: var(--ok); color: var(--ok); }

  @media (max-width: 1020px) {
    .rail-right { position: static; padding: 0.9rem 1rem 5rem; }
  }
</style>
