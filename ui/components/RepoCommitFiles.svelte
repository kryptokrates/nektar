<script lang="ts">
  import type { RepoBlob, RepoTreeEntry } from '../lib/types'
  import { fmtBytes } from './RepoCommitUtils'
  import RepoCommitFileView from './RepoCommitFileView.svelte'

  interface Props {
    repo: string
    activeBranch: string
    pinnedRef: string | null
    pinnedLabel: string
    entries: RepoTreeEntry[]
    blobs: Record<string, RepoBlob>
    selectedFile?: RepoTreeEntry | null
    busy?: boolean
    onExitTimeTravel: () => void
    onFileDeleted: () => Promise<void>
    onFileSaved: (path: string) => Promise<void>
    onCopy: (label: string, val: string) => void
    onToast: (msg: string, isErr?: boolean) => void
  }

  let {
    repo,
    activeBranch,
    pinnedRef,
    pinnedLabel,
    entries,
    blobs,
    selectedFile = $bindable(null),
    busy = $bindable(false),
    onExitTimeTravel,
    onFileDeleted,
    onFileSaved,
    onCopy,
    onToast
  }: Props = $props()
</script>

<!-- Time-Travel Ribbon (only shown when pinned to a historical commit or tag) -->
{#if pinnedRef}
  <div class="tt-ribbon">
    <span>Viewing <code>{pinnedLabel}</code></span>
    <button class="txt-btn amber" onclick={onExitTimeTravel}>Return to {activeBranch}</button>
  </div>
{/if}

<!-- Clean GitHub-Style File Tree -->
{#if entries.length === 0}
  <div class="empty">Empty tree on <code>{pinnedRef ? pinnedLabel : activeBranch}</code>.</div>
{:else if selectedFile}
  <RepoCommitFileView
    {repo}
    {activeBranch}
    {pinnedRef}
    {selectedFile}
    blob={blobs[selectedFile.blobHash]}
    bind:busy
    onBack={() => (selectedFile = null)}
    {onFileDeleted}
    {onFileSaved}
    {onCopy}
    {onToast}
  />
{:else}
  <div class="tree-list">
    {#each entries as e (e.path)}
      <button class="tree-row" onclick={() => (selectedFile = e)}>
        <svg class="file-ico" viewBox="0 0 16 16" fill="none" aria-hidden="true">
          <path
            d="M4 1.75h5.25L12.5 5v9.25H4V1.75Z"
            stroke="currentColor"
            stroke-width="1.25"
            stroke-linejoin="round"
          />
          <path d="M9 1.75V5h3.5" stroke="currentColor" stroke-width="1.25" stroke-linejoin="round" />
        </svg>
        <span class="tr-path">{e.path}</span>
        <span class="tr-meta">
          {#if e.encrypted}
            <span class="tr-note">encrypted</span>
          {:else if e.chirpUrl}
            <span class="tr-note">chirp</span>
          {/if}
          <span class="tr-size">{fmtBytes(e.size)}</span>
        </span>
      </button>
    {/each}
  </div>
{/if}

<style>
  /* Time-travel Ribbon */
  .tt-ribbon {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 7px 18px;
    background: #17120b;
    border-bottom: 1px solid #332614;
    font-size: 12px;
    color: #e4a149;
  }
  .tt-ribbon code {
    font-family: ui-monospace, monospace;
  }

  .empty {
    padding: 40px 18px;
    text-align: center;
    color: #6c645c;
    font-size: 13px;
  }
  .empty code {
    font-family: ui-monospace, monospace;
    color: #a89f96;
  }

  /* GitHub-Style File Tree */
  .tree-list {
    display: flex;
    flex-direction: column;
  }
  .tree-row {
    display: flex;
    align-items: center;
    gap: 10px;
    width: 100%;
    height: 40px;
    padding: 0 18px;
    border: 0;
    border-bottom: 1px solid #151311;
    background: transparent;
    color: inherit;
    text-align: left;
    cursor: pointer;
    transition: background 100ms;
  }
  .tree-row:hover {
    background: #13110f;
  }
  .file-ico {
    width: 14px;
    height: 14px;
    color: #6e665e;
    flex-shrink: 0;
  }
  .tr-path {
    flex: 1;
    min-width: 0;
    font: 500 12.5px ui-monospace, SFMono-Regular, Menlo, monospace;
    color: #e4ded7;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .tr-meta {
    display: flex;
    align-items: center;
    gap: 10px;
    flex-shrink: 0;
  }
  .tr-note {
    font: 500 11px ui-monospace, monospace;
    color: #8f877f;
  }
  .tr-size {
    font: 400 11.5px ui-monospace, monospace;
    color: #5e5750;
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
  .txt-btn.amber {
    color: #f7991b;
  }
</style>
