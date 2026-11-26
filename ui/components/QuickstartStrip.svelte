<script lang="ts">
  import type { NavTab, State } from '../lib/types'

  interface Props {
    s: State
    onOpenDock: () => void
    onNavigate: (tab: NavTab) => void
    onApproveStarter: () => void
    onRequestFaucet: () => void
  }

  let { s, onOpenDock, onNavigate, onApproveStarter, onRequestFaucet }: Props = $props()
  let dismissed = $state(typeof localStorage !== 'undefined' && localStorage.getItem('nektar_qs_dismissed') === '1')

  const funded = $derived((s.treasury?.sats ?? 0) > 0)
  const policyReady = $derived((s.policies?.length ?? 0) > 0)
  const hasMemories = $derived((s.memories?.length ?? 0) > 1)
  const allDone = $derived(funded && policyReady && hasMemories)

  function dismiss() {
    dismissed = true
    try {
      localStorage.setItem('nektar_qs_dismissed', '1')
    } catch {}
  }
</script>

{#if !dismissed && !allDone}
  <div class="qs">
    <div class="qhead">
      <b>First 60 Seconds on Nektar ({s.network === 'main' ? 'BSV Mainnet' : s.network})</b>
      <button class="qx" onclick={dismiss} title="Dismiss">✕</button>
    </div>
    <div class="steps">
      <button class="step" class:done={hasMemories} onclick={onOpenDock}>
        <span class="num">{hasMemories ? '✓' : '1'}</span>
        <span>
          <b>Dock your AI app</b>
          <small>1-click Claude, Cursor, VS Code, Codex or L0 Proxy</small>
        </span>
      </button>

      <button class="step" class:done={funded} onclick={() => (s.network === 'testnet' ? onRequestFaucet() : onNavigate('wallet'))}>
        <span class="num">{funded ? '✓' : '2'}</span>
        <span>
          <b>{s.network === 'main' ? 'Fund wallet (QR)' : 'Get free test sats'}</b>
          <small>{s.network === 'main' ? 'Scan QR with 5k–10k sats (~$0.003)' : '1-click Testnet faucet'}</small>
        </span>
      </button>

      <button class="step" class:done={policyReady} onclick={onApproveStarter}>
        <span class="num">{policyReady ? '✓' : '3'}</span>
        <span>
          <b>Enable knowledge market</b>
          <small>Approve capped Buyer + Seller policy</small>
        </span>
      </button>
    </div>
  </div>
{/if}

<style>
  .qs {
    margin: 0.75rem 0.9rem 0.25rem;
    padding: 0.75rem 0.9rem;
    background: linear-gradient(180deg, rgb(247 153 27 / 9%), var(--surf));
    border: 1px solid rgb(247 153 27 / 28%);
    border-radius: 12px;
    display: grid;
    gap: 0.55rem;
  }
  .qhead { display: flex; align-items: center; justify-content: space-between; font-size: 0.8rem; color: var(--amber); }
  .qx { background: none; border: 0; color: var(--mut); cursor: pointer; font-size: 0.85rem; }
  .steps { display: grid; grid-template-columns: repeat(auto-fit, minmax(175px, 1fr)); gap: 0.45rem; }
  .step {
    display: flex; align-items: center; gap: 0.55rem; text-align: left;
    background: var(--bg); border: 1px solid var(--line); border-radius: 9px;
    padding: 0.5rem 0.65rem; cursor: pointer; color: var(--ink);
  }
  .step:hover { border-color: var(--amber); }
  .step.done { opacity: 0.68; border-color: rgb(76 194 107 / 35%); }
  .num {
    width: 22px; height: 22px; border-radius: 999px; flex-shrink: 0;
    display: grid; place-items: center; font-size: 0.72rem; font-weight: 800;
    background: rgb(247 153 27 / 18%); color: var(--amber);
  }
  .step.done .num { background: rgb(76 194 107 / 20%); color: var(--ok); }
  .step b { display: block; font-size: 0.78rem; }
  .step small { display: block; font-size: 0.7rem; color: var(--mut); }
</style>
