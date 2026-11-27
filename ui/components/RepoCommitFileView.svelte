<script lang="ts">
  import type { BlameLine, RepoBlob, RepoTreeEntry } from '../lib/types'
  import { postJSON } from '../lib/api'
  import { decodeInline, fmtBytes, prettyText, shortSha } from './RepoCommitUtils'

  interface Props {
    repo: string
    activeBranch: string
    pinnedRef: string | null
    selectedFile: RepoTreeEntry
    blob: RepoBlob | undefined
    busy?: boolean
    onBack: () => void
    onFileDeleted: () => Promise<void>
    onFileSaved: (path: string) => Promise<void>
    onCopy: (label: string, val: string) => void
    onToast: (msg: string, isErr?: boolean) => void
  }

  let {
    repo,
    activeBranch,
    pinnedRef,
    selectedFile,
    blob,
    busy = $bindable(false),
    onBack,
    onFileDeleted,
    onFileSaved,
    onCopy,
    onToast
  }: Props = $props()

  let editingFile = $state(false)
  let editContent = $state('')
  let editMessage = $state('')
  let blameOpen = $state(false)
  let blameLoading = $state(false)
  let blameLines = $state<BlameLine[]>([])

  $effect(() => {
    void selectedFile.path
    void pinnedRef
    editingFile = false
    blameOpen = false
    blameLines = []
  })

  const isImg = $derived(Boolean(selectedFile.mediaType?.startsWith('image/') && blob?.inline && !selectedFile.encrypted))

  function startEditFile() {
    if (!selectedFile || pinnedRef) return
    editContent = blob?.inline ? decodeInline(blob.inline) : ''
    editMessage = `Update ${selectedFile.path}`
    blameOpen = false
    editingFile = true
  }

  async function saveEditedFile() {
    if (!repo || !selectedFile || busy) return
    busy = true
    try {
      const targetPath = selectedFile.path
      const res = await postJSON<{ anchorTxid?: string }>(`/v1/repos/${encodeURIComponent(repo)}/commit`, {
        branch: activeBranch,
        path: targetPath,
        mediaType: selectedFile.mediaType || 'text/plain',
        text: editContent,
        commitMessage: editMessage.trim() || `Update ${targetPath}`,
        autoAnchor: true
      })
      if (!res.ok) {
        onToast(res.data.error ?? `Save failed (${res.status})`, true)
        return
      }
      editingFile = false
      onToast(res.data.anchorTxid ? `Saved & anchored ${targetPath} ✓` : `Saved ${targetPath} ✓`)
      await onFileSaved(targetPath)
    } finally {
      busy = false
    }
  }

  async function deleteSelectedFile() {
    if (!repo || !selectedFile || pinnedRef || busy) return
    busy = true
    try {
      const targetPath = selectedFile.path
      const r = await fetch(`/v1/repos/${encodeURIComponent(repo)}/file`, {
        method: 'DELETE',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ branch: activeBranch, path: targetPath, autoAnchor: true })
      })
      const d = (await r.json().catch(() => ({}))) as { error?: string }
      if (!r.ok) {
        onToast(d.error ?? `Delete failed (${r.status})`, true)
        return
      }
      editingFile = false
      blameOpen = false
      onToast(`Committed git rm ${targetPath} ✓`)
      await onFileDeleted()
    } finally {
      busy = false
    }
  }

  async function toggleBlame() {
    if (!repo || !selectedFile) return
    if (blameOpen) {
      blameOpen = false
      return
    }
    editingFile = false
    blameOpen = true
    blameLoading = true
    blameLines = []
    try {
      const qs = new URLSearchParams({ path: selectedFile.path })
      if (pinnedRef) qs.set('commit', pinnedRef)
      else qs.set('branch', activeBranch)
      const r = await fetch(`/v1/repos/${encodeURIComponent(repo)}/blame?${qs.toString()}`)
      const d = (await r.json().catch(() => ({}))) as { lines?: BlameLine[]; error?: string }
      if (!r.ok) {
        onToast(d.error ?? 'Blame failed', true)
        blameOpen = false
        return
      }
      blameLines = d.lines ?? []
    } catch (e) {
      onToast(String(e), true)
      blameOpen = false
    } finally {
      blameLoading = false
    }
  }
</script>

<div class="file-view">
  <div class="fv-head">
    <button
      class="fv-back"
      onclick={() => {
        editingFile = false
        blameOpen = false
        onBack()
      }}
    >
      ← Files
    </button>
    <span class="fv-path" title={selectedFile.path}>{selectedFile.path}</span>
    <span class="fv-size">{fmtBytes(selectedFile.size)}</span>
    <div class="fv-acts">
      {#if !selectedFile.encrypted && !isImg && blob?.inline && !pinnedRef}
        <button class="txt-btn" class:lit={editingFile} onclick={() => (editingFile ? (editingFile = false) : startEditFile())}>
          Edit
        </button>
      {/if}
      {#if !selectedFile.encrypted && !isImg}
        <button class="txt-btn" class:lit={blameOpen} onclick={() => void toggleBlame()}>
          Blame
        </button>
      {/if}
      {#if !pinnedRef}
        <button class="txt-btn del" onclick={() => void deleteSelectedFile()} disabled={busy}>
          Delete
        </button>
      {/if}
      {#if blob?.inline && !selectedFile.encrypted && !isImg}
        <button class="txt-btn" onclick={() => onCopy('file content', decodeInline(blob.inline))}>Copy</button>
      {:else if selectedFile.chirpUrl}
        <button class="txt-btn" onclick={() => onCopy('CHIRP URL', selectedFile?.chirpUrl ?? '')}>Copy chirp://</button>
      {/if}
    </div>
  </div>

  {#if editingFile}
    <form
      class="edit-wrap"
      onsubmit={(e) => {
        e.preventDefault()
        void saveEditedFile()
      }}
    >
      <textarea bind:value={editContent} class="edit-area mono" rows="16"></textarea>
      <div class="cp-foot">
        <input bind:value={editMessage} placeholder="Commit message…" class="cp-msg" />
        <button type="button" class="txt-btn" onclick={() => (editingFile = false)}>Cancel</button>
        <button type="submit" class="cp-submit" disabled={busy}>
          {busy ? 'Saving…' : 'Commit Change'}
        </button>
      </div>
    </form>
  {:else if blameOpen}
    {#if blameLoading}
      <div class="empty">Computing cryptographic UTXO blame…</div>
    {:else if blameLines.length === 0}
      <div class="empty">No blame lines available.</div>
    {:else}
      <div class="blame-table">
        {#each blameLines as bl (bl.lineNo)}
          <div class="bl-row">
            <button
              class="bl-gutter"
              onclick={() => onCopy('line provenance', `${bl.commitHash}${bl.anchorTxid ? ` · ${bl.anchorTxid}:0` : ''}`)}
              title={`Commit ${bl.commitHash}${bl.modelId ? ` · ${bl.modelId}` : ''}${bl.anchorTxid ? ` · UTXO ${bl.anchorTxid}:0` : ''}`}
            >
              <span class="bl-sha">{shortSha(bl.commitHash, 7)}</span>
              {#if bl.anchorTxid}
                <span class="bl-utxo">●</span>
              {/if}
              {#if bl.modelId}
                <span class="bl-mod">{bl.modelId.slice(0, 10)}</span>
              {/if}
            </button>
            <span class="bl-no">{bl.lineNo}</span>
            <span class="bl-txt">{bl.text}</span>
          </div>
        {/each}
      </div>
    {/if}
  {:else if selectedFile.encrypted}
    <pre class="fv-code muted">Encrypted under BRC-369 Borg group {blob?.borgGroup ?? ''} (epoch {blob?.borgEpoch ?? 0})</pre>
  {:else if isImg && blob?.inline}
    <div class="fv-img-wrap">
      <img src={`data:${selectedFile.mediaType};base64,${blob.inline}`} alt={selectedFile.path} />
    </div>
  {:else if blob?.inline}
    <pre class="fv-code">{prettyText(selectedFile, decodeInline(blob.inline))}</pre>
  {:else if selectedFile.chirpUrl}
    <pre class="fv-code muted">Volumetric BRC-167 CHIRP object ({fmtBytes(selectedFile.size)})
{selectedFile.chirpUrl}</pre>
  {:else}
    <pre class="fv-code muted">Binary object ({fmtBytes(selectedFile.size)})</pre>
  {/if}
</div>

<style>
  .empty {
    padding: 40px 18px;
    text-align: center;
    color: #6c645c;
    font-size: 13px;
  }

  /* File Content Reader, Editor & Blame Gutter */
  .file-view {
    display: flex;
    flex-direction: column;
    height: 100%;
  }
  .fv-head {
    display: flex;
    align-items: center;
    gap: 8px;
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
    flex-shrink: 0;
  }
  .fv-back:hover {
    color: #f4f0eb;
  }
  .fv-path {
    flex: 1;
    min-width: 0;
    font: 500 12.5px ui-monospace, monospace;
    color: #f4f0eb;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .fv-size {
    font: 400 11px ui-monospace, monospace;
    color: #635c55;
    flex-shrink: 0;
  }
  .fv-acts {
    display: flex;
    align-items: center;
    gap: 2px;
    flex-shrink: 0;
  }
  .fv-code {
    margin: 0;
    padding: 16px 18px;
    font: 400 12.5px/1.6 ui-monospace, SFMono-Regular, Menlo, monospace;
    color: #ded8d0;
    white-space: pre-wrap;
    word-break: break-word;
    flex: 1;
  }
  .fv-img-wrap {
    padding: 18px;
    display: flex;
    justify-content: center;
  }
  .fv-img-wrap img {
    max-width: 100%;
    max-height: 420px;
    border-radius: 6px;
    border: 1px solid #1e1b18;
  }
  .edit-wrap {
    padding: 14px 18px;
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .edit-area {
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
  .edit-area:focus,
  .cp-msg:focus {
    border-color: #5a4630;
  }
  .cp-foot {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .cp-msg {
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

  /* Cryptographic UTXO Blame Gutter */
  .blame-table {
    display: flex;
    flex-direction: column;
    font: 400 11.5px/1.65 ui-monospace, SFMono-Regular, Menlo, monospace;
  }
  .bl-row {
    display: flex;
    align-items: baseline;
    border-bottom: 1px solid #131110;
  }
  .bl-gutter {
    width: 144px;
    flex-shrink: 0;
    display: inline-flex;
    align-items: center;
    gap: 5px;
    padding: 2px 8px;
    border: 0;
    border-right: 1px solid #1c1916;
    background: #100e0d;
    color: #8f877f;
    font: inherit;
    font-size: 10.5px;
    cursor: pointer;
    text-align: left;
    overflow: hidden;
  }
  .bl-gutter:hover {
    background: #171412;
    color: #ece7e1;
  }
  .bl-sha {
    color: #9e968d;
  }
  .bl-utxo {
    color: #4cc26b;
    font-size: 8px;
  }
  .bl-mod {
    color: #635c55;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .bl-no {
    width: 32px;
    flex-shrink: 0;
    text-align: right;
    padding: 2px 8px 2px 4px;
    color: #4e4842;
    user-select: none;
  }
  .bl-txt {
    flex: 1;
    min-width: 0;
    padding: 2px 10px 2px 4px;
    color: #ded8d0;
    white-space: pre-wrap;
    word-break: break-word;
  }

  .muted {
    color: #787068;
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
  .txt-btn:hover,
  .txt-btn.lit {
    background: #1c1916;
    color: #f4f0eb;
  }
  .txt-btn.del:hover {
    color: #e5534b;
  }
</style>
