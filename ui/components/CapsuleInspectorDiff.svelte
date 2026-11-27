<script lang="ts">
  import type { TreeDiffEntry } from '../lib/types'

  interface Props {
    repo: string
    diffEntries: TreeDiffEntry[]
  }

  let { repo, diffEntries }: Props = $props()
</script>

{#if diffEntries.length === 0}
  <div class="empty">Identical to local <code>{repo}</code> head tree.</div>
{:else}
  <div class="list">
    {#each diffEntries as d (d.path)}
      <div class="diff-row">
        <span class="df-kind" class:add={d.kind === 'added'} class:del={d.kind === 'deleted'}>
          {d.kind === 'added' ? 'A' : d.kind === 'deleted' ? 'D' : 'M'}
        </span>
        <span class="tr-path">{d.path}</span>
        <span class="tr-note">{d.kind}</span>
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
  .empty code {
    font-family: ui-monospace, monospace;
    color: #a89f96;
  }

  .list {
    display: flex;
    flex-direction: column;
  }

  .diff-row {
    display: flex;
    align-items: center;
    gap: 10px;
    width: 100%;
    height: 38px;
    padding: 0 18px;
    border: 0;
    border-bottom: 1px solid #151311;
    background: transparent;
    color: inherit;
    text-align: left;
  }

  .tr-path {
    flex: 1;
    min-width: 0;
    font: 500 12.5px ui-monospace, monospace;
    color: #e4ded7;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .tr-note {
    font: 500 11px ui-monospace, monospace;
    color: #8f877f;
  }

  .df-kind {
    font: 700 11.5px ui-monospace, monospace;
    color: #e4a149;
    width: 14px;
  }
  .df-kind.add {
    color: #4cc26b;
  }
  .df-kind.del {
    color: #e5534b;
  }
</style>
