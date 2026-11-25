<script lang="ts">
  interface Props {
    open: boolean
    onClose: () => void
  }

  let { open, onClose }: Props = $props()
  let mode = $state<'dock' | 'proxy' | 'mcp'>('dock')
  let copied = $state('')

  const origin = typeof window !== 'undefined' ? window.location.origin : 'http://127.0.0.1:18420'
  const snippets = $derived({
    dock: 'nektar dock',
    proxy: `export OPENAI_BASE_URL="${origin}/v1/proxy/openai/v1"\nexport ANTHROPIC_BASE_URL="${origin}/v1/proxy/anthropic"`,
    mcp: `${origin}/mcp`
  })

  async function copy(text: string) {
    await navigator.clipboard.writeText(text).catch(() => {})
    copied = mode
    setTimeout(() => (copied = ''), 1500)
  }
</script>

{#if open}
  <!-- svelte-ignore a11y_click_events_have_key_events -->
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div class="overlay" onclick={onClose}>
    <div class="modal" onclick={(e) => e.stopPropagation()}>
      <div class="head">
        <h3>Dock your AI agent</h3>
        <button class="x" onclick={onClose}>✕</button>
      </div>
      <div class="pills">
        <button class="pill" class:on={mode === 'dock'} onclick={() => (mode = 'dock')}>Auto-Dock CLI</button>
        <button class="pill" class:on={mode === 'proxy'} onclick={() => (mode = 'proxy')}>L0 LLM Proxy</button>
        <button class="pill" class:on={mode === 'mcp'} onclick={() => (mode = 'mcp')}>MCP URL</button>
      </div>
      <p class="mut">
        {#if mode === 'dock'}Auto-wires Claude Code, Cursor, VS Code, Windsurf, Codex, Zed, Cline, Goose, Hermes &amp; OpenClaw.
        {:else if mode === 'proxy'}Transparent memory injection for any OpenAI or Anthropic agent with zero code changes.
        {:else}Streamable HTTP MCP endpoint supporting OAuth 2.1 and Bearer tokens.{/if}
      </p>
      <div class="codebox">
        <pre>{snippets[mode]}</pre>
        <button class="pri" onclick={() => copy(snippets[mode])}>{copied === mode ? 'Copied ✓' : 'Copy'}</button>
      </div>
    </div>
  </div>
{/if}

<style>
  .overlay {
    position: fixed; inset: 0; z-index: 85;
    background: rgb(0 0 0 / 65%); backdrop-filter: blur(4px);
    display: grid; place-items: center; padding: 1rem;
  }
  .modal {
    width: min(500px, 100%); background: #141210; border: 1px solid var(--line2);
    border-radius: 14px; padding: 1.3rem; display: grid; gap: 0.85rem;
    box-shadow: 0 24px 60px rgb(0 0 0 / 70%);
  }
  .head { display: flex; align-items: center; justify-content: space-between; }
  h3 { margin: 0; font-size: 1rem; }
  .x { background: none; border: 0; color: var(--mut); cursor: pointer; }
  .pills { display: flex; gap: 0.4rem; flex-wrap: wrap; }
  .pill {
    background: var(--surf); border: 1px solid var(--line); color: var(--mut);
    border-radius: 999px; padding: 0.28rem 0.7rem; font: inherit; font-size: 0.78rem; font-weight: 600; cursor: pointer;
  }
  .pill.on { border-color: var(--amber); color: var(--amber); background: rgb(247 153 27 / 12%); }
  .mut { margin: 0; color: var(--mut); font-size: 0.82rem; line-height: 1.45; }
  .codebox {
    display: flex; align-items: center; gap: 0.6rem;
    background: var(--bg); border: 1px solid var(--line); border-radius: 10px; padding: 0.6rem 0.75rem;
  }
  pre { flex: 1; margin: 0; font-size: 0.78rem; white-space: pre-wrap; word-break: break-all; }
  .pri {
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.36rem 0.85rem; font: inherit; font-size: 0.78rem; font-weight: 800; cursor: pointer; flex-shrink: 0;
  }
</style>
