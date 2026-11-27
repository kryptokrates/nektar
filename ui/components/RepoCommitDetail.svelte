<script lang="ts">
  import type { RepoBlob, RepoCommit, UnifiedFileDiff } from '../lib/types'
  import { postJSON } from '../lib/api'
  import { decodeInline, fmtBytes, shortSha, type DiffChange } from './RepoCommitUtils'

  interface Props {
    repo: string
    commit: RepoCommit
    loading: boolean
    patch: UnifiedFileDiff[]
    changes: DiffChange[]
    blobs: Record<string, RepoBlob>
    busy?: boolean
    onBrowseAtRef: (ref: string) => void
    onRevertCommit: (c: RepoCommit) => void
    onCopy: (label: string, val: string) => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
  }

  let {
    repo,
    commit,
    loading,
    patch,
    changes,
    blobs,
    busy = $bindable(false),
    onBrowseAtRef,
    onRevertCommit,
    onCopy,
    onToast
  }: Props = $props()

  let proofOpen = $state(false)
  let proofFields = $state<Record<string, boolean>>({
    source: true,
    modelId: true,
    time: true,
    path: true,
    text: false
  })

  async function copySelectiveProof() {
    if (!repo || busy) return
    const selected = Object.entries(proofFields)
      .filter(([, v]) => v)
      .map(([k]) => k)
    if (selected.length === 0) {
      onToast('Select at least one field to disclose', true)
      return
    }
    busy = true
    try {
      const res = await postJSON<Record<string, unknown>>(`/v1/repos/${encodeURIComponent(repo)}/prove-fields`, {
        commit: commit.commitHash,
        seq: commit.seq || undefined,
        fields: selected
      })
      if (!res.ok) {
        onToast(res.data.error ?? `Proof failed (${res.status})`, true)
        return
      }
      await onCopy(`L1 MPE proof (${selected.join(', ')})`, JSON.stringify(res.data, null, 2))
      proofOpen = false
    } finally {
      busy = false
    }
  }
</script>

<!-- Click-to-Expand `git show` Inspector (Myers unified diff + actions + proof table) -->
<div class="commit-show">
  {#if loading}
    <div class="cs-loading">Loading diff…</div>
  {:else if patch.length > 0}
    <div class="diff-list">
      {#each patch as pf}
        <div class="diff-file">
          <div class="df-head">
            <span class="df-kind" class:add={pf.status === 'added'} class:del={pf.status === 'deleted'}>
              {pf.status === 'added' ? 'A' : pf.status === 'deleted' ? 'D' : 'M'}
            </span>
            <span class="df-path">{pf.path}</span>
            {#if pf.additions > 0}
              <span class="df-stat add">+{pf.additions}</span>
            {/if}
            {#if pf.deletions > 0}
              <span class="df-stat del">-{pf.deletions}</span>
            {/if}
          </div>
          {#if pf.binary}
            <div class="df-body muted">Binary object changed</div>
          {:else if pf.lines && pf.lines.length > 0}
            <pre class="df-code">{#each pf.lines.slice(0, 60) as ln}<span class="df-line" class:plus={ln.op === '+'} class:minus={ln.op === '-'}>{ln.op === '+' ? '+ ' : ln.op === '-' ? '- ' : '  '}{ln.text}</span>
{/each}{#if pf.lines.length > 60}<span class="df-line muted">… {pf.lines.length - 60} more lines</span>{/if}</pre>
          {/if}
        </div>
      {/each}
    </div>
  {:else if changes.length > 0}
    <div class="diff-list">
      {#each changes as ch}
        {@const b = ch.newBlobHash ? blobs[ch.newBlobHash] : undefined}
        {@const txt = b?.inline ? decodeInline(b.inline) : ''}
        <div class="diff-file">
          <div class="df-head">
            <span class="df-kind" class:add={(ch.status || ch.kind) === 'added'} class:del={(ch.status || ch.kind) === 'deleted'}>
              {(ch.status || ch.kind) === 'added' ? 'A' : (ch.status || ch.kind) === 'deleted' ? 'D' : 'M'}
            </span>
            <span class="df-path">{ch.path}</span>
            {#if b?.size}
              <span class="df-size">{fmtBytes(b.size)}</span>
            {/if}
          </div>
          {#if b?.encrypted}
            <div class="df-body muted">Encrypted under BRC-369 ({b.borgGroup || 'group'})</div>
          {:else if txt}
            <pre class="df-code">{#each txt.split('\n').slice(0, 12) as line}<span class="df-line" class:plus={(ch.status || ch.kind) !== 'deleted'}>{(ch.status || ch.kind) === 'deleted' ? '- ' : '+ '}{line}</span>
{/each}</pre>
          {/if}
        </div>
      {/each}
    </div>
  {/if}

  <!-- Quiet monochrome commit actions -->
  <div class="cs-acts">
    <button class="txt-btn" onclick={() => onBrowseAtRef(commit.commitHash)}>
      Browse @{shortSha(commit.commitHash, 7)}
    </button>
    <button class="txt-btn" onclick={() => onRevertCommit(commit)} disabled={busy}>
      Revert
    </button>
    <button
      class="txt-btn"
      class:lit={proofOpen}
      onclick={() => (proofOpen = !proofOpen)}
    >
      Share Proof
    </button>
  </div>

  {#if proofOpen}
    <div class="proof-bar">
      <div class="pb-fields">
        {#each Object.keys(proofFields) as fk}
          <label class="pb-chk">
            <input type="checkbox" bind:checked={proofFields[fk]} />
            <span>{fk}</span>
          </label>
        {/each}
      </div>
      <button class="cp-submit" onclick={() => void copySelectiveProof()} disabled={busy}>
        Copy L1 MPE Proof
      </button>
    </div>
  {/if}

  <!-- Minimal key-value provenance table (zero pills) -->
  <dl class="kv">
    <dt>Commit</dt>
    <dd>
      <button class="kv-copy" onclick={() => void onCopy('commit hash', commit.commitHash)}>{commit.commitHash}</button>
    </dd>

    <dt>UTXO</dt>
    <dd>
      {#if commit.anchorTxid}
        <button class="kv-copy ok" onclick={() => void onCopy('UTXO outpoint', `${commit.anchorTxid}:0`)}>
          {shortSha(commit.anchorTxid, 12)}:0
        </button>
        <span class="kv-note">batch #{commit.anchorN}</span>
      {:else}
        <span class="kv-muted">Unanchored (local DAG)</span>
      {/if}
    </dd>

    {#if commit.parents && commit.parents.length > 0}
      <dt>{commit.parents.length > 1 ? 'Parents' : 'Parent'}</dt>
      <dd class="kv-parents">
        {#each commit.parents as p}
          <button class="kv-copy" onclick={() => void onCopy('parent commit', p.commitHash)}>
            {shortSha(p.commitHash, 7)}{p.repoGenesis ? ` (fork ${shortSha(p.repoGenesis, 6)})` : ''}
          </button>
        {/each}
      </dd>
    {/if}
  </dl>
</div>

<style>
  /* Expanded `git show` Panel */
  .commit-show {
    padding: 4px 18px 14px 42px;
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .cs-loading {
    font-size: 11.5px;
    color: #6b635b;
    padding: 4px 0;
  }
  .cs-acts {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .proof-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    padding: 8px 10px;
    border-radius: 6px;
    border: 1px solid #24201c;
    background: #0a0908;
    flex-wrap: wrap;
  }
  .pb-fields {
    display: flex;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
  }
  .pb-chk {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    font: 500 11.5px ui-monospace, monospace;
    color: #b7afa6;
    cursor: pointer;
  }

  .diff-list {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .diff-file {
    border: 1px solid #1e1b18;
    border-radius: 6px;
    overflow: hidden;
    background: #090807;
  }
  .df-head {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 6px 10px;
    border-bottom: 1px solid #191614;
    background: #12100e;
    font: 500 11.5px ui-monospace, monospace;
  }
  .df-kind {
    font-weight: 700;
    color: #e4a149;
  }
  .df-kind.add {
    color: #4cc26b;
  }
  .df-kind.del {
    color: #e5534b;
  }
  .df-path {
    flex: 1;
    min-width: 0;
    color: #d6cfc7;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .df-stat {
    font-size: 11px;
    font-weight: 600;
  }
  .df-stat.add {
    color: #4cc26b;
  }
  .df-stat.del {
    color: #e5534b;
  }
  .df-size {
    color: #635c55;
    font-size: 11px;
  }
  .df-code {
    margin: 0;
    padding: 6px 0;
    font: 400 11.5px/1.55 ui-monospace, SFMono-Regular, Menlo, monospace;
    color: #9e968d;
    overflow-x: auto;
    white-space: pre-wrap;
    word-break: break-word;
  }
  .df-line {
    display: block;
    padding: 0 10px;
  }
  .df-line.plus {
    background: rgba(76, 194, 107, 0.08);
    color: #a9d9b6;
  }
  .df-line.minus {
    background: rgba(229, 83, 75, 0.09);
    color: #f0a19c;
  }
  .df-body {
    padding: 8px 10px;
    font-size: 11.5px;
  }

  .kv {
    display: grid;
    grid-template-columns: 56px 1fr;
    row-gap: 4px;
    column-gap: 10px;
    margin: 0;
    padding-top: 2px;
    font-size: 11.5px;
  }
  .kv dt {
    color: #5e5750;
    font-weight: 500;
  }
  .kv dd {
    margin: 0;
    min-width: 0;
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
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
    max-width: 100%;
  }
  .kv-copy:hover {
    color: #f4f0eb;
  }
  .kv-copy.ok {
    color: #68b87d;
  }
  .kv-note {
    color: #5e5750;
    font-size: 11px;
  }
  .kv-muted {
    color: #6b635b;
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
</style>
