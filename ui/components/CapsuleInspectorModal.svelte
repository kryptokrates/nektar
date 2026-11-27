<script lang="ts">
  import type { CapsuleInspectResult, RepoTreeEntry } from '../lib/types'
  import { operatorJSON } from '../lib/api'
  import { bytesToBase64, fmtBytes } from './CapsuleInspectorUtils'
  import CapsuleInspectorCommits from './CapsuleInspectorCommits.svelte'
  import CapsuleInspectorFiles from './CapsuleInspectorFiles.svelte'
  import CapsuleInspectorUtxo from './CapsuleInspectorUtxo.svelte'
  import CapsuleInspectorDiff from './CapsuleInspectorDiff.svelte'
  import CapsuleInspectorFooter from './CapsuleInspectorFooter.svelte'

  interface Props {
    file: File | null
    onClose: () => void
    onToast: (msg: string, isErr?: boolean) => void
    onRefresh: () => Promise<void>
    onImported?: (repoName: string) => void
  }

  let { file, onClose, onToast, onRefresh, onImported }: Props = $props()

  let loading = $state(false)
  let busy = $state(false)
  let errorMsg = $state<string | null>(null)
  let result = $state<CapsuleInspectResult | null>(null)
  let rawBase64 = $state('')
  let tab = $state<'commits' | 'files' | 'utxo' | 'diff'>('commits')
  let selectedFile = $state<RepoTreeEntry | null>(null)
  let expandedCommit = $state<string | null>(null)
  let forkMode = $state(false)
  let forkName = $state('')

  async function inspectFile(f: File) {
    loading = true
    errorMsg = null
    result = null
    selectedFile = null
    expandedCommit = null
    forkMode = false
    tab = 'commits'
    try {
      const buf = await f.arrayBuffer()
      const bytes = new Uint8Array(buf)
      rawBase64 = bytesToBase64(bytes)

      const r = await fetch('/v1/repos/inspect-capsule', {
        method: 'POST',
        headers: { 'content-type': 'application/octet-stream' },
        body: bytes
      })
      const data = (await r.json().catch(() => ({}))) as CapsuleInspectResult & { error?: string }
      if (!r.ok) {
        errorMsg = data.error ?? `Could not decode capsule (${r.status})`
        return
      }
      result = data
      forkName = `${data.repo || 'capsule'}-fork`
    } catch (e) {
      errorMsg = String(e)
    } finally {
      loading = false
    }
  }

  $effect(() => {
    if (file) {
      void inspectFile(file)
    } else {
      result = null
      errorMsg = null
      rawBase64 = ''
    }
  })

  async function importCapsule(targetRepo: string, mergeIntoMain: boolean) {
    if (!result || !result.verification?.ok || !rawBase64 || busy) return
    const cleanTarget = targetRepo.trim()
    if (!cleanTarget) {
      onToast('Target repo name is required', true)
      return
    }
    busy = true
    try {
      const r = await operatorJSON(
        'POST',
        '/v1/repos/import',
        {
          capsuleBase64: rawBase64,
          targetRepo: cleanTarget,
          targetBranch: result.branch || 'main',
          mergeIntoMain
        },
        `Import .nkt capsule into ${cleanTarget}`
      )
      if (!r) return
      if (!r.ok) {
        onToast(r.data.error ?? `Import failed (${r.status})`, true)
        return
      }
      onToast(mergeIntoMain ? `Imported & merged into ${cleanTarget} ✓` : `Forked into ${cleanTarget} ✓`)
      await onRefresh()
      onClose()
      onImported?.(cleanTarget)
    } finally {
      busy = false
    }
  }

  async function copyVal(label: string, val: string) {
    try {
      await navigator.clipboard.writeText(val)
      onToast(`Copied ${label}`)
    } catch {
      onToast(val)
    }
  }

  function onKey(e: KeyboardEvent) {
    if (file && e.key === 'Escape') {
      e.preventDefault()
      onClose()
    }
  }

  const entries = $derived(result?.tree?.entries ?? [])
  const commits = $derived(result?.commits ?? [])
  const anchors = $derived(result?.anchors ?? [])
  const diffEntries = $derived(result?.diffVsLocal ?? [])
  const verified = $derived(Boolean(result?.verification?.ok))
</script>

<svelte:window onkeydown={onKey} />

{#if file}
  <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
  <div class="overlay" onclick={onClose}>
    <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_noninteractive_element_interactions -->
    <div class="modal" role="dialog" aria-modal="true" aria-label="Memory Capsule Inspector" tabindex="-1" onclick={(e) => e.stopPropagation()}>
      <!-- 1. Calm Verification Header -->
      <header class="head">
        <div class="h-left">
          <div class="h-title-row">
            <span class="file-title" title={file.name}>{file.name}</span>
            <span class="file-size">{fmtBytes(file.size)}</span>
          </div>
          {#if loading}
            <div class="verdict muted">Verifying signatures, Merkle roots, and BSV Script VM…</div>
          {:else if errorMsg}
            <div class="verdict bad">
              <span class="dot"></span>
              <span>Invalid capsule — {errorMsg}</span>
            </div>
          {:else if result}
            <div class="verdict" class:ok={verified} class:bad={!verified}>
              <span class="dot"></span>
              {#if verified}
                <span>
                  Verified · {result.repo}/{result.branch} · {result.verification.anchorsVerified}
                  {result.verification.anchorsVerified === 1 ? 'UTXO' : 'UTXOs'} · Script VM OK
                </span>
              {:else}
                <span>Verification failed — {result.verification?.reason || 'broken UTXO lineage or signature'}</span>
              {/if}
            </div>
          {/if}
        </div>

        <button class="icon-close" onclick={onClose} aria-label="Close">
          <svg viewBox="0 0 16 16" fill="none" aria-hidden="true">
            <path d="M4 4l8 8M12 4l-8 8" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
          </svg>
        </button>
      </header>

      {#if result}
        <!-- 2. Hairline Tabs -->
        <nav class="tabs" aria-label="Capsule sections">
          <button class="tab" class:active={tab === 'commits'} onclick={() => { tab = 'commits'; selectedFile = null }}>
            Commits <span class="tab-n">{commits.length}</span>
          </button>
          <button class="tab" class:active={tab === 'files'} onclick={() => (tab = 'files')}>
            Files <span class="tab-n">{entries.length}</span>
          </button>
          <button class="tab" class:active={tab === 'utxo'} onclick={() => { tab = 'utxo'; selectedFile = null }}>
            UTXO Chain <span class="tab-n">{anchors.length}</span>
          </button>
          {#if result.localExists}
            <button class="tab" class:active={tab === 'diff'} onclick={() => { tab = 'diff'; selectedFile = null }}>
              Diff vs Local <span class="tab-n">{diffEntries.length}</span>
            </button>
          {/if}
        </nav>

        <!-- 3. Scrollable Content Body -->
        <div class="body">
          {#if tab === 'commits'}
            <CapsuleInspectorCommits
              {commits}
              bind:expandedCommit
              onCopy={(label, val) => void copyVal(label, val)}
            />
          {:else if tab === 'files'}
            <CapsuleInspectorFiles
              {entries}
              blobs={result.blobs}
              bind:selectedFile
            />
          {:else if tab === 'utxo'}
            <CapsuleInspectorUtxo
              genesisShardId={result.genesisShardId}
              ownerPub={result.ownerPub}
              {anchors}
              onCopy={(label, val) => void copyVal(label, val)}
            />
          {:else if tab === 'diff'}
            <CapsuleInspectorDiff repo={result.repo} {diffEntries} />
          {/if}
        </div>
      {/if}

      <CapsuleInspectorFooter
        hasResult={Boolean(result)}
        {loading}
        {verified}
        {busy}
        repo={result?.repo ?? ''}
        localExists={Boolean(result?.localExists)}
        bind:forkMode
        bind:forkName
        {onClose}
        onImport={(target, merge) => void importCapsule(target, merge)}
      />
    </div>
  </div>
{/if}

<style>
  .overlay {
    position: fixed;
    inset: 0;
    z-index: 95;
    background: rgba(0, 0, 0, 0.66);
    backdrop-filter: blur(4px);
    display: grid;
    place-items: center;
    padding: 1rem;
  }

  .modal {
    width: min(680px, 96vw);
    max-height: 86vh;
    background: #0c0b0a;
    border: 1px solid #24201c;
    border-radius: 12px;
    box-shadow: 0 24px 68px rgba(0, 0, 0, 0.78);
    display: flex;
    flex-direction: column;
    overflow: hidden;
    color: #ece7e1;
  }

  .head {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 12px;
    padding: 14px 18px;
    border-bottom: 1px solid #1a1714;
  }

  .h-left {
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .h-title-row {
    display: flex;
    align-items: baseline;
    gap: 8px;
  }

  .file-title {
    font: 600 14.5px ui-monospace, SFMono-Regular, Menlo, monospace;
    color: #f4f0eb;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .file-size {
    font: 400 11.5px ui-monospace, monospace;
    color: #6b635b;
    flex-shrink: 0;
  }

  .verdict {
    display: flex;
    align-items: center;
    gap: 7px;
    font: 500 12px system-ui, sans-serif;
    color: #9e968d;
  }
  .verdict.ok {
    color: #68b87d;
  }
  .verdict.bad {
    color: #e5534b;
  }
  .dot {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: currentColor;
    flex-shrink: 0;
  }

  .icon-close {
    width: 28px;
    height: 28px;
    display: grid;
    place-items: center;
    border: 0;
    border-radius: 6px;
    background: transparent;
    color: #7d756d;
    cursor: pointer;
    flex-shrink: 0;
  }
  .icon-close svg {
    width: 14px;
    height: 14px;
  }
  .icon-close:hover {
    background: #181512;
    color: #f4f0eb;
  }

  .tabs {
    display: flex;
    align-items: stretch;
    gap: 18px;
    height: 38px;
    padding: 0 18px;
    border-bottom: 1px solid #1a1714;
    flex-shrink: 0;
  }

  .tab {
    position: relative;
    display: inline-flex;
    align-items: center;
    gap: 6px;
    border: 0;
    padding: 0 2px;
    background: transparent;
    color: #7a726a;
    font: 500 12.5px system-ui, sans-serif;
    cursor: pointer;
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

  .body {
    flex: 1;
    overflow-y: auto;
    min-height: 220px;
  }

  .muted {
    color: #787068;
  }
</style>
