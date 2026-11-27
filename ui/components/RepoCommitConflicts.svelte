<script lang="ts">
  import type { MergeConflict } from '../lib/types'

  interface Props {
    sourceBranch: string
    conflicts: MergeConflict[]
    resolutions?: Record<string, string>
    busy: boolean
    onDismiss: () => void
    onComplete: (resolutions: Record<string, string>) => void
  }

  let {
    sourceBranch,
    conflicts,
    resolutions = $bindable({}),
    busy,
    onDismiss,
    onComplete
  }: Props = $props()
</script>

<!-- Inline 3-Way Merge Conflict Resolver (only appears on HTTP 409) -->
<div class="conflict-box">
  <div class="cf-head">
    <span>3-Way Conflict merging <code>{sourceBranch}</code> → <code>main</code></span>
    <button class="txt-btn" onclick={onDismiss}>Dismiss</button>
  </div>
  {#each conflicts as cf (cf.path)}
    <div class="cf-file">
      <div class="cf-fpath">
        <span>{cf.path}</span>
        <div class="cf-pick">
          <button class="txt-btn" onclick={() => (resolutions[cf.path] = cf.oursText ?? '')}>Keep main</button>
          <button class="txt-btn" onclick={() => (resolutions[cf.path] = cf.theirsText ?? '')}>Keep {sourceBranch}</button>
        </div>
      </div>
      <textarea
        class="cp-area mono"
        rows="4"
        bind:value={resolutions[cf.path]}
      ></textarea>
    </div>
  {/each}
  <div class="cf-foot">
    <button class="cp-submit" onclick={() => onComplete(resolutions)} disabled={busy}>
      {busy ? 'Merging…' : 'Complete 3-Way Merge'}
    </button>
  </div>
</div>

<style>
  /* 3-Way Conflict Box */
  .conflict-box {
    padding: 12px 18px;
    background: #140f0c;
    border-bottom: 1px solid #3c2917;
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .cf-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    font-size: 12px;
    color: #f7991b;
  }
  .cf-head code {
    font-family: ui-monospace, monospace;
  }
  .cf-file {
    display: flex;
    flex-direction: column;
    gap: 5px;
  }
  .cf-fpath {
    display: flex;
    align-items: center;
    justify-content: space-between;
    font: 500 11.5px ui-monospace, monospace;
    color: #d6cfc7;
  }
  .cf-pick {
    display: flex;
    gap: 6px;
  }
  .cf-foot {
    display: flex;
    justify-content: flex-end;
  }

  .cp-area {
    width: 100%;
    background: #090807;
    border: 1px solid #26211d;
    border-radius: 6px;
    padding: 8px 10px;
    color: #e4ded7;
    font-size: 12px;
    line-height: 1.55;
    resize: vertical;
    outline: none;
  }
  .cp-area:focus {
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
