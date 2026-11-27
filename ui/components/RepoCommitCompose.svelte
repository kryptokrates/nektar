<script lang="ts">
  import { postJSON } from '../lib/api'
  import { fileToBase64 } from './RepoCommitUtils'

  interface Props {
    repo: string
    activeBranch: string
    busy?: boolean
    onClose: () => void
    onReloadRepo: () => Promise<void>
    onRefresh: () => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
  }

  let {
    repo,
    activeBranch,
    busy = $bindable(false),
    onClose,
    onReloadRepo,
    onRefresh,
    onToast
  }: Props = $props()

  let composePath = $state('')
  let composeBody = $state('')
  let composeMsg = $state('')
  let composeLockOpen = $state(false)
  let composeBorgGroup = $state('')
  let composeLevel = $state(0)
  let composeAutoAnchor = $state(true)
  let composeFile = $state<File | null>(null)
  let composeFileInput = $state<HTMLInputElement | null>(null)

  async function submitComposer() {
    if (!repo || busy) return
    const path = composePath.trim() || (composeFile ? `files/${composeFile.name}` : '')
    if (!path) {
      onToast('File path is required (e.g. notes/plan.md)', true)
      return
    }
    if (!composeBody.trim() && !composeFile) {
      onToast('Enter file content or attach a file', true)
      return
    }
    busy = true
    try {
      const payload: Record<string, unknown> = {
        branch: activeBranch,
        path,
        text: composeBody,
        commitMessage: composeMsg.trim() || `Add ${path}`,
        autoAnchor: composeAutoAnchor
      }
      if (composeFile) {
        payload.blobBase64 = await fileToBase64(composeFile)
        if (composeFile.type) payload.mediaType = composeFile.type
      }
      if (composeLockOpen && composeBorgGroup.trim()) {
        payload.borgGroup = composeBorgGroup.trim()
      }
      if (composeLockOpen && composeLevel > 0) {
        payload.level = Number(composeLevel)
      }
      const res = await postJSON<{ anchorTxid?: string }>(`/v1/repos/${encodeURIComponent(repo)}/commit`, payload)
      if (!res.ok) {
        onToast(res.data.error ?? `Commit failed (${res.status})`, true)
        return
      }
      composePath = ''
      composeBody = ''
      composeMsg = ''
      composeFile = null
      composeLockOpen = false
      composeBorgGroup = ''
      composeLevel = 0
      onClose()
      onToast(res.data.anchorTxid ? `Committed & anchored ${path} ✓` : `Committed ${path} ✓`)
      await onReloadRepo()
      await onRefresh()
    } finally {
      busy = false
    }
  }
</script>

<!-- Inline New / Edit File Composer (Progressive Disclosure via header `+`) -->
<form
  class="compose-panel"
  onsubmit={(e) => {
    e.preventDefault()
    void submitComposer()
  }}
>
  <div class="cp-row">
    <input bind:value={composePath} placeholder="notes/entry.md" class="cp-input mono" aria-label="File path" />
    <input
      bind:this={composeFileInput}
      type="file"
      class="hidden-file"
      onchange={(e) => {
        const f = (e.currentTarget as HTMLInputElement).files?.[0] ?? null
        composeFile = f
        if (f && !composePath.trim()) composePath = `files/${f.name}`
      }}
    />
    <button
      type="button"
      class="txt-btn"
      onclick={() => (composeFile ? (composeFile = null) : composeFileInput?.click())}
    >
      {composeFile ? `${composeFile.name.slice(0, 14)} ×` : 'Attach'}
    </button>
    <button
      type="button"
      class="txt-btn"
      class:lit={composeLockOpen}
      onclick={() => (composeLockOpen = !composeLockOpen)}
      title="Optional BRC-369 Borg encryption & clearance level"
    >
      {composeLockOpen ? '🔒' : '🔓'}
    </button>
  </div>

  {#if composeLockOpen}
    <div class="cp-row">
      <input bind:value={composeBorgGroup} placeholder="Borg group (optional)" class="cp-input mono" />
      <input type="number" min="0" max="10" bind:value={composeLevel} placeholder="Level" class="cp-lvl mono" />
    </div>
  {/if}

  <textarea
    bind:value={composeBody}
    rows="4"
    class="cp-area mono"
    placeholder={composeFile ? 'Optional text description…' : 'Write markdown, JSON, or facts…'}
  ></textarea>

  <div class="cp-foot">
    <input bind:value={composeMsg} placeholder="Commit message…" class="cp-msg" aria-label="Commit message" />
    <label class="cp-chk">
      <input type="checkbox" bind:checked={composeAutoAnchor} />
      <span>Anchor</span>
    </label>
    <button type="submit" class="cp-submit" disabled={busy}>
      {busy ? 'Committing…' : 'Commit'}
    </button>
  </div>
</form>

<style>
  .hidden-file {
    display: none;
  }

  .compose-panel {
    padding: 12px 18px;
    background: #0f0d0c;
    border-bottom: 1px solid #1e1b18;
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .cp-row {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .cp-input,
  .cp-msg,
  .cp-lvl {
    background: #090807;
    border: 1px solid #26211d;
    border-radius: 6px;
    padding: 6px 9px;
    color: #f4f0eb;
    font-size: 12px;
    outline: none;
  }
  .cp-input {
    flex: 1;
    min-width: 0;
  }
  .cp-lvl {
    width: 72px;
  }
  .cp-msg {
    flex: 1;
    min-width: 0;
  }
  .cp-input:focus,
  .cp-msg:focus,
  .cp-area:focus {
    border-color: #5a4630;
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
  .cp-foot {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .cp-chk {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    font-size: 11.5px;
    color: #8f877f;
    cursor: pointer;
    user-select: none;
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
  .txt-btn:hover,
  .txt-btn.lit {
    background: #1c1916;
    color: #f4f0eb;
  }
</style>
