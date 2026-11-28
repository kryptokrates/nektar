<script lang="ts">
  import { onDestroy } from 'svelte'
  import type { State } from '../lib/types'

  interface Props {
    s: State
    live: boolean
    onToast: (msg: string, isErr?: boolean) => void
  }

  let { s, live, onToast }: Props = $props()

  const capabilityPills = [
    'Pure-Go Single Binary',
    'AWS Cedar',
    'Google Mangle',
    'BRC-100 / BRC-369',
    'MCP + A2A + Git'
  ] as const

  let copiedCard = $state(false)
  let copiedTimer: ReturnType<typeof setTimeout> | null = null
  let showRawState = $state(false)

  function shareCardHref(): string {
    return s.share?.url || '/card'
  }

  function shareCardAbsoluteUrl(): string {
    if (s.share?.url) return s.share.url
    if (typeof window !== 'undefined') return `${window.location.origin}/card`
    return '/card'
  }

  async function copyShareCardUrl() {
    const url = shareCardAbsoluteUrl()
    try {
      await navigator.clipboard.writeText(url)
      copiedCard = true
      if (copiedTimer) clearTimeout(copiedTimer)
      copiedTimer = setTimeout(() => (copiedCard = false), 1800)
      onToast('Public Share Card URL copied ✓')
    } catch {
      onToast('Could not copy URL to clipboard', true)
    }
  }

  onDestroy(() => {
    if (copiedTimer) clearTimeout(copiedTimer)
  })
</script>

<div class="pad">
  <div class="glbl">About Nektar</div>
  <section class="about-card" aria-label="About Nektar">
    <div class="about-head">
      <svg viewBox="0 0 400 400" width="38" height="38" class="about-logo" aria-hidden="true">
        <rect width="400" height="400" fill="#181513" rx="84" />
        <path
          fill="#f7991b"
          d="M 195 55 C 178 55 164 63 157 80 C 153 100 149 112 143 124 C 136 139 115 158 93 186 C 80 206 73 226 72 250 C 71 277 79 306 98 329 C 117 352 150 372 190 377 C 232 380 255 356 259 322 C 261 296 274 273 296 251 C 310 234 318 210 318 186 C 318 158 312 137 302 120 C 292 102 276 84 260 74 C 245 64 218 55 195 55 Z"
        />
        <circle cx="307" cy="78" r="38" fill="#f7991b" />
        <ellipse cx="163" cy="190" rx="20" ry="21" fill="#181513" />
        <ellipse cx="231" cy="190" rx="20" ry="21" fill="#181513" />
        <path d="M 150 265.5 C 161 279 176 287 196 288 C 216 287 230 278 243 265" fill="none" stroke="#181513" stroke-width="24" stroke-linecap="round" />
      </svg>
      <div class="about-title-col">
        <div class="about-title-row">
          <h2>Nektar</h2>
          <span class="ver-pill mono">v0.1</span>
        </div>
        <p class="about-summary">
          One shared, verifiable brain for all your AI agents — encrypted, Git-versioned memory &amp; BSV knowledge market.
        </p>
      </div>
    </div>

    <div class="cap-row" aria-label="Core capabilities">
      {#each capabilityPills as pill}
        <span class="cap-pill">{pill}</span>
      {/each}
    </div>

    <div class="about-actions">
      <a class="act-link" href="https://nektar.run" target="_blank" rel="noopener noreferrer">
        <span>Website</span>
        <span class="ext" aria-hidden="true">↗</span>
      </a>
      <a class="act-link" href="https://github.com/kryptokrates/nektar" target="_blank" rel="noopener noreferrer">
        <span>GitHub</span>
        <span class="ext" aria-hidden="true">↗</span>
      </a>
      <a class="act-link" href="/.well-known/agent.json" target="_blank" rel="noopener noreferrer">
        <span>Agent Card</span>
        <span class="ext" aria-hidden="true">↗</span>
      </a>
      <div class="share-split">
        <a class="act-link share-open" href={shareCardHref()} target="_blank" rel="noopener noreferrer">
          <span>Public Share Card</span>
          <span class="ext" aria-hidden="true">↗</span>
        </a>
        <button
          type="button"
          class="act-copy"
          class:done={copiedCard}
          onclick={() => void copyShareCardUrl()}
          title="Copy Public Share Card URL"
        >
          {copiedCard ? 'Copied ✓' : 'Copy URL'}
        </button>
      </div>
    </div>

    <div class="honest-box" role="note" aria-label="Honest Boundaries">
      <div class="honest-head">
        <svg viewBox="0 0 24 24" width="14" height="14" class="honest-ico" aria-hidden="true">
          <path
            d="M12 3l7 3v5c0 4.5-3 8.2-7 10-4-1.8-7-5.5-7-10V6l7-3z"
            fill="none"
            stroke="currentColor"
            stroke-width="1.8"
            stroke-linejoin="round"
          />
          <path d="M12 8.5v4.2M12 15.6h.01" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" />
        </svg>
        <b>Honest Boundaries</b>
      </div>
      <p>
        Proofs show a cited record was committed, is unaltered, and existed by its block time — never that it is true. Model identity is declared, not zkML-proven.
      </p>
    </div>
  </section>

  <div class="glbl">Node Health</div>
  <div class="group">
    <div class="grow"><b>Status</b><span class="val" class:ok={live}>{live ? 'Live' : 'Offline'}</span></div>
    <div class="grow"><b>Network</b><span class="val">{s.network}</span></div>
    <div class="grow">
      <b>Durable Deals</b>
      <span
        class="val"
        class:ok={s.automation?.rt.executor === 'go-workflows' || s.automation?.rt.executor === 'restate'}
        class:warn={s.automation?.rt.executor !== 'go-workflows' && s.automation?.rt.executor !== 'restate' && s.network !== 'memory'}
      >
        {s.automation?.rt.executor === 'go-workflows' || s.automation?.rt.executor === 'restate'
          ? 'Embedded SQLite WAL Active'
          : s.network === 'memory'
            ? 'Memory Mode'
            : 'Workflow runtime starting…'}
      </span>
    </div>
    <div class="grow"><b>Memories</b><span class="val">{s.memory.anchored} / {s.memory.records} anchored</span></div>
    <div class="grow"><b>Identity</b><span class="val mono">{s.pub}…</span></div>
  </div>

  <div class="glbl">Developer</div>
  <div class="group">
    <button class="grow clickable" onclick={() => (showRawState = !showRawState)}>
      <b>Inspect Raw State JSON</b>
      <span class="arr">{showRawState ? '▾' : '→'}</span>
    </button>
  </div>
  {#if showRawState}
    <pre class="raw">{JSON.stringify(s, null, 2)}</pre>
  {/if}
</div>

<style>
  .pad { padding: 1.35rem 1.5rem 2.4rem; display: grid; gap: 1rem; width: 100%; }
  .glbl {
    font-size: 0.7rem; font-weight: 700; letter-spacing: 0.1em; text-transform: uppercase;
    color: var(--mut2); margin-top: 0.15rem;
  }
  .group { background: var(--surf); border: 1px solid var(--line); border-radius: 14px; overflow: hidden; }
  .grow {
    width: 100%; display: flex; align-items: center; justify-content: space-between; gap: 0.8rem;
    padding: 0.88rem 1rem; border-bottom: 1px solid var(--line);
    background: none; border-left: 0; border-right: 0; border-top: 0;
    color: var(--ink); font: inherit; font-size: 0.88rem; text-align: left;
  }
  .grow:last-child { border-bottom: 0; }
  .grow.clickable { cursor: pointer; }
  .grow.clickable:hover { background: var(--hover); }
  .val { color: var(--mut); font-size: 0.84rem; }
  .val.ok { color: var(--ok); font-weight: 600; }
  .val.warn { color: var(--amber); }
  .arr { color: var(--mut2); }
  .raw {
    background: var(--surf); border: 1px solid var(--line); border-radius: 12px;
    padding: 0.85rem; font-size: 0.72rem; max-height: 360px; overflow: auto; margin: 0;
  }
  .about-card {
    background: var(--surf); border: 1px solid var(--line); border-radius: 14px;
    padding: 1.15rem 1.15rem 1.1rem; display: grid; gap: 0.95rem;
  }
  .about-head { display: flex; align-items: flex-start; gap: 0.85rem; }
  .about-logo { border-radius: 9px; border: 1px solid var(--line2); flex-shrink: 0; }
  .about-title-col { display: grid; gap: 0.3rem; min-width: 0; }
  .about-title-row { display: flex; align-items: center; gap: 0.5rem; flex-wrap: wrap; }
  .about-title-row h2 {
    margin: 0; font-size: 1.04rem; font-weight: 800; letter-spacing: -0.02em; color: var(--ink);
  }
  .ver-pill {
    font-size: 0.7rem; font-weight: 700; color: var(--amber);
    background: rgb(247 153 27 / 12%); border: 1px solid rgb(247 153 27 / 32%);
    border-radius: 999px; padding: 0.08rem 0.5rem;
  }
  .about-summary { margin: 0; font-size: 0.84rem; line-height: 1.48; color: var(--mut); }
  .cap-row { display: flex; flex-wrap: wrap; gap: 0.42rem; }
  .cap-pill {
    font-size: 0.72rem; font-weight: 600; color: #d6ccc3;
    background: var(--bg); border: 1px solid var(--line);
    border-radius: 999px; padding: 0.22rem 0.64rem; letter-spacing: 0.005em;
  }
  .about-actions { display: flex; flex-wrap: wrap; align-items: center; gap: 0.48rem; }
  .act-link {
    display: inline-flex; align-items: center; gap: 0.35rem; text-decoration: none;
    background: var(--bg); border: 1px solid var(--line2); color: var(--ink);
    border-radius: 9px; padding: 0.4rem 0.72rem; font-size: 0.78rem; font-weight: 700;
    transition: border-color 0.14s, color 0.14s, background 0.14s;
  }
  .act-link:hover { border-color: var(--amber); color: var(--amber); background: rgb(247 153 27 / 6%); }
  .ext { font-size: 0.74rem; opacity: 0.7; }
  .share-split { display: inline-flex; align-items: stretch; }
  .share-open { border-top-right-radius: 0; border-bottom-right-radius: 0; }
  .act-copy {
    background: #1a1714; border: 1px solid var(--line2); border-left: 0;
    border-radius: 0 9px 9px 0; color: var(--mut); padding: 0.4rem 0.68rem;
    font: inherit; font-size: 0.75rem; font-weight: 700; cursor: pointer;
    transition: border-color 0.14s, color 0.14s, background 0.14s;
  }
  .act-copy:hover { color: var(--ink); background: var(--hover); }
  .act-copy.done { color: var(--ok); border-color: rgb(76 194 107 / 40%); }
  .honest-box {
    background: rgb(247 153 27 / 5%); border: 1px solid rgb(247 153 27 / 22%);
    border-left: 3px solid var(--amber); border-radius: 10px;
    padding: 0.72rem 0.88rem; display: grid; gap: 0.28rem;
  }
  .honest-head {
    display: flex; align-items: center; gap: 0.4rem; color: var(--amber);
    font-size: 0.74rem; letter-spacing: 0.04em; text-transform: uppercase;
  }
  .honest-ico { flex-shrink: 0; }
  .honest-box p { margin: 0; font-size: 0.79rem; line-height: 1.48; color: var(--mut); }
</style>
