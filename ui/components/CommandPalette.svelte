<script lang="ts">
  interface CommandItem {
    id: string
    label: string
    hint: string
    run: () => void
  }

  interface Props {
    open: boolean
    commands: CommandItem[]
    onClose: () => void
  }

  let { open, commands, onClose }: Props = $props()
  let q = $state('')
  let inputEl = $state<HTMLInputElement | null>(null)

  const filtered = $derived.by(() => {
    const t = q.trim().toLowerCase()
    if (!t) return commands
    return commands.filter((c) => `${c.label} ${c.hint}`.toLowerCase().includes(t))
  })

  $effect(() => {
    if (open) {
      q = ''
      setTimeout(() => inputEl?.focus(), 10)
    }
  })

  function pick(c: CommandItem) {
    onClose()
    c.run()
  }

  function onKey(e: KeyboardEvent) {
    if (!open) return
    if (e.key === 'Escape') {
      e.preventDefault()
      onClose()
    } else if (e.key === 'Enter' && filtered.length > 0) {
      e.preventDefault()
      pick(filtered[0])
    }
  }
</script>

{#if open}
  <!-- svelte-ignore a11y_click_events_have_key_events -->
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div class="overlay" onclick={onClose}>
    <div class="palette" onclick={(e) => e.stopPropagation()} onkeydown={onKey}>
      <input bind:this={inputEl} bind:value={q} class="pin" placeholder="Type a command or jump to a view…" />
      <div class="plist">
        {#each filtered as c}
          <button class="pitem" onclick={() => pick(c)}>
            <span>{c.label}</span>
            <small>{c.hint}</small>
          </button>
        {:else}
          <div class="pempty">No matching commands</div>
        {/each}
      </div>
    </div>
  </div>
{/if}

<style>
  .overlay {
    position: fixed; inset: 0; z-index: 80;
    background: rgb(0 0 0 / 60%); backdrop-filter: blur(4px);
    display: flex; align-items: flex-start; justify-content: center;
    padding: 12vh 1rem 1rem;
  }
  .palette {
    width: min(520px, 100%);
    background: #141210; border: 1px solid var(--line2); border-radius: 14px;
    box-shadow: 0 24px 60px rgb(0 0 0 / 70%); overflow: hidden;
  }
  .pin {
    width: 100%; background: transparent; border: 0; border-bottom: 1px solid var(--line);
    color: var(--ink); padding: 0.9rem 1.1rem; font: inherit; font-size: 0.95rem; outline: none;
  }
  .plist { max-height: 320px; overflow-y: auto; padding: 0.4rem; display: grid; gap: 0.15rem; }
  .pitem {
    display: flex; align-items: center; justify-content: space-between; gap: 1rem;
    background: none; border: 0; color: var(--ink); font: inherit; font-size: 0.86rem;
    padding: 0.6rem 0.75rem; border-radius: 9px; cursor: pointer; text-align: left;
  }
  .pitem:hover { background: var(--hover); }
  .pitem small { color: var(--mut); font-size: 0.76rem; }
  .pempty { padding: 1.2rem; text-align: center; color: var(--mut); font-size: 0.84rem; }
</style>
