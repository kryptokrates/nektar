<script lang="ts">
  import type { RepoSummary } from '../lib/types'
  import { operatorJSON } from '../lib/api'
  import { shortSha } from './RepoCommitUtils'

  interface Props {
    repo: string
    activeTab: 'history' | 'files'
    commitCount: number
    entryCount: number
    utxoOpen: boolean
    summary: RepoSummary | null
    anchorCount: number
    allSettled: boolean
    busy?: boolean
    onSelectTab: (tab: 'history' | 'files') => void
    onToggleUtxo: () => void
    onCopy: (label: string, val: string) => void
    onReloadRepo: () => Promise<void>
    onRefresh: () => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
  }

  let {
    repo,
    activeTab,
    commitCount,
    entryCount,
    utxoOpen,
    summary,
    anchorCount,
    allSettled,
    busy = $bindable(false),
    onSelectTab,
    onToggleUtxo,
    onCopy,
    onReloadRepo,
    onRefresh,
    onToast
  }: Props = $props()

  let rotateOpen = $state(false)
  let rotateOwnerKey = $state('')

  $effect(() => {
    void repo
    rotateOpen = false
  })

  async function rotateOwner() {
    if (!repo || !rotateOwnerKey.trim() || busy) return
    busy = true
    try {
      const r = await operatorJSON(
        'POST',
        `/v1/repos/${encodeURIComponent(repo)}/rotate`,
        { newOwner: rotateOwnerKey.trim() },
        `Rotate BSV NektarAnchor owner key for ${repo}`
      )
      if (!r) return
      if (!r.ok) {
        onToast(r.data.error ?? `Rotate failed (${r.status})`, true)
        return
      }
      rotateOwnerKey = ''
      rotateOpen = false
      onToast(`Rotated NektarAnchor UTXO owner key ✓`)
      await onReloadRepo()
      await onRefresh()
    } finally {
      busy = false
    }
  }
</script>

<!-- 2. Hairline GitHub-Style Tab Strip + Whisper-Quiet UTXO Lineage Toggle -->
<nav class="subbar" aria-label="Repository views">
  <div class="tabs">
    <button class="tab" class:active={activeTab === 'history'} onclick={() => onSelectTab('history')}>
      History <span class="tab-n">{commitCount}</span>
    </button>
    <button class="tab" class:active={activeTab === 'files'} onclick={() => onSelectTab('files')}>
      Files <span class="tab-n">{entryCount}</span>
    </button>
  </div>

  <button
    class="utxo-toggle"
    class:ok={allSettled}
    class:open={utxoOpen}
    onclick={onToggleUtxo}
    title="Inspect BSV NektarAnchor UTXO spend lineage"
  >
    <span class="ut-dot"></span>
    {#if anchorCount > 0}
      <span>{anchorCount} {anchorCount === 1 ? 'UTXO' : 'UTXOs'}</span>
    {:else}
      <span>Local</span>
    {/if}
    <svg class="chev" viewBox="0 0 12 12" fill="none" aria-hidden="true">
      <path d="M3 4.5 6 7.5 9 4.5" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" />
    </svg>
  </button>
</nav>

<!-- Collapsible On-Chain Lineage Sheet (hidden by default — zero noise unless clicked) -->
{#if utxoOpen}
  <div class="utxo-sheet">
    {#if summary?.genesisShardId}
      <div class="us-row">
        <span class="us-k">Genesis</span>
        <button class="us-v" onclick={() => onCopy('Genesis ID', summary?.genesisShardId ?? '')}>
          {summary.genesisShardId}
        </button>
      </div>
    {/if}
    {#if anchorCount > 0}
      <div class="us-chain">
        {#each summary?.anchors ?? [] as a, idx}
          {#if idx > 0}
            <span class="us-arr">→</span>
          {/if}
          <button
            class="us-hop"
            class:settled={a.settled}
            onclick={() => onCopy(`UTXO n=${a.n}`, `${a.txid}:0`)}
            title={`Batch n=${a.n} · ${a.size} leaves · ${a.settled ? `Block #${a.blockHeight}` : 'Broadcast'} — click to copy outpoint`}
          >
            <span class="us-n">#{a.n}</span>
            <span>{shortSha(a.txid, 8)}:0</span>
          </button>
        {/each}
      </div>
    {:else}
      <p class="us-note">Signed locally in the commit DAG. Anchor to bind the head UTXO on BSV.</p>
    {/if}

    <div class="us-foot">
      <button class="txt-btn" onclick={() => (rotateOpen = !rotateOpen)}>
        {rotateOpen ? 'Cancel rotation' : 'Rotate Owner Key…'}
      </button>
    </div>

    {#if rotateOpen}
      <form
        class="rot-form"
        onsubmit={(e) => {
          e.preventDefault()
          void rotateOwner()
        }}
      >
        <input
          bind:value={rotateOwnerKey}
          placeholder="New owner 33-byte pubkey hex or 20-byte PKH…"
          class="cp-input mono"
        />
        <button type="submit" class="cp-submit" disabled={!rotateOwnerKey.trim() || busy}>Rotate</button>
      </form>
    {/if}
  </div>
{/if}

<style>
  /* 2. Hairline Sub-Navigation */
  .subbar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 0 18px;
    height: 38px;
    border-bottom: 1px solid #1a1714;
    flex-shrink: 0;
  }

  .tabs {
    display: flex;
    align-items: stretch;
    gap: 18px;
    height: 100%;
  }

  .tab {
    position: relative;
    display: inline-flex;
    align-items: center;
    gap: 6px;
    padding: 0 2px;
    border: 0;
    background: transparent;
    color: #7a726a;
    font: 500 12.5px system-ui, sans-serif;
    cursor: pointer;
    transition: color 120ms;
  }
  .tab:hover {
    color: #d6cfc7;
  }
  .tab.active {
    color: #f4f0eb;
  }
  .tab.active::after {
    content: '';
    position: absolute;
    left: 0;
    right: 0;
    bottom: -1px;
    height: 1.5px;
    background: #f7991b;
  }

  .tab-n {
    font: 500 11px ui-monospace, monospace;
    color: #635c55;
  }
  .tab.active .tab-n {
    color: #9e968d;
  }

  .utxo-toggle {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    height: 24px;
    padding: 0 7px;
    border-radius: 5px;
    border: 1px solid transparent;
    background: transparent;
    color: #7a726a;
    font: 500 11.5px ui-monospace, monospace;
    cursor: pointer;
    transition: background 120ms, color 120ms;
  }
  .utxo-toggle:hover,
  .utxo-toggle.open {
    background: #161412;
    color: #d6cfc7;
  }
  .utxo-toggle.ok {
    color: #68b87d;
  }

  .ut-dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: currentColor;
  }

  .chev {
    width: 11px;
    height: 11px;
    opacity: 0.6;
  }

  /* Collapsible UTXO Lineage Sheet */
  .utxo-sheet {
    padding: 10px 18px 12px;
    background: #090807;
    border-bottom: 1px solid #1a1714;
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .us-row {
    display: flex;
    align-items: center;
    gap: 10px;
    font-size: 11.5px;
  }
  .us-k {
    color: #6b635b;
    width: 54px;
    flex-shrink: 0;
  }
  .us-v {
    border: 0;
    padding: 0;
    background: transparent;
    color: #a89f96;
    font: 500 11.5px ui-monospace, monospace;
    cursor: pointer;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .us-v:hover {
    color: #f4f0eb;
  }
  .us-chain {
    display: flex;
    align-items: center;
    gap: 6px;
    overflow-x: auto;
    padding: 2px 0;
  }
  .us-arr {
    color: #3f3933;
    font-size: 11px;
  }
  .us-hop {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    padding: 2px 7px;
    border-radius: 4px;
    border: 1px solid #25201c;
    background: #12100e;
    color: #c9a165;
    font: 500 11px ui-monospace, monospace;
    cursor: pointer;
    flex-shrink: 0;
  }
  .us-hop.settled {
    color: #68b87d;
    border-color: #1f3325;
    background: #0d1610;
  }
  .us-hop:hover {
    border-color: #4cc26b;
  }
  .us-n {
    opacity: 0.6;
  }
  .us-note {
    margin: 0;
    font-size: 11.5px;
    color: #706860;
  }
  .us-foot {
    display: flex;
    justify-content: flex-end;
  }
  .rot-form {
    display: flex;
    gap: 8px;
  }

  .cp-input {
    flex: 1;
    min-width: 0;
    background: #090807;
    border: 1px solid #26211d;
    border-radius: 6px;
    padding: 6px 9px;
    color: #f4f0eb;
    font-size: 12px;
    outline: none;
  }
  .cp-input:focus {
    border-color: #5a4630;
  }
  .cp-submit {
    padding: 5px 12px;
    border: 0;
    border-radius: 6px;
    background: #f7991b;
    color: #120e0c;
    font: 700 11.5px system-ui, sans-serif;
    cursor: pointer;
    flex-shrink: 0;
  }
  .cp-submit:disabled {
    opacity: 0.4;
    cursor: default;
  }

  .txt-btn {
    border: 0;
    padding: 3px 6px;
    border-radius: 4px;
    background: transparent;
    color: #9e968d;
    font: 500 11.5px system-ui, sans-serif;
    cursor: pointer;
  }
  .txt-btn:hover {
    background: #1c1916;
    color: #f4f0eb;
  }
</style>
