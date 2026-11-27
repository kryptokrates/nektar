<script module lang="ts">
  export type WalletAction = {
    txid?: string
    satoshis?: number
    status?: string
    description?: string
    isOutgoing?: boolean
    labels?: string[]
  }
</script>

<script lang="ts">
  import { fmtSats, shortHash } from '../lib/format'

  interface Props {
    actions: WalletAction[]
    network: string
    isDesktopCustody: boolean
  }

  let { actions, network, isDesktopCustody }: Props = $props()

  let agentFilter = $state<string>('all')

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
    if (network === 'main') return `https://whatsonchain.com/tx/${txid}`
    if (network === 'testnet') return `https://test.whatsonchain.com/tx/${txid}`
    return ''
  }
</script>

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
    {@const tx = a.txid || ''}
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

<style>
  .wallet-right {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }
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
