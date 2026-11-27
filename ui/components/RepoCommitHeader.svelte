<script lang="ts">
  import type { RepoTag } from '../lib/types'
  import RepoCommitBranchMenu from './RepoCommitBranchMenu.svelte'

  interface Props {
    repo: string
    branches: string[]
    tags: RepoTag[]
    activeBranch: string
    pinnedRef: string | null
    branchMenuOpen?: boolean
    composeOpen?: boolean
    busy?: boolean
    checkoutDir: string
    onSwitchBranch: (br: string) => void
    onBrowseAtRef: (ref: string, label?: string) => void
    onMergeIntoMain: () => void
    onBranchCreated: (br: string) => Promise<void>
    onReloadRepo: () => Promise<void>
    onRefresh: () => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
    onSelectRepo?: (repoName: string) => void
    onCheckoutGit: () => void
    onExportCapsule: () => void
    onClose: () => void
    onCopy: (label: string, val: string) => void
  }

  let {
    repo,
    branches,
    tags,
    activeBranch,
    pinnedRef,
    branchMenuOpen = $bindable(false),
    composeOpen = $bindable(false),
    busy = $bindable(false),
    checkoutDir,
    onSwitchBranch,
    onBrowseAtRef,
    onMergeIntoMain,
    onBranchCreated,
    onReloadRepo,
    onRefresh,
    onToast,
    onSelectRepo,
    onCheckoutGit,
    onExportCapsule,
    onClose,
    onCopy
  }: Props = $props()
</script>

<!-- 1. Single-Line Git Header (calm, typographic, zero screaming kickers or hash pills) -->
<header class="topbar">
  <div class="tb-left">
    <svg class="repo-ico" viewBox="0 0 16 16" fill="none" aria-hidden="true">
      <path
        d="M3 2.75A1.75 1.75 0 0 1 4.75 1h7.5v12.5H4.75A1.75 1.75 0 0 0 3 15.25V2.75Zm0 0v10.5M4.75 10.5h7.5"
        stroke="currentColor"
        stroke-width="1.35"
        stroke-linecap="round"
        stroke-linejoin="round"
      />
    </svg>
    <h2 class="repo-name" title={repo}>{repo}</h2>
    <span class="slash">/</span>

    <RepoCommitBranchMenu
      {repo}
      {branches}
      {tags}
      {activeBranch}
      {pinnedRef}
      bind:branchMenuOpen
      bind:busy
      onToggleOpen={() => {
        branchMenuOpen = !branchMenuOpen
      }}
      {onSwitchBranch}
      {onBrowseAtRef}
      {onMergeIntoMain}
      {onBranchCreated}
      {onReloadRepo}
      {onRefresh}
      {onToast}
      {onSelectRepo}
    />
  </div>

  <div class="tb-right">
    <button
      class="ghost-act"
      class:active={composeOpen}
      onclick={() => {
        composeOpen = !composeOpen
        branchMenuOpen = false
      }}
      title="Commit a new or updated file to this branch"
    >
      +
    </button>

    <button
      class="ghost-act"
      onclick={() => {
        branchMenuOpen = false
        onCheckoutGit()
      }}
      disabled={busy}
      title="Checkout a local .git folder on disk"
    >
      .git
    </button>

    <button class="ghost-act primary" onclick={onExportCapsule} disabled={busy} title="Download self-verifying .nkt CHIRP capsule">
      ↓ .nkt
    </button>
    <button class="icon-close" onclick={onClose} aria-label="Close">
      <svg viewBox="0 0 16 16" fill="none" aria-hidden="true">
        <path d="M4 4l8 8M12 4l-8 8" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
      </svg>
    </button>
  </div>
</header>

{#if checkoutDir}
  <div class="notice-bar">
    <span>Checked out to <code>{checkoutDir}</code></span>
    <button class="txt-btn" onclick={() => onCopy('path', checkoutDir)}>Copy</button>
  </div>
{/if}

<style>
  /* 1. Single-Line Git Header */
  .topbar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 10px;
    height: 52px;
    padding: 0 16px 0 18px;
    border-bottom: 1px solid #1a1714;
    flex-shrink: 0;
  }

  .tb-left {
    display: flex;
    align-items: center;
    gap: 7px;
    min-width: 0;
  }

  .repo-ico {
    width: 15px;
    height: 15px;
    color: #7d756d;
    flex-shrink: 0;
  }

  .repo-name {
    margin: 0;
    font: 600 14.5px system-ui, -apple-system, sans-serif;
    letter-spacing: -0.01em;
    color: #f4f0eb;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    max-width: 175px;
  }

  .slash {
    color: #3d3732;
    font: 400 14px ui-monospace, monospace;
    user-select: none;
  }

  .tb-right {
    display: flex;
    align-items: center;
    gap: 4px;
    flex-shrink: 0;
  }

  .ghost-act {
    height: 27px;
    padding: 0 9px;
    border-radius: 6px;
    border: 1px solid transparent;
    background: transparent;
    color: #8f877f;
    font: 500 12px ui-monospace, SFMono-Regular, Menlo, monospace;
    cursor: pointer;
    transition: background 120ms, color 120ms, border-color 120ms;
  }
  .ghost-act:hover,
  .ghost-act.active {
    background: #171412;
    border-color: #27221e;
    color: #f4f0eb;
  }
  .ghost-act.primary {
    color: #e4a149;
  }
  .ghost-act.primary:hover {
    color: #ffb44d;
    border-color: #3d2f1c;
    background: #1a140d;
  }

  .icon-close {
    width: 28px;
    height: 28px;
    display: grid;
    place-items: center;
    border-radius: 6px;
    border: 0;
    background: transparent;
    color: #7d756d;
    cursor: pointer;
    margin-left: 2px;
  }
  .icon-close svg {
    width: 14px;
    height: 14px;
  }
  .icon-close:hover {
    background: #181512;
    color: #f4f0eb;
  }

  .notice-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 10px;
    padding: 7px 18px;
    background: #101c14;
    border-bottom: 1px solid #1e3826;
    font-size: 11.5px;
    color: #a9d9b6;
  }
  .notice-bar code {
    font-family: ui-monospace, monospace;
    color: #4cc26b;
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
