<script lang="ts">
  import type { RepoCommit } from '../lib/types'
  import { agoUnix } from '../lib/format'
  import { firstLine, shortSha } from './CapsuleInspectorUtils'

  interface Props {
    commits: RepoCommit[]
    expandedCommit?: string | null
    onCopy: (label: string, val: string) => void
  }

  let { commits, expandedCommit = $bindable(null), onCopy }: Props = $props()
</script>

{#if commits.length === 0}
  <div class="empty">No commits packaged in this capsule.</div>
{:else}
  <div class="list">
    {#each commits as c, i (c.commitHash)}
      {@const isExp = expandedCommit === c.commitHash}
      {@const isAnchored = Boolean(c.anchorTxid)}
      <div class="log-entry" class:expanded={isExp}>
        <button class="log-row" onclick={() => (expandedCommit = isExp ? null : c.commitHash)}>
          <svg class="rail-svg" viewBox="0 0 18 40" aria-hidden="true">
            {#if i > 0}
              <line x1="9" y1="0" x2="9" y2="20" stroke={isAnchored ? '#2d6a42' : '#2b2622'} stroke-width="1.5" />
            {/if}
            {#if i < commits.length - 1}
              <line x1="9" y1="20" x2="9" y2="40" stroke={isAnchored ? '#2d6a42' : '#2b2622'} stroke-width="1.5" />
            {/if}
            {#if isAnchored}
              <circle cx="9" cy="20" r="3.4" fill="#4cc26b" />
            {:else}
              <circle cx="9" cy="20" r="3.2" fill="#0d0c0b" stroke="#6c645c" stroke-width="1.4" />
            {/if}
          </svg>
          <span class="log-msg">{firstLine(c.message)}</span>
          <span class="log-sha">{shortSha(c.commitHash, 7)}</span>
          <span class="log-time">{agoUnix(c.timestamp)}</span>
        </button>
        {#if isExp}
          <dl class="kv">
            <dt>Commit</dt>
            <dd><button class="kv-copy" onclick={() => onCopy('commit hash', c.commitHash)}>{c.commitHash}</button></dd>
            <dt>UTXO</dt>
            <dd>
              {#if c.anchorTxid}
                <button class="kv-copy ok" onclick={() => onCopy('UTXO', `${c.anchorTxid}:0`)}>
                  {shortSha(c.anchorTxid, 14)}:0
                </button>
                <span class="kv-note">batch #{c.anchorN}</span>
              {:else}
                <span class="kv-muted">Included in capsule head</span>
              {/if}
            </dd>
            {#if c.modelId}
              <dt>Model</dt>
              <dd><span class="kv-muted">{c.modelId}</span></dd>
            {/if}
          </dl>
        {/if}
      </div>
    {/each}
  </div>
{/if}

<style>
  .empty {
    padding: 36px 18px;
    text-align: center;
    color: #6c645c;
    font-size: 13px;
  }

  .list {
    display: flex;
    flex-direction: column;
  }

  .log-entry {
    border-bottom: 1px solid #151311;
  }
  .log-entry.expanded {
    background: #100e0d;
  }

  .log-row {
    display: flex;
    align-items: center;
    gap: 10px;
    width: 100%;
    height: 40px;
    padding: 0 18px 0 14px;
    border: 0;
    background: transparent;
    color: inherit;
    text-align: left;
    cursor: pointer;
  }
  .log-row:hover {
    background: #13110f;
  }

  .rail-svg {
    width: 18px;
    height: 40px;
    flex-shrink: 0;
  }

  .log-msg {
    flex: 1;
    min-width: 0;
    font: 500 13px system-ui, sans-serif;
    color: #ece7e1;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .log-sha {
    font: 500 11.5px ui-monospace, monospace;
    color: #706860;
  }

  .log-time {
    font: 400 11.5px system-ui, sans-serif;
    color: #57514b;
    min-width: 30px;
    text-align: right;
  }

  .kv {
    display: grid;
    grid-template-columns: 60px 1fr;
    gap: 4px 10px;
    margin: 0;
    padding: 2px 18px 12px 42px;
    font-size: 11.5px;
  }
  .kv dt {
    color: #5e5750;
  }
  .kv dd {
    margin: 0;
    display: flex;
    align-items: center;
    gap: 8px;
    min-width: 0;
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
  .kv-note,
  .kv-muted {
    color: #6b635b;
  }
</style>
