<script lang="ts">
  import { untrack } from 'svelte'
  import type { MergeConflict, RepoBlob, RepoCommit, RepoSummary, RepoTreeEntry } from '../lib/types'
  import { operatorFetch, operatorJSON } from '../lib/api'
  import { shortSha } from './RepoCommitUtils'
  import RepoCommitHeader from './RepoCommitHeader.svelte'
  import RepoCommitSubbar from './RepoCommitSubbar.svelte'
  import RepoCommitCompose from './RepoCommitCompose.svelte'
  import RepoCommitConflicts from './RepoCommitConflicts.svelte'
  import RepoCommitHistory from './RepoCommitHistory.svelte'
  import RepoCommitFiles from './RepoCommitFiles.svelte'

  interface Props {
    repo: string | null
    onClose: () => void
    onToast: (msg: string, isErr?: boolean) => void
    onRefresh: () => Promise<void>
    onSelectRepo?: (repoName: string) => void
  }

  let { repo, onClose, onToast, onRefresh, onSelectRepo }: Props = $props()

  let loading = $state(false)
  let summary = $state<RepoSummary | null>(null)
  let commits = $state<RepoCommit[]>([])
  let entries = $state<RepoTreeEntry[]>([])
  let blobs = $state<Record<string, RepoBlob>>({})
  let activeBranch = $state('main')
  let activeTab = $state<'history' | 'files'>('history')

  // Progressive disclosure states — everything noisy stays hidden until clicked
  let branchMenuOpen = $state(false)
  let composeOpen = $state(false)
  let utxoOpen = $state(false)
  let loadSeq = $state(0)

  // Epistemic time-travel state (`Browse @sha` or clicking a tag)
  let pinnedRef = $state<string | null>(null)
  let pinnedLabel = $state<string>('')
  let pinnedEntries = $state<RepoTreeEntry[]>([])
  let pinnedBlobs = $state<Record<string, RepoBlob>>({})

  let selectedFile = $state<RepoTreeEntry | null>(null)

  // 3-Way Merge Conflict Resolver (only shown on HTTP 409)
  let mergeConflicts = $state<MergeConflict[]>([])
  let conflictSourceBranch = $state('')
  let conflictResolutions = $state<Record<string, string>>({})

  let busy = $state(false)
  let checkoutDir = $state('')

  function resetSubViews() {
    selectedFile = null
    loadSeq += 1
    checkoutDir = ''
    mergeConflicts = []
    conflictResolutions = {}
  }

  async function loadRepo(target: string, branch = 'main') {
    loading = true
    resetSubViews()
    pinnedRef = null
    pinnedLabel = ''
    try {
      const r = await fetch(`/v1/repos/${encodeURIComponent(target)}?branch=${encodeURIComponent(branch)}`)
      if (!r.ok) {
        const err = await r.json().catch(() => ({}))
        onToast((err as { error?: string }).error ?? `Failed to load ${target}`, true)
        return
      }
      const data = (await r.json()) as {
        summary?: RepoSummary
        commits?: RepoCommit[]
        tree?: { entries?: RepoTreeEntry[] }
        blobs?: Record<string, RepoBlob>
      }
      summary = data.summary ?? null
      commits = data.commits ?? []
      entries = data.tree?.entries ?? []
      blobs = data.blobs ?? {}
    } catch (e) {
      onToast(String(e), true)
    } finally {
      loading = false
    }
  }

  $effect(() => {
    const target = repo
    if (target) {
      untrack(() => {
        activeBranch = 'main'
        activeTab = 'history'
        branchMenuOpen = false
        composeOpen = false
        utxoOpen = false
        void loadRepo(target, 'main')
      })
    }
  })

  async function browseAtRef(ref: string, label?: string) {
    if (!repo) return
    branchMenuOpen = false
    selectedFile = null
    try {
      const r = await fetch(`/v1/repos/${encodeURIComponent(repo)}/tree?commit=${encodeURIComponent(ref)}`)
      if (!r.ok) {
        const err = (await r.json().catch(() => ({}))) as { error?: string }
        onToast(err.error ?? `Could not inspect ${ref}`, true)
        return
      }
      const td = (await r.json()) as {
        commit?: RepoCommit
        tree?: { entries?: RepoTreeEntry[] }
        blobs?: Record<string, RepoBlob>
      }
      pinnedRef = td.commit?.commitHash ?? ref
      pinnedLabel = label ?? `@${shortSha(pinnedRef, 7)}`
      pinnedEntries = td.tree?.entries ?? []
      pinnedBlobs = td.blobs ?? {}
      activeTab = 'files'
    } catch (e) {
      onToast(String(e), true)
    }
  }

  function exitTimeTravel() {
    pinnedRef = null
    pinnedLabel = ''
    pinnedEntries = []
    pinnedBlobs = {}
    selectedFile = null
  }

  async function switchBranch(br: string) {
    if (!repo) return
    branchMenuOpen = false
    activeBranch = br
    await loadRepo(repo, br)
  }

  async function mergeIntoMain(resolutions?: Record<string, string>) {
    const src = resolutions ? conflictSourceBranch : activeBranch
    if (!repo || !src || src === 'main' || busy) return
    busy = true
    try {
      const r = await operatorJSON<{ conflicts?: MergeConflict[] }>(
        'POST',
        `/v1/repos/${encodeURIComponent(repo)}/merge`,
        {
          sourceBranch: src,
          targetBranch: 'main',
          resolutions: resolutions && Object.keys(resolutions).length > 0 ? resolutions : undefined,
          autoAnchor: true
        },
        `Merge ${src} into main`
      )
      if (!r) return
      if (r.ok) {
        onToast(`Merged ${src} → main ✓`)
        mergeConflicts = []
        conflictResolutions = {}
        activeBranch = 'main'
        branchMenuOpen = false
        await loadRepo(repo, 'main')
        await onRefresh()
      } else if (r.status === 409) {
        mergeConflicts = r.data.conflicts ?? []
        conflictSourceBranch = src
        const initRes: Record<string, string> = {}
        for (const cf of mergeConflicts) {
          initRes[cf.path] = cf.theirsText || cf.oursText || ''
        }
        conflictResolutions = initRes
        branchMenuOpen = false
        onToast(`3-way merge conflict on ${mergeConflicts.length} file(s) — resolve below`, true)
      } else {
        onToast(r.data.error ?? `Merge failed (${r.status})`, true)
      }
    } finally {
      busy = false
    }
  }

  async function revertCommit(c: RepoCommit) {
    if (!repo || busy) return
    busy = true
    try {
      const r = await operatorJSON(
        'POST',
        `/v1/repos/${encodeURIComponent(repo)}/revert`,
        { branch: activeBranch, commit: c.commitHash, autoAnchor: true },
        `Revert commit ${shortSha(c.commitHash, 7)}`
      )
      if (!r) return
      if (!r.ok) {
        onToast(r.data.error ?? `Revert failed (${r.status})`, true)
        return
      }
      onToast(`Committed revert of ${shortSha(c.commitHash, 7)} ✓`)
      await loadRepo(repo, activeBranch)
      await onRefresh()
    } finally {
      busy = false
    }
  }

  async function exportCapsule() {
    if (!repo || busy) return
    busy = true
    try {
      const url = `/v1/repos/${encodeURIComponent(repo)}/capsule?branch=${encodeURIComponent(activeBranch)}&download=1`
      const r = await operatorFetch('GET', url, undefined, `Export ${repo}.nkt capsule`)
      if (!r) return
      if (!r.ok) {
        const j = (await r.json().catch(() => ({}))) as { error?: string }
        onToast(j.error ?? `Export failed (${r.status})`, true)
        return
      }
      const blob = await r.blob()
      const objUrl = URL.createObjectURL(blob)
      try {
        const a = document.createElement('a')
        a.href = objUrl
        a.download = `${repo}.nkt`
        a.click()
      } finally {
        setTimeout(() => URL.revokeObjectURL(objUrl), 5000)
      }
      onToast(`Downloaded ${repo}.nkt ✓`)
    } finally {
      busy = false
    }
  }

  async function checkoutGit() {
    if (!repo || busy) return
    busy = true
    try {
      const r = await operatorJSON<{ dir?: string; fileCount?: number }>(
        'POST',
        `/v1/repos/${encodeURIComponent(repo)}/checkout`,
        { branch: activeBranch },
        'Checkout Git workspace'
      )
      if (r && r.ok) {
        checkoutDir = r.data.dir ?? ''
        onToast(`Checked out .git workspace (${r.data.fileCount ?? 0} files)`)
      } else if (r) {
        onToast(r.data.error ?? `Checkout failed (${r.status})`, true)
      }
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

  const branches = $derived(summary?.branches?.length ? summary.branches : ['main'])
  const tags = $derived(summary?.tags ?? [])
  const anchorCount = $derived(summary?.anchors?.length ?? 0)
  const settledCount = $derived(summary?.settledAnchors ?? 0)
  const allSettled = $derived(anchorCount > 0 && settledCount >= anchorCount)
  const activeEntries = $derived(pinnedRef ? pinnedEntries : entries)
  const activeBlobs = $derived(pinnedRef ? pinnedBlobs : blobs)
</script>

{#if repo}
  <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
  <div class="backdrop" onclick={onClose}></div>

  <aside class="drawer" aria-label="Memory Repository">
    <RepoCommitHeader
      {repo}
      {branches}
      {tags}
      {activeBranch}
      {pinnedRef}
      bind:branchMenuOpen
      bind:composeOpen
      bind:busy
      {checkoutDir}
      onSwitchBranch={(br) => void switchBranch(br)}
      onBrowseAtRef={(ref, label) => void browseAtRef(ref, label)}
      onMergeIntoMain={() => void mergeIntoMain()}
      onBranchCreated={async (br) => {
        activeBranch = br
        await loadRepo(repo, br)
        await onRefresh()
      }}
      onReloadRepo={() => loadRepo(repo, activeBranch)}
      {onRefresh}
      {onToast}
      {onSelectRepo}
      onCheckoutGit={() => void checkoutGit()}
      onExportCapsule={() => void exportCapsule()}
      {onClose}
      onCopy={(label, val) => void copyVal(label, val)}
    />

    <RepoCommitSubbar
      {repo}
      {activeTab}
      commitCount={commits.length}
      entryCount={activeEntries.length}
      {utxoOpen}
      {summary}
      {anchorCount}
      {allSettled}
      bind:busy
      onSelectTab={(t) => (activeTab = t)}
      onToggleUtxo={() => (utxoOpen = !utxoOpen)}
      onCopy={(label, val) => void copyVal(label, val)}
      onReloadRepo={() => loadRepo(repo, activeBranch)}
      {onRefresh}
      {onToast}
    />

    {#if composeOpen}
      <RepoCommitCompose
        {repo}
        {activeBranch}
        bind:busy
        onClose={() => (composeOpen = false)}
        onReloadRepo={() => loadRepo(repo, activeBranch)}
        {onRefresh}
        {onToast}
      />
    {/if}

    {#if mergeConflicts.length > 0}
      <RepoCommitConflicts
        sourceBranch={conflictSourceBranch}
        conflicts={mergeConflicts}
        bind:resolutions={conflictResolutions}
        {busy}
        onDismiss={() => (mergeConflicts = [])}
        onComplete={(res) => void mergeIntoMain(res)}
      />
    {/if}

    <!-- 3. Main Content Body -->
    <div class="body">
      {#if loading}
        <div class="empty">Loading…</div>
      {:else if activeTab === 'history'}
        <RepoCommitHistory
          {repo}
          {activeBranch}
          {commits}
          {loadSeq}
          bind:busy
          onBrowseAtRef={(ref) => void browseAtRef(ref)}
          onRevertCommit={(c) => void revertCommit(c)}
          onCopy={copyVal}
          {onToast}
        />
      {:else}
        <RepoCommitFiles
          {repo}
          {activeBranch}
          {pinnedRef}
          {pinnedLabel}
          entries={activeEntries}
          blobs={activeBlobs}
          bind:selectedFile
          bind:busy
          onExitTimeTravel={exitTimeTravel}
          onFileDeleted={async () => {
            selectedFile = null
            await loadRepo(repo, activeBranch)
            await onRefresh()
          }}
          onFileSaved={async (targetPath) => {
            await loadRepo(repo, activeBranch)
            selectedFile = entries.find((e) => e.path === targetPath) ?? null
            await onRefresh()
          }}
          onCopy={(label, val) => void copyVal(label, val)}
          {onToast}
        />
      {/if}
    </div>
  </aside>
{/if}

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    background: rgba(0, 0, 0, 0.52);
    backdrop-filter: blur(2px);
    z-index: 80;
  }

  .drawer {
    position: fixed;
    top: 0;
    right: 0;
    bottom: 0;
    width: min(580px, 95vw);
    background: #0c0b0a;
    border-left: 1px solid #1e1b18;
    z-index: 90;
    display: flex;
    flex-direction: column;
    color: #ece7e1;
    box-shadow: -16px 0 48px rgba(0, 0, 0, 0.68);
  }

  /* 3. Main Body */
  .body {
    flex: 1;
    overflow-y: auto;
  }

  .empty {
    padding: 40px 18px;
    text-align: center;
    color: #6c645c;
    font-size: 13px;
  }
</style>
