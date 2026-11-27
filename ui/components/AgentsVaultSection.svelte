<script lang="ts">
  import type { RepoSummary } from '../lib/types'
  import { postJSON } from '../lib/api'
  import { fileToBase64, prettyTopic, type HiveTopic, type VaultRow } from './AgentsModel'

  interface Props {
    rows: VaultRow[]
    selectedTopic: string
    activeTopicMeta: HiveTopic | null
    activeRepoSummary: RepoSummary | null
    firstRepoName: string | null
    treasuryFunded: boolean
    onClearTopic: () => void
    onToggleListing: (shard: string, active: boolean) => void
    onInspectRepo: (repo: string) => void
    onInspectCapsuleFile: (file: File) => void
    onOpenRow: (row: VaultRow) => void
    onRefresh: () => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
  }

  let {
    rows,
    selectedTopic,
    activeTopicMeta,
    activeRepoSummary,
    firstRepoName,
    treasuryFunded,
    onClearTopic,
    onToggleListing,
    onInspectRepo,
    onInspectCapsuleFile,
    onOpenRow,
    onRefresh,
    onToast
  }: Props = $props()

  let addOpen = $state(false)
  let noteText = $state('')
  let attachFile = $state<File | null>(null)
  let fileInputEl = $state<HTMLInputElement | null>(null)
  let capsuleInputEl = $state<HTMLInputElement | null>(null)
  let busy = $state(false)
  let addInputEl = $state<HTMLInputElement | null>(null)

  function toggleAdd() {
    addOpen = !addOpen
    if (addOpen) setTimeout(() => addInputEl?.focus(), 20)
  }

  async function saveMemory() {
    const body = noteText.trim()
    if ((!body && !attachFile) || busy) return
    busy = true
    try {
      let targetShard = selectedTopic !== '__all__' ? selectedTopic : 'notes'
      if (selectedTopic === '__all__' && body) {
        try {
          const cl = await fetch(`/v1/topics/cluster?hint=${encodeURIComponent(body.slice(0, 180))}`)
          if (cl.ok) {
            const j = (await cl.json()) as { cluster?: string }
            if (j.cluster && j.cluster !== 'general') targetShard = j.cluster
          }
        } catch {}
      }
      if (attachFile) {
        const b64 = await fileToBase64(attachFile)
        const res = await postJSON<{ anchorTxid?: string; chirpUrl?: string }>(
          `/v1/repos/${encodeURIComponent(targetShard)}/commit`,
          {
            path: `files/${attachFile.name}`,
            mediaType: attachFile.type || undefined,
            text: body,
            blobBase64: b64,
            commitMessage: body || `Attach ${attachFile.name}`,
            autoAnchor: treasuryFunded
          }
        )
        if (!res.ok) {
          onToast(res.data.error ?? `Commit failed (${res.status})`, true)
          return
        }
        noteText = ''
        attachFile = null
        addOpen = false
        onToast(`Committed ${res.data.chirpUrl ? 'CHIRP file ' : ''}to ${prettyTopic(targetShard)} ✓`)
        await onRefresh()
        return
      }
      const res = await postJSON<{ anchorTxid?: string }>('/v1/remember', {
        shard: targetShard,
        text: body,
        autoAnchor: treasuryFunded
      })
      if (!res.ok) {
        onToast(res.data.error ?? `Save failed (${res.status})`, true)
        return
      }
      noteText = ''
      addOpen = false
      onToast(
        res.data.anchorTxid
          ? `Committed & anchored to ${prettyTopic(targetShard)} ✓`
          : `Committed to ${prettyTopic(targetShard)} ✓`
      )
      await onRefresh()
    } catch (e) {
      onToast(String(e), true)
    } finally {
      busy = false
    }
  }
</script>

<section class="vault">
  <div class="vault-head">
    <div class="vh-left">
      <span class="vh-title">Memory Repos</span>
      <span class="vh-cnt">{rows.length}</span>
      {#if activeTopicMeta}
        <button class="filter-chip" onclick={onClearTopic} title="Clear repo filter">
          {activeTopicMeta.label} <span>×</span>
        </button>
        {#if activeTopicMeta.listed}
          <button
            class="list-toggle"
            onclick={() => onToggleListing(activeTopicMeta.shard, !activeTopicMeta.active)}
          >
            {activeTopicMeta.active ? 'Pause selling' : 'Resume selling'}
          </button>
        {/if}
      {/if}
    </div>

    <div class="vh-acts">
      <input
        bind:this={capsuleInputEl}
        type="file"
        accept=".nkt,application/octet-stream"
        class="hidden-file"
        onchange={(e) => {
          const inp = e.currentTarget as HTMLInputElement
          const f = inp.files?.[0] ?? null
          inp.value = ''
          if (f) onInspectCapsuleFile(f)
        }}
      />
      <button
        class="add-btn"
        onclick={() => capsuleInputEl?.click()}
        title="Inspect & verify a portable .nkt Memory Capsule offline"
      >
        ↑ .nkt
      </button>
      {#if selectedTopic !== '__all__' || firstRepoName}
        <button
          class="dag-btn"
          onclick={() => {
            const target = selectedTopic !== '__all__' ? selectedTopic : firstRepoName
            if (target) onInspectRepo(target)
          }}
          title="Inspect Git commit history, files, and .nkt export"
        >
          ⎇ History
        </button>
      {/if}
      <button class="add-btn" class:open={addOpen} onclick={toggleAdd}>
        {addOpen ? 'Cancel' : '+ Commit'}
      </button>
    </div>
  </div>

  {#if activeRepoSummary}
    <div class="repo-lineage">
      <div class="rl-left">
        <span class="rl-br">{activeRepoSummary.branch || 'main'}</span>
        <span class="rl-stat">{activeRepoSummary.commitCount} {activeRepoSummary.commitCount === 1 ? 'commit' : 'commits'}</span>
        <span class="rl-stat">{activeRepoSummary.fileCount} {activeRepoSummary.fileCount === 1 ? 'file' : 'files'}</span>
        {#if activeRepoSummary.headAnchorTxid}
          <span class="rl-utxo">Anchored</span>
        {:else}
          <span class="rl-pending">Local</span>
        {/if}
      </div>
      <div class="rl-acts">
        <button class="rl-link" onclick={() => onInspectRepo(activeRepoSummary.repo)}>Open repo →</button>
      </div>
    </div>
  {/if}

  {#if addOpen}
    <div class="add-row">
      <input
        bind:this={addInputEl}
        bind:value={noteText}
        placeholder={attachFile ? `Commit message for ${attachFile.name}…` : 'Commit a fact, rule, or note to your Memory Repo…'}
        disabled={busy}
        onkeydown={(e) => {
          if (e.key === 'Enter' && !e.shiftKey) {
            e.preventDefault()
            void saveMemory()
          } else if (e.key === 'Escape') {
            addOpen = false
          }
        }}
      />
      <input
        bind:this={fileInputEl}
        type="file"
        class="hidden-file"
        onchange={(e) => {
          const f = (e.currentTarget as HTMLInputElement).files?.[0] ?? null
          attachFile = f
        }}
      />
      <button
        class="attach-btn"
        class:has={Boolean(attachFile)}
        onclick={() => (attachFile ? (attachFile = null) : fileInputEl?.click())}
        title={attachFile ? 'Remove attached file' : 'Attach a volumetric file (PDF, JSON, MD, etc.) via BRC-167 CHIRP'}
      >
        {attachFile ? `📎 ${attachFile.name.slice(0, 16)} ×` : '📎 File'}
      </button>
      <button class="save-btn" onclick={saveMemory} disabled={busy || (!noteText.trim() && !attachFile)}>
        {busy ? 'Committing…' : 'Commit'}
      </button>
    </div>
  {/if}

  {#if rows.length === 0}
    <div class="empty">Nothing here yet.</div>
  {:else}
    <div class="list">
      {#each rows as row (row.id)}
        <button class="row" onclick={() => onOpenRow(row)}>
          <span class="rdot" class:ok={row.dot === 'ok'} class:warn={row.dot === 'warn'} class:bad={row.dot === 'bad'}></span>
          <span class="rtxt">{row.text}</span>
          <span class="rmeta">
            {#if row.sats}
              <b class="rsats" class:ok={row.satsTone === 'ok'} class:amber={row.satsTone === 'amber'}>{row.sats}</b>
            {/if}
            {#if row.kind === 'memory' && row.raw.chirpUrl}
              <span class="rchirp">CHIRP</span>
            {/if}
            <span class="rtopic">{row.topic}</span>
            <span class="rtime">{row.time}</span>
          </span>
        </button>
      {/each}
    </div>
  {/if}
</section>

<style>
  .vault {
    display: flex;
    flex-direction: column;
    flex: 1;
  }
  .vault-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.75rem;
    padding: 0.75rem 1.25rem;
    border-bottom: 1px solid var(--line);
  }
  .vh-left { display: flex; align-items: center; gap: 0.55rem; flex-wrap: wrap; }
  .vh-title {
    font-size: 0.72rem; font-weight: 800; letter-spacing: 0.09em;
    text-transform: uppercase; color: var(--mut2);
  }
  .vh-cnt {
    font-family: ui-monospace, monospace; font-size: 0.74rem; color: var(--mut);
    font-variant-numeric: tabular-nums;
  }
  .filter-chip {
    display: inline-flex; align-items: center; gap: 0.32rem;
    background: rgb(247 153 27 / 12%); color: var(--amber);
    border: 1px solid rgb(247 153 27 / 32%); border-radius: 999px;
    padding: 0.14rem 0.58rem; font: inherit; font-size: 0.74rem; font-weight: 700; cursor: pointer;
  }
  .filter-chip span { opacity: 0.75; }
  .list-toggle {
    background: none; border: 1px solid var(--line); border-radius: 999px;
    color: var(--mut); padding: 0.14rem 0.58rem; font: inherit; font-size: 0.72rem;
    font-weight: 600; cursor: pointer;
  }
  .list-toggle:hover { border-color: var(--amber); color: var(--amber); }

  .vh-acts { display: flex; align-items: center; gap: 0.45rem; }
  .dag-btn {
    background: rgb(76 194 107 / 9%); border: 1px solid rgb(76 194 107 / 28%);
    border-radius: 999px; color: var(--ok); padding: 0.24rem 0.72rem;
    font: inherit; font-size: 0.74rem; font-weight: 700; cursor: pointer;
    transition: border-color 0.14s, background 0.14s;
  }
  .dag-btn:hover { border-color: var(--ok); background: rgb(76 194 107 / 16%); }
  .add-btn {
    background: none; border: 1px solid var(--line); border-radius: 999px;
    color: var(--mut); padding: 0.24rem 0.72rem; font: inherit; font-size: 0.75rem;
    font-weight: 700; cursor: pointer; transition: border-color 0.14s, color 0.14s;
  }
  .add-btn:hover, .add-btn.open { border-color: var(--line2); color: var(--ink); }

  .repo-lineage {
    display: flex; align-items: center; justify-content: space-between;
    gap: 0.75rem; flex-wrap: wrap;
    padding: 0.52rem 1.25rem;
    background: #0d0c0b;
    border-bottom: 1px solid var(--line);
    font-size: 0.74rem;
  }
  .rl-left, .rl-acts { display: flex; align-items: center; gap: 0.55rem; flex-wrap: wrap; }
  .rl-br {
    font-family: ui-monospace, monospace; font-weight: 700; color: var(--amber);
    background: rgb(247 153 27 / 10%); border: 1px solid rgb(247 153 27 / 24%);
    border-radius: 999px; padding: 0.08rem 0.52rem;
  }
  .rl-stat { color: var(--mut); font-weight: 600; }
  .rl-utxo {
    font-family: ui-monospace, monospace; font-size: 0.71rem; font-weight: 700;
    color: var(--ok); background: rgb(76 194 107 / 10%); border: 1px solid rgb(76 194 107 / 24%);
    border-radius: 999px; padding: 0.08rem 0.52rem;
  }
  .rl-pending {
    font-size: 0.71rem; color: var(--mut2);
    border: 1px solid var(--line); border-radius: 999px; padding: 0.08rem 0.48rem;
  }
  .rl-link {
    background: none; border: 1px solid var(--line); border-radius: 999px;
    color: var(--ink); padding: 0.14rem 0.62rem; font: inherit; font-size: 0.72rem;
    font-weight: 700; cursor: pointer;
  }
  .rl-link:hover { border-color: var(--line2); }

  .hidden-file { display: none; }
  .attach-btn {
    background: none; border: 1px solid var(--line); border-radius: 999px;
    color: var(--mut); padding: 0.28rem 0.7rem; font: inherit; font-size: 0.74rem;
    font-weight: 600; cursor: pointer; flex-shrink: 0;
  }
  .attach-btn:hover, .attach-btn.has { border-color: var(--ok); color: var(--ok); }
  .rchirp {
    font-family: ui-monospace, monospace; font-size: 0.66rem; font-weight: 800;
    color: var(--ok); background: rgb(76 194 107 / 10%); border: 1px solid rgb(76 194 107 / 24%);
    border-radius: 4px; padding: 0.04rem 0.34rem;
  }

  .add-row {
    display: flex; align-items: center; gap: 0.65rem;
    padding: 0.65rem 1.25rem;
    background: var(--surf);
    border-bottom: 1px solid var(--line);
  }
  .add-row input {
    flex: 1; min-width: 0; background: none; border: 0; color: var(--ink);
    font: inherit; font-size: 0.88rem; outline: none;
  }
  .add-row input::placeholder { color: var(--mut2); }
  .save-btn {
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.3rem 0.85rem; font: inherit; font-size: 0.76rem; font-weight: 800; cursor: pointer;
  }
  .save-btn:disabled { opacity: 0.38; cursor: default; }

  .list { display: flex; flex-direction: column; }
  .row {
    width: 100%;
    display: flex;
    align-items: center;
    gap: 0.75rem;
    padding: 0.78rem 1.25rem;
    background: none;
    border: 0;
    border-bottom: 1px solid var(--line);
    color: var(--ink);
    font: inherit;
    text-align: left;
    cursor: pointer;
    transition: background 0.12s;
  }
  .row:hover { background: var(--hover); }
  .rdot {
    width: 6.5px; height: 6.5px; border-radius: 50%;
    background: var(--mut2); flex-shrink: 0;
  }
  .rdot.ok { background: var(--ok); }
  .rdot.warn { background: var(--amber); }
  .rdot.bad { background: var(--bad); }
  .rtxt {
    flex: 1; min-width: 0;
    font-size: 0.88rem; color: var(--ink);
    white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
  }
  .rmeta {
    display: flex; align-items: center; gap: 0.65rem; flex-shrink: 0;
  }
  .rsats {
    font-family: ui-monospace, monospace; font-size: 0.78rem; font-weight: 800;
    font-variant-numeric: tabular-nums;
  }
  .rsats.ok { color: var(--ok); }
  .rsats.amber { color: var(--amber); }
  .rtopic {
    font-size: 0.74rem; font-weight: 600; color: var(--mut);
  }
  .rtime {
    font-family: ui-monospace, monospace; font-size: 0.72rem; color: var(--mut2);
    font-variant-numeric: tabular-nums; min-width: 26px; text-align: right;
  }
  .empty {
    padding: 3rem 1.25rem;
    text-align: center;
    color: var(--mut2);
    font-size: 0.86rem;
  }
</style>
