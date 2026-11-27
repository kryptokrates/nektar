<script lang="ts">
  import type { State } from '../lib/types'
  import NodeViewPolicyTab from './NodeViewPolicyTab.svelte'
  import NodeViewNetworkTab from './NodeViewNetworkTab.svelte'
  import NodeViewSystemTab from './NodeViewSystemTab.svelte'

  type Sub = 'policy' | 'network' | 'system'

  interface Props {
    s: State
    live: boolean
    sub?: Sub
    onRefresh: () => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
    onConfirm: (title: string, detail: string) => Promise<boolean>
    onOpenDock: () => void
  }

  let { s, live, sub = $bindable<Sub>('policy'), onRefresh, onToast, onConfirm, onOpenDock }: Props = $props()
</script>

<div class="view">
  <header class="sticky-head">
    <div class="toprow">
      <h1>Settings</h1>
      <button class="dock-btn" onclick={onOpenDock}>Dock Agent</button>
    </div>
    <div class="seg">
      <button class="s" class:on={sub === 'policy'} onclick={() => (sub = 'policy')}>Policy</button>
      <button class="s" class:on={sub === 'network'} onclick={() => (sub = 'network')}>Network</button>
      <button class="s" class:on={sub === 'system'} onclick={() => (sub = 'system')}>System</button>
    </div>
  </header>

  {#if sub === 'policy'}
    <NodeViewPolicyTab {s} {onRefresh} {onToast} {onConfirm} />
  {:else if sub === 'network'}
    <NodeViewNetworkTab {s} {onRefresh} {onToast} />
  {:else}
    <NodeViewSystemTab {s} {live} {onToast} />
  {/if}
</div>

<style>
  .view { display: flex; flex-direction: column; }
  .sticky-head {
    position: sticky; top: 0; z-index: 15;
    background: rgb(9 8 7 / 92%); backdrop-filter: blur(12px);
    border-bottom: 1px solid var(--line);
    padding: 0.9rem 1.25rem 0.75rem; display: grid; gap: 0.75rem;
  }
  .toprow { display: flex; align-items: center; justify-content: space-between; }
  h1 { margin: 0; font-size: 1.15rem; font-weight: 800; letter-spacing: -0.02em; }
  .dock-btn {
    background: transparent; border: 1px solid var(--line2); color: var(--ink);
    border-radius: 999px; padding: 0.34rem 0.85rem; font: inherit; font-size: 0.78rem; font-weight: 700; cursor: pointer;
  }
  .dock-btn:hover { border-color: var(--amber); color: var(--amber); }
  .seg {
    display: grid; grid-template-columns: repeat(3, 1fr); gap: 0.22rem;
    background: var(--surf); border: 1px solid var(--line); border-radius: 10px; padding: 0.22rem;
  }
  .s {
    background: none; border: 0; color: var(--mut); font: inherit; font-size: 0.8rem; font-weight: 700;
    padding: 0.42rem 0.25rem; border-radius: 8px; cursor: pointer;
  }
  .s.on { background: #1e1b18; color: var(--ink); }
</style>
