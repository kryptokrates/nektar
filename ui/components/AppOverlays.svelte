<script lang="ts">
  import type { DrawerTarget, ReputationState } from '../lib/types'
  import type { AppCommandItem } from '../lib/appActions'
  import DetailDrawer from './DetailDrawer.svelte'
  import CommandPalette from './CommandPalette.svelte'
  import CapsuleInspectorModal from './CapsuleInspectorModal.svelte'
  import DockModal from './DockModal.svelte'
  import OperatorModal from './OperatorModal.svelte'

  interface Props {
    dragDepth: number
    drawer: DrawerTarget
    reputation?: ReputationState
    paletteOpen: boolean
    commands: AppCommandItem[]
    capsuleFile: File | null
    dockOpen: boolean
    modalMode: 'none' | 'token' | 'confirm'
    modalTitle: string
    modalDetail: string
    toast: { msg: string; isErr: boolean } | null
    onPickCapsuleFile: (f: File) => void
    onCloseDrawer: () => void
    onClosePalette: () => void
    onCloseCapsule: () => void
    onCloseDock: () => void
    onResolveToken: (v: string | null) => void
    onResolveConfirm: (ok: boolean) => void
    onToast: (msg: string, isErr?: boolean) => void
    onRefresh: () => Promise<void>
    onImportedRepo: (repo: string) => void
  }

  let {
    dragDepth,
    drawer,
    reputation,
    paletteOpen,
    commands,
    capsuleFile,
    dockOpen,
    modalMode,
    modalTitle,
    modalDetail,
    toast,
    onPickCapsuleFile,
    onCloseDrawer,
    onClosePalette,
    onCloseCapsule,
    onCloseDock,
    onResolveToken,
    onResolveConfirm,
    onToast,
    onRefresh,
    onImportedRepo
  }: Props = $props()

  let capsulePickerEl = $state<HTMLInputElement | null>(null)

  export function openCapsulePicker() {
    capsulePickerEl?.click()
  }
</script>

<input
  bind:this={capsulePickerEl}
  type="file"
  accept=".nkt,application/octet-stream"
  class="hidden-file"
  onchange={(e) => {
    const inp = e.currentTarget as HTMLInputElement
    const f = inp.files?.[0] ?? null
    inp.value = ''
    if (f) onPickCapsuleFile(f)
  }}
/>

{#if dragDepth > 0}
  <div class="drop-overlay" aria-hidden="true">
    <div class="drop-card">
      <span>Drop <code>.nkt</code> Memory Capsule to verify offline</span>
    </div>
  </div>
{/if}

<DetailDrawer target={drawer} {reputation} onClose={onCloseDrawer} />
<CommandPalette open={paletteOpen} {commands} onClose={onClosePalette} />
<CapsuleInspectorModal
  file={capsuleFile}
  onClose={onCloseCapsule}
  {onToast}
  {onRefresh}
  onImported={onImportedRepo}
/>
<DockModal open={dockOpen} onClose={onCloseDock} />
<OperatorModal
  mode={modalMode}
  title={modalTitle}
  detail={modalDetail}
  {onResolveToken}
  {onResolveConfirm}
/>

{#if toast}
  <div class="toast" class:err={toast.isErr} role="status">{toast.msg}</div>
{/if}

<style>
  .hidden-file {
    display: none;
  }
  .drop-overlay {
    position: fixed;
    inset: 0;
    z-index: 120;
    background: rgba(9, 8, 7, 0.78);
    backdrop-filter: blur(4px);
    display: grid;
    place-items: center;
    pointer-events: none;
  }
  .drop-card {
    padding: 24px 36px;
    border: 1.5px dashed #f7991b;
    border-radius: 14px;
    background: #141210;
    color: #f4f0eb;
    font: 600 15px system-ui, sans-serif;
    box-shadow: 0 24px 60px rgba(0, 0, 0, 0.72);
  }
  .drop-card code {
    font-family: ui-monospace, monospace;
    color: #f7991b;
  }
  .toast {
    position: fixed;
    left: 50%;
    bottom: 1.5rem;
    transform: translateX(-50%);
    z-index: 100;
    background: #1b1815;
    border: 1px solid var(--line2);
    color: var(--ink);
    padding: 0.55rem 1.05rem;
    border-radius: 999px;
    font-size: 0.82rem;
    font-weight: 600;
    box-shadow: 0 12px 34px rgb(0 0 0 / 65%);
  }
  .toast.err {
    border-color: var(--bad);
    color: #ffd5d2;
  }
  @media (max-width: 720px) {
    .toast {
      bottom: 4.5rem;
    }
  }
</style>
