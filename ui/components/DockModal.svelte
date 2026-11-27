<script lang="ts">
  import type { DockClient } from '../lib/types'
  import { operatorFetch } from '../lib/api'

  interface Props {
    open: boolean
    onClose: () => void
  }

  type DetectResp = {
    clients: DockClient[]
    detectedCount: number
    dockedCount: number
    containerMode: boolean
    mcpUrl: string
    openaiProxy: string
    anthropicProxy: string
    a2aCard: string
    samplePrompt: string
  }

  let { open, onClose }: Props = $props()
  let mode = $state<'apps' | 'proxy' | 'mcp'>('apps')
  let info = $state<DetectResp | null>(null)
  let loading = $state(false)
  let busyId = $state('')
  let msg = $state('')
  let isErr = $state(false)
  let probeRes = $state<{ ok: boolean; ttfcMs?: number; count?: number } | null>(null)
  let copied = $state('')

  const origin = typeof window !== 'undefined' ? window.location.origin : 'http://127.0.0.1:18420'

  const proxySnippet = $derived(
    `export OPENAI_BASE_URL="${info?.openaiProxy ?? `${origin}/v1/proxy/openai/v1`}"\nexport ANTHROPIC_BASE_URL="${info?.anthropicProxy ?? `${origin}/v1/proxy/anthropic`}"`
  )
  const mcpUrl = $derived(info?.mcpUrl ?? `${origin}/mcp`)
  const mcpJson = $derived(
    JSON.stringify(
      {
        mcpServers: {
          nektar: {
            command: 'nektar',
            args: ['mcp']
          }
        }
      },
      null,
      2
    )
  )

  async function refresh() {
    loading = true
    try {
      const r = await fetch('/v1/dock/detect')
      if (r.ok) info = await r.json()
    } finally {
      loading = false
    }
  }

  $effect(() => {
    if (open) {
      msg = ''
      isErr = false
      void refresh()
    }
  })

  async function applyDock(client: string, uninstall = false) {
    busyId = client
    msg = ''
    isErr = false
    try {
      const r = await operatorFetch('POST', '/v1/dock/apply', { client, uninstall }, uninstall ? `Undock ${client}` : `Dock ${client}`)
      if (!r) return
      const j = (await r.json().catch(() => ({}))) as { ok?: boolean; updated?: string[]; errors?: string[]; error?: string; nextStep?: string; clients?: DockClient[] }
      if (!r.ok || j.error) {
        isErr = true
        msg = j.error ?? `Dock failed (${r.status})`
        return
      }
      if (j.clients && info) info = { ...info, clients: j.clients, dockedCount: j.clients.filter((c) => c.docked).length }
      else await refresh()
      if (j.errors?.length) {
        isErr = true
        msg = j.errors.join('; ')
      } else if (j.updated?.length) {
        msg = `${uninstall ? 'Undocked' : 'Docked ✓'}: ${j.updated.join(', ')}. ${uninstall ? '' : (j.nextStep ?? '')}`
      } else {
        msg = uninstall ? 'Already undocked.' : 'Already docked — restart your IDE if tools are not visible yet.'
      }
    } finally {
      busyId = ''
    }
  }

  async function runProbe() {
    busyId = 'probe'
    try {
      const r = await fetch('/v1/dock/probe', { method: 'POST' })
      if (r.ok) probeRes = await r.json()
    } finally {
      busyId = ''
    }
  }

  async function copy(id: string, text: string) {
    await navigator.clipboard.writeText(text).catch(() => {})
    copied = id
    setTimeout(() => (copied = ''), 1500)
  }

  const detectedClients = $derived((info?.clients ?? []).filter((c) => c.detected || c.docked))
  const otherClients = $derived((info?.clients ?? []).filter((c) => !c.detected && !c.docked))
</script>

{#if open}
  <!-- svelte-ignore a11y_click_events_have_key_events -->
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div class="overlay" onclick={onClose}>
    <div class="modal" onclick={(e) => e.stopPropagation()}>
      <div class="head">
        <div>
          <h3>Dock Your AI Apps</h3>
          <p class="sub">Plug Claude, Cursor, VS Code, Codex, Hermes, or any LLM agent into this node's encrypted, BSV-verified memory.</p>
        </div>
        <button class="x" onclick={onClose} aria-label="Close">✕</button>
      </div>

      <div class="pills">
        <button class="pill" class:on={mode === 'apps'} onclick={() => (mode = 'apps')}>
          1-Click Apps {#if info && info.dockedCount > 0}<span class="badge">{info.dockedCount}</span>{/if}
        </button>
        <button class="pill" class:on={mode === 'proxy'} onclick={() => (mode = 'proxy')}>Zero-Code LLM Proxy</button>
        <button class="pill" class:on={mode === 'mcp'} onclick={() => (mode = 'mcp')}>Manual MCP / A2A</button>
      </div>

      {#if mode === 'apps'}
        <div class="explain">
          <b>What docking does:</b> Safely merges a <code>nektar</code> entry into your AI app's config file (never overwriting existing servers; reversible anytime with <b>Undock</b>).
        </div>

        {#if info?.containerMode}
          <div class="warn">
            This daemon is inside Docker, so it cannot edit host IDE files directly. Run <code>nektar dock</code> on your laptop/desktop, or use the <b>Zero-Code LLM Proxy</b> tab.
          </div>
        {:else if detectedClients.length > 0}
          <div class="bar">
            <span class="mut">{detectedClients.length} app(s) detected on this machine</span>
            <div class="acts">
              <button class="sec" onclick={runProbe} disabled={busyId === 'probe'}>
                {busyId === 'probe' ? 'Testing…' : probeRes?.ok ? `Healthy (${probeRes.ttfcMs}ms) ✓` : 'Test Memory'}
              </button>
              <button class="pri" onclick={() => applyDock('auto', false)} disabled={busyId !== ''}>
                {busyId === 'auto' ? 'Docking…' : 'Dock All Detected'}
              </button>
            </div>
          </div>

          <div class="list">
            {#each detectedClients as c}
              <div class="row">
                <div class="rinfo">
                  <div class="rhead">
                    <b>{c.label}</b>
                    <span class="tier">{c.tierLabel}</span>
                    {#if c.docked}<span class="ok">Docked ✓</span>{:else}<span class="det">Detected</span>{/if}
                  </div>
                  <div class="rdesc">{c.summary}</div>
                  <div class="rpath mono">{c.configPath}</div>
                </div>
                <div class="ract">
                  {#if c.oneClick}
                    {#if c.docked}
                      <button class="sec" onclick={() => applyDock(c.id, true)} disabled={busyId !== ''}>Undock</button>
                    {:else}
                      <button class="pri" onclick={() => applyDock(c.id, false)} disabled={busyId !== ''}>Dock</button>
                    {/if}
                  {:else if c.note}
                    <button class="sec" onclick={() => copy(c.id, c.note!)}>{copied === c.id ? 'Copied ✓' : 'Copy CLI'}</button>
                  {/if}
                </div>
              </div>
            {/each}
          </div>

          {#if otherClients.length > 0}
            <details class="more-apps">
              <summary>Other supported apps ({otherClients.length})</summary>
              <div class="chip-grid">
                {#each otherClients as c}
                  <button
                    class="chip"
                    disabled={busyId !== ''}
                    onclick={() => (c.oneClick ? applyDock(c.id, false) : copy(c.id, c.note ?? `nektar dock ${c.id}`))}
                  >
                    + {c.label}
                  </button>
                {/each}
              </div>
            </details>
          {/if}
        {:else if !loading}
          <div class="empty-box">
            <p class="mut">No local IDE config folders auto-detected. Run <code>nektar dock &lt;app&gt;</code> or click Dock below:</p>
            <div class="chip-grid">
              {#each otherClients as c}
                <button class="chip" disabled={busyId !== ''} onclick={() => (c.oneClick ? applyDock(c.id, false) : copy(c.id, c.note ?? `nektar dock ${c.id}`))}>
                  + {c.label}
                </button>
              {/each}
            </div>
          </div>
        {/if}

        {#if msg}
          <div class="status" class:err={isErr}>{msg}</div>
        {/if}

        {#if info?.samplePrompt}
          <div class="trybox">
            <div class="tryhead">
              <span><b>Try this right after restarting your AI app:</b></span>
              <button class="mini" onclick={() => copy('prompt', info!.samplePrompt)}>{copied === 'prompt' ? 'Copied ✓' : 'Copy prompt'}</button>
            </div>
            <div class="trytxt">“{info.samplePrompt}”</div>
          </div>
        {/if}
      {:else if mode === 'proxy'}
        <div class="explain">
          <b>Works with ANY OpenAI or Anthropic app (zero code changes):</b><br />
          <code>Your Agent → Nektar L0 Proxy (auto-recall; opt-in capture via X-Nektar-Capture: 1) → OpenAI / Anthropic</code>
        </div>
        <div class="codebox">
          <pre>{proxySnippet}</pre>
          <button class="pri" onclick={() => copy('proxy', proxySnippet)}>{copied === 'proxy' ? 'Copied ✓' : 'Copy'}</button>
        </div>
        <p class="mut">
          Every prompt automatically recalls verified local memories. Raw turn capture is off by default — send <code>X-Nektar-Capture: 1</code> to opt in, or <code>X-Nektar-Shard: my-project</code> to isolate shards. When daemon auth is enabled, pass <code>X-Nektar-Token: &lt;token&gt;</code> (or prefix your API key as <code>nkt:&lt;token&gt;:&lt;providerKey&gt;</code>).
        </p>
      {:else}
        <div class="explain">
          <b>Streamable HTTP MCP</b> works with VS Code, OpenCode, Gemini CLI, and remote agents (supports OAuth 2.1 + Bearer tokens).
        </div>
        <div class="codebox">
          <pre>{mcpUrl}</pre>
          <button class="pri" onclick={() => copy('mcp', mcpUrl)}>{copied === 'mcp' ? 'Copied ✓' : 'Copy URL'}</button>
        </div>
        <div class="codebox">
          <pre>{mcpJson}</pre>
          <button class="sec" onclick={() => copy('mcpjson', mcpJson)}>{copied === 'mcpjson' ? 'Copied ✓' : 'Copy JSON'}</button>
        </div>
        <p class="mut">A2A Agent Card: <code>{info?.a2aCard ?? `${origin}/.well-known/agent.json`}</code></p>
      {/if}
    </div>
  </div>
{/if}

<style>
  .overlay {
    position: fixed; inset: 0; z-index: 85;
    background: rgb(0 0 0 / 70%); backdrop-filter: blur(4px);
    display: grid; place-items: center; padding: 1rem;
  }
  .modal {
    width: min(620px, 100%); max-height: 88vh; overflow-y: auto;
    background: #141210; border: 1px solid var(--line2);
    border-radius: 14px; padding: 1.25rem; display: grid; gap: 0.8rem;
    box-shadow: 0 24px 60px rgb(0 0 0 / 75%);
  }
  .head { display: flex; align-items: flex-start; justify-content: space-between; gap: 0.75rem; }
  h3 { margin: 0; font-size: 1.05rem; }
  .sub { margin: 0.2rem 0 0; color: var(--mut); font-size: 0.8rem; line-height: 1.4; }
  .x { background: none; border: 0; color: var(--mut); cursor: pointer; font-size: 1rem; padding: 0.2rem; }
  .pills { display: flex; gap: 0.4rem; flex-wrap: wrap; }
  .pill {
    background: var(--surf); border: 1px solid var(--line); color: var(--mut);
    border-radius: 999px; padding: 0.3rem 0.75rem; font: inherit; font-size: 0.78rem; font-weight: 600; cursor: pointer;
    display: inline-flex; align-items: center; gap: 0.35rem;
  }
  .pill.on { border-color: var(--amber); color: var(--amber); background: rgb(247 153 27 / 12%); }
  .badge { background: var(--ok); color: #09120b; border-radius: 999px; padding: 0 0.4rem; font-size: 0.7rem; font-weight: 800; }
  .explain {
    background: var(--surf); border: 1px solid var(--line); border-radius: 10px;
    padding: 0.65rem 0.8rem; font-size: 0.8rem; line-height: 1.45; color: var(--mut);
  }
  .explain b { color: var(--ink); }
  .warn {
    background: rgb(247 153 27 / 10%); border: 1px solid rgb(247 153 27 / 35%);
    color: #ffd89b; border-radius: 10px; padding: 0.65rem 0.8rem; font-size: 0.8rem; line-height: 1.45;
  }
  .bar { display: flex; align-items: center; justify-content: space-between; gap: 0.5rem; flex-wrap: wrap; }
  .acts { display: flex; gap: 0.45rem; }
  .list { display: grid; gap: 0.45rem; max-height: 270px; overflow-y: auto; }
  .row {
    display: flex; align-items: center; justify-content: space-between; gap: 0.75rem;
    background: var(--bg); border: 1px solid var(--line); border-radius: 10px; padding: 0.6rem 0.75rem;
  }
  .rinfo { min-width: 0; flex: 1; }
  .rhead { display: flex; align-items: center; gap: 0.4rem; flex-wrap: wrap; font-size: 0.84rem; }
  .tier { font-size: 0.7rem; color: var(--mut); background: var(--surf); border: 1px solid var(--line); border-radius: 6px; padding: 0.08rem 0.4rem; }
  .ok { font-size: 0.72rem; font-weight: 700; color: var(--ok); }
  .det { font-size: 0.72rem; color: var(--amber); }
  .rdesc { font-size: 0.76rem; color: var(--mut); margin-top: 0.15rem; }
  .rpath { font-size: 0.68rem; color: var(--mut2); margin-top: 0.15rem; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .ract { flex-shrink: 0; }
  .status {
    background: rgb(76 194 107 / 12%); border: 1px solid rgb(76 194 107 / 35%);
    color: #b7f2c5; border-radius: 9px; padding: 0.55rem 0.75rem; font-size: 0.78rem;
  }
  .status.err { background: rgb(229 83 75 / 12%); border-color: rgb(229 83 75 / 40%); color: #ffc6c3; }
  .trybox {
    background: var(--bg); border: 1px dashed var(--line2); border-radius: 10px;
    padding: 0.65rem 0.8rem; display: grid; gap: 0.35rem;
  }
  .tryhead { display: flex; align-items: center; justify-content: space-between; font-size: 0.76rem; color: var(--mut); }
  .trytxt { font-size: 0.8rem; color: var(--ink); font-style: italic; }
  .empty-box { display: grid; gap: 0.55rem; }
  .more-apps {
    background: var(--bg); border: 1px solid var(--line); border-radius: 10px; padding: 0.5rem 0.7rem;
  }
  .more-apps summary {
    cursor: pointer; font-size: 0.76rem; font-weight: 700; color: var(--mut); user-select: none;
  }
  .more-apps[open] summary { margin-bottom: 0.5rem; color: var(--ink); }
  .chip-grid { display: flex; flex-wrap: wrap; gap: 0.4rem; }
  .chip {
    background: var(--surf); border: 1px solid var(--line); color: var(--ink);
    border-radius: 999px; padding: 0.28rem 0.68rem; font: inherit; font-size: 0.76rem; cursor: pointer;
  }
  .chip:hover { border-color: var(--amber); color: var(--amber); }
  .mut { margin: 0; color: var(--mut); font-size: 0.78rem; line-height: 1.45; }
  .codebox {
    display: flex; align-items: center; gap: 0.6rem;
    background: var(--bg); border: 1px solid var(--line); border-radius: 10px; padding: 0.6rem 0.75rem;
  }
  pre { flex: 1; margin: 0; font-size: 0.75rem; white-space: pre-wrap; word-break: break-all; }
  .pri {
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.34rem 0.85rem; font: inherit; font-size: 0.76rem; font-weight: 800; cursor: pointer; flex-shrink: 0;
  }
  .sec, .mini {
    background: var(--surf); color: var(--ink); border: 1px solid var(--line2); border-radius: 999px;
    padding: 0.3rem 0.72rem; font: inherit; font-size: 0.74rem; font-weight: 600; cursor: pointer; flex-shrink: 0;
  }
  .sec:hover, .mini:hover { border-color: var(--amber); color: var(--amber); }
</style>
