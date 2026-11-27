<script lang="ts">
  import type { RepoBlob, RepoTreeEntry } from '../lib/types'
  import { decodeInline, fmtBytes, prettyContent } from './CapsuleInspectorUtils'

  interface Props {
    entries: RepoTreeEntry[]
    blobs: Record<string, RepoBlob> | undefined
    selectedFile?: RepoTreeEntry | null
  }

  let { entries, blobs, selectedFile = $bindable(null) }: Props = $props()
</script>

{#if entries.length === 0}
  <div class="empty">No files in head tree.</div>
{:else if selectedFile}
  {@const blob = blobs?.[selectedFile.blobHash]}
  {@const isImg = Boolean(selectedFile.mediaType?.startsWith('image/') && blob?.inline && !selectedFile.encrypted)}
  <div class="file-view">
    <div class="fv-head">
      <button class="fv-back" onclick={() => (selectedFile = null)}>← Files</button>
      <span class="fv-path">{selectedFile.path}</span>
      <span class="fv-size">{fmtBytes(selectedFile.size)}</span>
    </div>
    {#if selectedFile.encrypted}
      <pre class="fv-code muted">Encrypted under BRC-369 Borg group {blob?.borgGroup ?? ''} (epoch {blob?.borgEpoch ?? 0})</pre>
    {:else if isImg && blob?.inline}
      <div class="img-wrap">
        <img src={`data:${selectedFile.mediaType};base64,${blob.inline}`} alt={selectedFile.path} />
      </div>
    {:else if blob?.inline}
      <pre class="fv-code">{prettyContent(selectedFile, decodeInline(blob.inline))}</pre>
    {:else if selectedFile.chirpUrl}
      <pre class="fv-code muted">Volumetric CHIRP object ({fmtBytes(selectedFile.size)})
{selectedFile.chirpUrl}</pre>
    {:else}
      <pre class="fv-code muted">Binary object ({fmtBytes(selectedFile.size)})</pre>
    {/if}
  </div>
{:else}
  <div class="list">
    {#each entries as e (e.path)}
      <button class="tree-row" onclick={() => (selectedFile = e)}>
        <span class="tr-path">{e.path}</span>
        <span class="tr-meta">
          {#if e.encrypted}<span class="tr-note">encrypted</span>{/if}
          {#if e.chirpUrl}<span class="tr-note">chirp</span>{/if}
          <span class="tr-size">{fmtBytes(e.size)}</span>
        </span>
      </button>
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

  .tree-row {
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
    cursor: pointer;
  }
  .tree-row:hover {
    background: #13110f;
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
  .tr-meta {
    display: flex;
    align-items: center;
    gap: 10px;
  }
  .tr-note {
    font: 500 11px ui-monospace, monospace;
    color: #8f877f;
  }
  .tr-size {
    font: 400 11.5px ui-monospace, monospace;
    color: #5e5750;
  }

  .file-view {
    display: flex;
    flex-direction: column;
  }
  .fv-head {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 9px 18px;
    border-bottom: 1px solid #191614;
    background: #100e0d;
  }
  .fv-back {
    border: 0;
    padding: 0;
    background: transparent;
    color: #9e968d;
    font: 500 12px system-ui, sans-serif;
    cursor: pointer;
  }
  .fv-back:hover {
    color: #f4f0eb;
  }
  .fv-path {
    flex: 1;
    font: 500 12.5px ui-monospace, monospace;
    color: #f4f0eb;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .fv-size {
    font: 400 11.5px ui-monospace, monospace;
    color: #635c55;
  }
  .fv-code {
    margin: 0;
    padding: 14px 18px;
    font: 400 12px/1.6 ui-monospace, SFMono-Regular, Menlo, monospace;
    color: #ded8d0;
    white-space: pre-wrap;
    word-break: break-word;
  }
  .img-wrap {
    padding: 16px 18px;
    display: flex;
    justify-content: center;
  }
  .img-wrap img {
    max-width: 100%;
    max-height: 360px;
    border-radius: 6px;
    border: 1px solid #1e1b18;
  }
  .muted {
    color: #787068;
  }
</style>
