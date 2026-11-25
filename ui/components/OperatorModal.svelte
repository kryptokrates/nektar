<script lang="ts">
  interface Props {
    mode: 'none' | 'token' | 'confirm'
    title: string
    detail: string
    onResolveToken: (tok: string | null) => void
    onResolveConfirm: (ok: boolean) => void
  }

  let { mode, title, detail, onResolveToken, onResolveConfirm }: Props = $props()
  let tok = $state('')

  $effect(() => {
    if (mode === 'token') tok = ''
  })

  function submitTok(e?: Event) {
    e?.preventDefault()
    if (!tok.trim()) return
    const v = tok.trim()
    tok = ''
    onResolveToken(v)
  }
</script>

{#if mode !== 'none'}
  <!-- svelte-ignore a11y_click_events_have_key_events -->
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div class="overlay" onclick={() => (mode === 'token' ? onResolveToken(null) : onResolveConfirm(false))}>
    <div class="modal" onclick={(e) => e.stopPropagation()}>
      <h3>{title}</h3>
      <p class="mut">{detail}</p>
      {#if mode === 'token'}
        <form onsubmit={submitTok} class="form">
          <input class="in" type="password" bind:value={tok} placeholder="NEKTAR_OPERATOR_TOKEN" autocomplete="current-password" />
          <div class="acts">
            <button type="button" class="ghost" onclick={() => onResolveToken(null)}>Cancel</button>
            <button type="submit" class="pri" disabled={!tok.trim()}>Authorize</button>
          </div>
        </form>
      {:else}
        <div class="acts">
          <button class="ghost" onclick={() => onResolveConfirm(false)}>Cancel</button>
          <button class="pri" onclick={() => onResolveConfirm(true)}>Confirm</button>
        </div>
      {/if}
    </div>
  </div>
{/if}

<style>
  .overlay {
    position: fixed; inset: 0; z-index: 90;
    background: rgb(0 0 0 / 65%); backdrop-filter: blur(4px);
    display: grid; place-items: center; padding: 1rem;
  }
  .modal {
    width: min(420px, 100%); background: #141210; border: 1px solid var(--line2);
    border-radius: 14px; padding: 1.3rem; display: grid; gap: 0.85rem;
    box-shadow: 0 24px 60px rgb(0 0 0 / 70%);
  }
  h3 { margin: 0; font-size: 1rem; }
  .mut { margin: 0; color: var(--mut); font-size: 0.84rem; line-height: 1.45; }
  .form { display: grid; gap: 0.85rem; }
  .in {
    width: 100%; background: var(--bg); border: 1px solid var(--line2); border-radius: 9px;
    color: var(--ink); padding: 0.6rem 0.8rem; font-family: ui-monospace, monospace; font-size: 0.84rem; outline: none;
  }
  .in:focus { border-color: var(--amber); }
  .acts { display: flex; justify-content: flex-end; gap: 0.5rem; }
  .ghost {
    background: transparent; border: 1px solid var(--line2); color: var(--mut);
    border-radius: 999px; padding: 0.44rem 0.95rem; font: inherit; font-size: 0.82rem; font-weight: 600; cursor: pointer;
  }
  .pri {
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.44rem 1.05rem; font: inherit; font-size: 0.82rem; font-weight: 800; cursor: pointer;
  }
  .pri:disabled { opacity: 0.45; }
</style>
