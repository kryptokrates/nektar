<script lang="ts">
  import type { RepoBlob, RepoCommit, RepoTreeEntry, UnifiedFileDiff } from '../lib/types'
  import { agoUnix } from '../lib/format'
  import { firstLine, shortSha, type DiffChange } from './RepoCommitUtils'
  import RepoCommitDetail from './RepoCommitDetail.svelte'

  interface Props {
    repo: string
    activeBranch: string
    commits: RepoCommit[]
    loadSeq: number
    busy?: boolean
    onBrowseAtRef: (ref: string) => void
    onRevertCommit: (c: RepoCommit) => void
    onCopy: (label: string, val: string) => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
  }

  let {
    repo,
    activeBranch,
    commits,
    loadSeq,
    busy = $bindable(false),
    onBrowseAtRef,
    onRevertCommit,
    onCopy,
    onToast
  }: Props = $props()

  let expandedCommit = $state<string | null>(null)
  let commitDetailLoading = $state(false)
  let commitChanges = $state<DiffChange[]>([])
  let commitPatch = $state<UnifiedFileDiff[]>([])
  let commitBlobs = $state<Record<string, RepoBlob>>({})
  let commitTreeEntries = $state<RepoTreeEntry[]>([])

  let lastLoadSeq = 0
  $effect(() => {
    if (loadSeq !== lastLoadSeq) {
      lastLoadSeq = loadSeq
      expandedCommit = null
    }
  })

  async function toggleCommit(c: RepoCommit) {
    if (!repo) return
    if (expandedCommit === c.commitHash) {
      expandedCommit = null
      return
    }
    expandedCommit = c.commitHash
    commitDetailLoading = true
    commitChanges = []
    commitPatch = []
    commitBlobs = {}
    commitTreeEntries = []
    try {
      const parentHash = c.parents?.[0]?.commitHash ?? ''
      const diffUrl = parentHash
        ? `/v1/repos/${encodeURIComponent(repo)}/diff?from=${encodeURIComponent(parentHash)}&to=${encodeURIComponent(c.commitHash)}`
        : `/v1/repos/${encodeURIComponent(repo)}/diff?to=${encodeURIComponent(c.commitHash)}`
      const [treeRes, diffRes] = await Promise.all([
        fetch(`/v1/repos/${encodeURIComponent(repo)}/tree?commit=${encodeURIComponent(c.commitHash)}`),
        fetch(diffUrl)
      ])
      if (treeRes.ok) {
        const td = (await treeRes.json()) as {
          tree?: { entries?: RepoTreeEntry[] }
          blobs?: Record<string, RepoBlob>
        }
        commitTreeEntries = td.tree?.entries ?? []
        commitBlobs = td.blobs ?? {}
      }
      if (diffRes.ok) {
        const dd = (await diffRes.json()) as { changes?: DiffChange[]; patch?: UnifiedFileDiff[] }
        commitChanges = dd.changes ?? []
        commitPatch = dd.patch ?? []
      }
      if (!parentHash && commitChanges.length === 0) {
        commitChanges = commitTreeEntries.map((e) => ({
          kind: 'added',
          path: e.path,
          newBlobHash: e.blobHash
        }))
      }
    } catch {
      // keep silent — commit metadata still displays
    } finally {
      commitDetailLoading = false
    }
  }

  const hasAnyMerge = $derived(commits.some((c) => (c.parents?.length ?? 0) > 1))
</script>

{#if commits.length === 0}
  <div class="empty">No commits on <code>{activeBranch}</code> yet.</div>
{:else}
  <div class="git-log">
    {#each commits as c, i (c.commitHash)}
      {@const isExp = expandedCommit === c.commitHash}
      {@const isMerge = (c.parents?.length ?? 0) > 1}
      {@const isAnchored = Boolean(c.anchorTxid)}
      {@const isFirst = i === 0}
      {@const isLast = i === commits.length - 1}
      {@const marketFork = c.parents?.some((p) => Boolean(p.repoGenesis))}

      <div class="log-entry" class:expanded={isExp}>
        <button class="log-row" onclick={() => toggleCommit(c)}>
          <!-- Real SVG Git Graph Rail -->
          <svg class="rail-svg" class:wide={hasAnyMerge} viewBox={hasAnyMerge ? '0 0 28 44' : '0 0 18 44'} aria-hidden="true">
            {#if !isFirst}
              <line x1="9" y1="0" x2="9" y2="22" stroke={isAnchored ? '#2d6a42' : '#2b2622'} stroke-width="1.5" />
            {/if}
            {#if !isLast}
              <line x1="9" y1="22" x2="9" y2="44" stroke={isAnchored ? '#2d6a42' : '#2b2622'} stroke-width="1.5" />
            {/if}
            {#if isMerge}
              <path d="M 22 44 C 22 31, 14 27, 9 22" fill="none" stroke="#c87f1e" stroke-width="1.5" />
            {/if}
            {#if isMerge}
              <circle cx="9" cy="22" r="4.5" fill="#0d0c0b" stroke="#f7991b" stroke-width="1.8" />
              <circle cx="9" cy="22" r="1.8" fill="#f7991b" />
            {:else if isAnchored}
              <circle cx="9" cy="22" r="3.6" fill="#4cc26b" />
            {:else}
              <circle cx="9" cy="22" r="3.5" fill="#0d0c0b" stroke="#6c645c" stroke-width="1.5" />
            {/if}
          </svg>

          <!-- Clean single-line commit subject + subtle right-aligned SHA & time -->
          <div class="log-main">
            <span class="log-msg">{firstLine(c.message)}</span>
            {#if isMerge}
              <span class="log-sub">{marketFork ? 'merged external UTXO fork' : 'merged branch'}</span>
            {/if}
          </div>

          <div class="log-right">
            <span class="log-sha" title={c.commitHash}>{shortSha(c.commitHash, 7)}</span>
            <span class="log-time">{agoUnix(c.timestamp)}</span>
          </div>
        </button>

        {#if isExp}
          <RepoCommitDetail
            {repo}
            commit={c}
            loading={commitDetailLoading}
            patch={commitPatch}
            changes={commitChanges}
            blobs={commitBlobs}
            bind:busy
            {onBrowseAtRef}
            {onRevertCommit}
            {onCopy}
            {onToast}
          />
        {/if}
      </div>
    {/each}
  </div>
{/if}

<style>
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

  /* Git Log Graph List */
  .git-log {
    display: flex;
    flex-direction: column;
  }
  .log-entry {
    border-bottom: 1px solid #151311;
  }
  .log-entry.expanded {
    background: #100e0d;
  }
  .log-row {
    display: flex;
    align-items: center;
    gap: 10px;
    width: 100%;
    min-height: 44px;
    padding: 0 18px 0 14px;
    border: 0;
    background: transparent;
    color: inherit;
    text-align: left;
    cursor: pointer;
    transition: background 100ms;
  }
  .log-row:hover {
    background: #13110f;
  }

  .rail-svg {
    width: 18px;
    height: 44px;
    flex-shrink: 0;
    display: block;
  }
  .rail-svg.wide {
    width: 28px;
  }
  .log-main {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 1px;
    padding: 8px 0;
  }
  .log-msg {
    font: 500 13px system-ui, -apple-system, sans-serif;
    color: #ece7e1;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    line-height: 1.35;
  }
  .log-sub {
    font: 400 11px system-ui, sans-serif;
    color: #a67c46;
  }
  .log-right {
    display: flex;
    align-items: center;
    gap: 12px;
    flex-shrink: 0;
  }
  .log-sha {
    font: 500 11.5px ui-monospace, SFMono-Regular, Menlo, monospace;
    color: #706860;
    transition: color 120ms;
  }
  .log-row:hover .log-sha {
    color: #b7afa6;
  }
  .log-time {
    font: 400 11.5px system-ui, sans-serif;
    color: #57514b;
    min-width: 28px;
    text-align: right;
  }
</style>
