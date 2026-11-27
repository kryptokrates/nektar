<script lang="ts">
  import type { RepoAnchorSummary } from '../lib/types'
  import { shortSha } from './CapsuleInspectorUtils'

  interface Props {
    genesisShardId?: string
    ownerPub?: string
    anchors: RepoAnchorSummary[]
    onCopy: (label: string, val: string) => void
  }

  let { genesisShardId, ownerPub, anchors, onCopy }: Props = $props()
</script>

<div class="utxo-panel">
  {#if genesisShardId}
    <div class="up-row">
      <span class="up-k">Genesis ID</span>
      <button class="kv-copy" onclick={() => onCopy('Genesis ID', genesisShardId ?? '')}>
        {genesisShardId}
      </button>
    </div>
  {/if}
  {#if ownerPub}
    <div class="up-row">
      <span class="up-k">Owner Pub</span>
      <button class="kv-copy" onclick={() => onCopy('Owner pubkey', ownerPub ?? '')}>
        {ownerPub}
      </button>
    </div>
  {/if}
  {#if anchors.length === 0}
    <p class="empty">No BSV NektarAnchor UTXOs packaged in this capsule.</p>
  {:else}
    <div class="hop-list">
      {#each anchors as a, idx}
        <div class="hop-row">
          <span class="hop-n">#{a.n}</span>
          <button class="kv-copy" class:ok={a.settled} onclick={() => onCopy(`UTXO #${a.n}`, `${a.txid}:0`)}>
            {shortSha(a.txid, 16)}:0
          </button>
          <span class="hop-meta">
            {a.size} {a.size === 1 ? 'leaf' : 'leaves'} · {a.settled ? `block #${a.blockHeight}` : 'mempool'}
          </span>
          {#if idx > 0}
            <span class="hop-link">spends #{anchors[idx - 1].n}:0</span>
          {/if}
        </div>
      {/each}
    </div>
  {/if}
</div>

<style>
  .empty {
    padding: 36px 18px;
    text-align: center;
    color: #6c645c;
    font-size: 13px;
  }

  .utxo-panel {
    padding: 14px 18px;
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .up-row {
    display: flex;
    align-items: center;
    gap: 12px;
    font-size: 12px;
  }
  .up-k {
    width: 78px;
    color: #6b635b;
    flex-shrink: 0;
  }
  .hop-list {
    display: flex;
    flex-direction: column;
    gap: 6px;
    margin-top: 4px;
  }
  .hop-row {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 7px 10px;
    border-radius: 6px;
    border: 1px solid #1e1b18;
    background: #100e0d;
    font-size: 11.5px;
    flex-wrap: wrap;
  }
  .hop-n {
    font: 600 11px ui-monospace, monospace;
    color: #8f877f;
    min-width: 24px;
  }
  .hop-meta {
    color: #7d756d;
    margin-left: auto;
  }
  .hop-link {
    font: 500 11px ui-monospace, monospace;
    color: #5e5750;
  }

  .kv-copy {
    border: 0;
    padding: 0;
    background: transparent;
    color: #9e968d;
    font: 500 11.5px ui-monospace, monospace;
    cursor: pointer;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .kv-copy:hover {
    color: #f4f0eb;
  }
  .kv-copy.ok {
    color: #68b87d;
  }
</style>
