<script lang="ts">
  import type { RepoTag } from '../lib/types'
  import { operatorJSON } from '../lib/api'

  interface Props {
    repo: string
    branches: string[]
    tags: RepoTag[]
    activeBranch: string
    pinnedRef: string | null
    branchMenuOpen?: boolean
    busy?: boolean
    onToggleOpen: () => void
    onSwitchBranch: (br: string) => void
    onBrowseAtRef: (ref: string, label?: string) => void
    onMergeIntoMain: () => void
    onBranchCreated: (br: string) => Promise<void>
    onReloadRepo: () => Promise<void>
    onRefresh: () => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
    onSelectRepo?: (repoName: string) => void
  }

  let {
    repo,
    branches,
    tags,
    activeBranch,
    pinnedRef,
    branchMenuOpen = $bindable(false),
    busy = $bindable(false),
    onToggleOpen,
    onSwitchBranch,
    onBrowseAtRef,
    onMergeIntoMain,
    onBranchCreated,
    onReloadRepo,
    onRefresh,
    onToast,
    onSelectRepo
  }: Props = $props()

  let newBranchName = $state('')
  let tagFormOpen = $state(false)
  let newTagName = $state('')
  let newTagMsg = $state('')
  let forkFormOpen = $state(false)
  let forkRepoName = $state('')

  $effect(() => {
    void repo
    tagFormOpen = false
    forkFormOpen = false
  })

  async function createBranch() {
    if (!repo || !newBranchName.trim() || busy) return
    busy = true
    try {
      const br = newBranchName.trim()
      const r = await operatorJSON(
        'POST',
        `/v1/repos/${encodeURIComponent(repo)}/branches`,
        { branch: br, from: activeBranch },
        'Create branch'
      )
      if (r && r.ok) {
        newBranchName = ''
        branchMenuOpen = false
        onToast(`Switched to new branch ${br}`)
        await onBranchCreated(br)
      } else if (r) {
        onToast(r.data.error ?? `Branch failed (${r.status})`, true)
      }
    } finally {
      busy = false
    }
  }

  async function createTag() {
    if (!repo || !newTagName.trim() || busy) return
    busy = true
    try {
      const tag = newTagName.trim()
      const r = await operatorJSON(
        'POST',
        `/v1/repos/${encodeURIComponent(repo)}/tags`,
        { tag, commit: pinnedRef ?? activeBranch, message: newTagMsg.trim() },
        `Create immutable tag ${tag}`
      )
      if (r && r.ok) {
        newTagName = ''
        newTagMsg = ''
        tagFormOpen = false
        branchMenuOpen = false
        onToast(`Signed immutable tag ${tag} ✓`)
        await onReloadRepo()
        await onRefresh()
      } else if (r) {
        onToast(r.data.error ?? `Tag failed (${r.status})`, true)
      }
    } finally {
      busy = false
    }
  }

  async function forkCurrentRepo() {
    if (!repo || !forkRepoName.trim() || busy) return
    busy = true
    try {
      const target = forkRepoName.trim()
      const r = await operatorJSON(
        'POST',
        `/v1/repos/${encodeURIComponent(repo)}/fork`,
        { branch: activeBranch, newRepo: target, autoAnchor: true },
        `Fork ${repo} as ${target}`
      )
      if (r && r.ok) {
        forkRepoName = ''
        forkFormOpen = false
        branchMenuOpen = false
        onToast(`Forked ${repo} → ${target} ✓`)
        await onRefresh()
        if (onSelectRepo) {
          onSelectRepo(target)
        } else {
          await onReloadRepo()
        }
      } else if (r) {
        onToast(r.data.error ?? `Fork failed (${r.status})`, true)
      }
    } finally {
      busy = false
    }
  }
</script>

<!-- Compact GitHub-style Branch & Tag Dropdown -->
<div class="br-wrap">
  <button
    class="br-trigger"
    class:open={branchMenuOpen}
    onclick={onToggleOpen}
    aria-expanded={branchMenuOpen}
  >
    <svg class="ico-sm" viewBox="0 0 16 16" fill="none" aria-hidden="true">
      <circle cx="4.5" cy="3.5" r="1.75" stroke="currentColor" stroke-width="1.4" />
      <circle cx="4.5" cy="12.5" r="1.75" stroke="currentColor" stroke-width="1.4" />
      <circle cx="11.5" cy="6" r="1.75" stroke="currentColor" stroke-width="1.4" />
      <path d="M4.5 5.25v5.5M11.5 7.75c0 2-2 2.75-7 2.75" stroke="currentColor" stroke-width="1.4" />
    </svg>
    <span>{activeBranch}</span>
    <svg class="chev" viewBox="0 0 12 12" fill="none" aria-hidden="true">
      <path d="M3 4.5 6 7.5 9 4.5" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" />
    </svg>
  </button>

  {#if branchMenuOpen}
    <div class="br-menu">
      <div class="bm-list">
        {#each branches as br}
          <button class="bm-item" class:cur={activeBranch === br} onclick={() => onSwitchBranch(br)}>
            <span class="bm-check">{activeBranch === br ? '✓' : ''}</span>
            <span class="bm-name">{br}</span>
          </button>
        {/each}
      </div>

      {#if tags.length > 0}
        <div class="bm-sect">Tags</div>
        <div class="bm-list">
          {#each tags as tg}
            <button class="bm-item" onclick={() => onBrowseAtRef(tg.commitHash, `tag ${tg.tag}`)}>
              <span class="bm-check">#</span>
              <span class="bm-name">{tg.tag}</span>
            </button>
          {/each}
        </div>
      {/if}

      {#if activeBranch !== 'main'}
        <button class="bm-merge" onclick={onMergeIntoMain} disabled={busy}>
          Merge {activeBranch} into main
        </button>
      {/if}

      <form
        class="bm-form"
        onsubmit={(e) => {
          e.preventDefault()
          void createBranch()
        }}
      >
        <input bind:value={newBranchName} placeholder="New branch…" maxlength="64" aria-label="New branch name" />
        <button type="submit" disabled={!newBranchName.trim() || busy}>Create</button>
      </form>

      <div class="bm-actions">
        <button
          class="bm-act"
          onclick={() => {
            tagFormOpen = !tagFormOpen
            forkFormOpen = false
          }}
        >
          Create tag…
        </button>
        <button
          class="bm-act"
          onclick={() => {
            forkFormOpen = !forkFormOpen
            tagFormOpen = false
            forkRepoName = `${repo}-fork`
          }}
        >
          Fork repo…
        </button>
      </div>

      {#if tagFormOpen}
        <form
          class="bm-subform"
          onsubmit={(e) => {
            e.preventDefault()
            void createTag()
          }}
        >
          <input bind:value={newTagName} placeholder="v1.0-audit" maxlength="64" aria-label="Tag name" />
          <input bind:value={newTagMsg} placeholder="Optional note…" maxlength="140" aria-label="Tag message" />
          <button type="submit" disabled={!newTagName.trim() || busy}>Sign Tag</button>
        </form>
      {/if}

      {#if forkFormOpen}
        <form
          class="bm-subform"
          onsubmit={(e) => {
            e.preventDefault()
            void forkCurrentRepo()
          }}
        >
          <input bind:value={forkRepoName} placeholder="new-repo-name" maxlength="64" aria-label="Fork repo name" />
          <button type="submit" disabled={!forkRepoName.trim() || busy}>Fork</button>
        </form>
      {/if}
    </div>
  {/if}
</div>

<style>
  .br-wrap {
    position: relative;
  }

  .br-trigger {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    height: 26px;
    padding: 0 8px;
    border-radius: 6px;
    border: 1px solid transparent;
    background: transparent;
    color: #b7afa6;
    font: 500 12.5px ui-monospace, SFMono-Regular, Menlo, monospace;
    cursor: pointer;
    transition: background 120ms, border-color 120ms, color 120ms;
  }
  .br-trigger:hover,
  .br-trigger.open {
    background: #161412;
    border-color: #27231f;
    color: #f4f0eb;
  }

  .ico-sm {
    width: 13px;
    height: 13px;
    opacity: 0.75;
  }

  .chev {
    width: 11px;
    height: 11px;
    opacity: 0.6;
  }

  .br-menu {
    position: absolute;
    top: calc(100% + 6px);
    left: 0;
    width: 245px;
    background: #141210;
    border: 1px solid #29241f;
    border-radius: 8px;
    box-shadow: 0 12px 32px rgba(0, 0, 0, 0.72);
    z-index: 25;
    overflow: hidden;
  }

  .bm-sect {
    padding: 6px 12px 2px;
    font: 600 10px system-ui, sans-serif;
    letter-spacing: 0.08em;
    text-transform: uppercase;
    color: #6b635b;
    border-top: 1px solid #221e1a;
  }

  .bm-list {
    padding: 4px;
    max-height: 150px;
    overflow-y: auto;
  }

  .bm-item {
    display: flex;
    align-items: center;
    gap: 8px;
    width: 100%;
    padding: 6px 8px;
    border: 0;
    border-radius: 5px;
    background: transparent;
    color: #b7afa6;
    font: 500 12px ui-monospace, monospace;
    text-align: left;
    cursor: pointer;
  }
  .bm-item:hover {
    background: #1e1b18;
    color: #f4f0eb;
  }
  .bm-item.cur {
    color: #f4f0eb;
  }

  .bm-check {
    width: 12px;
    color: #f7991b;
    font-size: 11px;
  }

  .bm-name {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .bm-merge {
    display: block;
    width: 100%;
    padding: 8px 12px;
    border: 0;
    border-top: 1px solid #221e1a;
    background: transparent;
    color: #f7991b;
    font: 600 11.5px system-ui, sans-serif;
    text-align: left;
    cursor: pointer;
  }
  .bm-merge:hover {
    background: #1d170f;
  }

  .bm-form {
    display: flex;
    gap: 6px;
    padding: 8px;
    border-top: 1px solid #221e1a;
    background: #100e0d;
  }
  .bm-form input,
  .bm-subform input {
    flex: 1;
    min-width: 0;
    background: #0a0908;
    border: 1px solid #28231e;
    border-radius: 5px;
    padding: 4px 8px;
    color: #f4f0eb;
    font: 500 11.5px ui-monospace, monospace;
    outline: none;
  }
  .bm-form input:focus,
  .bm-subform input:focus {
    border-color: #5a4630;
  }
  .bm-form button,
  .bm-subform button {
    padding: 4px 9px;
    border-radius: 5px;
    border: 1px solid #2d2823;
    background: #1c1916;
    color: #ece7e1;
    font: 600 11px system-ui, sans-serif;
    cursor: pointer;
  }
  .bm-form button:disabled,
  .bm-subform button:disabled {
    opacity: 0.4;
    cursor: default;
  }

  .bm-actions {
    display: flex;
    border-top: 1px solid #221e1a;
  }
  .bm-act {
    flex: 1;
    padding: 7px 10px;
    border: 0;
    background: transparent;
    color: #8f877f;
    font: 500 11.5px system-ui, sans-serif;
    cursor: pointer;
    text-align: center;
  }
  .bm-act + .bm-act {
    border-left: 1px solid #221e1a;
  }
  .bm-act:hover {
    background: #1b1815;
    color: #f4f0eb;
  }

  .bm-subform {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding: 8px;
    border-top: 1px solid #221e1a;
    background: #0d0c0b;
  }
</style>
