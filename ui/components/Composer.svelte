<script lang="ts">
  import { postJSON } from '../lib/api'

  interface Props {
    feeRateSatPerKB: number
    funded: boolean
    inputEl?: HTMLTextAreaElement | null
    onDone: () => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
  }

  let { funded, inputEl = $bindable(null), onDone, onToast }: Props = $props()

  type Mode = 'remember' | 'ask' | 'buy'
  let mode = $state<Mode>('remember')
  let text = $state('')
  let shard = $state('notes')
  let domain = $state('ops')
  let maxSats = $state('100')
  let busy = $state(false)

  const placeholder = $derived(
    mode === 'remember'
      ? "What should your agent remember?"
      : mode === 'ask'
        ? 'Ask your memory for a verified answer…'
        : 'What knowledge do you want to buy?'
  )

  const ctaLabel = $derived(
    busy ? 'Working…' : mode === 'remember' ? (funded ? 'Remember & Anchor' : 'Remember') : mode === 'ask' ? 'Ask' : 'Buy'
  )

  async function submit() {
    const body = text.trim()
    if (!body || busy) return
    busy = true
    try {
      if (mode === 'remember') {
        // Automatically anchor on BSV whenever wallet is funded — zero manual anchor button!
        const res = await postJSON<{ seq?: number; anchorTxid?: string }>('/v1/remember', {
          shard: shard.trim() || 'notes',
          text: body,
          autoAnchor: funded
        })
        if (!res.ok) {
          onToast(res.data.error ?? `Save failed (${res.status})`, true)
          return
        }
        text = ''
        onToast(res.data.anchorTxid ? 'Saved & anchored on BSV ✓' : 'Saved to memory ✓')
        await onDone()
      } else if (mode === 'ask') {
        const rec = await postJSON<{ items?: Array<{ shard: string; seq: number; text?: string }> }>('/v1/tools/recall', {
          query: body,
          limit: 5
        })
        if (!rec.ok) {
          onToast(rec.data.error ?? `Recall failed (${rec.status})`, true)
          return
        }
        const items = rec.data.items ?? []
        if (items.length === 0) {
          onToast('No matching memory yet.', true)
          return
        }
        const top = items[0]
        const ans = await postJSON<{ ungrounded?: boolean }>('/v1/tools/groundedAnswer', {
          claims: [{ text: (top.text ?? '').trim().slice(0, 200), cites: [{ shard: top.shard, seq: top.seq }] }],
          autoAnchor: funded
        })
        if (!ans.ok) {
          onToast(ans.data.error ?? `Grounding failed (${ans.status})`, true)
          return
        }
        text = ''
        onToast(ans.data.ungrounded ? 'Could not verify answer.' : 'Verified receipt issued ✓', Boolean(ans.data.ungrounded))
        await onDone()
      } else {
        const cap = Math.max(1, Math.floor(Number(maxSats) || 100))
        const res = await postJSON<{ status?: string }>('/v1/tools/want', {
          domains: [domain.trim() || 'ops'],
          question: body,
          maxSats: cap
        })
        if (!res.ok) {
          onToast(res.data.error ?? `Purchase failed (${res.status})`, true)
          return
        }
        text = ''
        onToast(res.data.status === 'settled' ? 'Knowledge bought & verified ✓' : `Order ${res.data.status ?? 'sent'}`)
        await onDone()
      }
    } catch (e) {
      onToast(String(e), true)
    } finally {
      busy = false
    }
  }

  function onKey(e: KeyboardEvent) {
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
      e.preventDefault()
      void submit()
    }
  }
</script>

<section class="composer">
  <div class="avatar" aria-hidden="true">
    <svg viewBox="0 0 400 400" width="36" height="36">
      <rect width="400" height="400" fill="#181513" rx="200" />
      <path
        fill="#f7991b"
        d="M 195 55 C 178 55 164 63 157 80 C 153 100 149 112 143 124 C 136 139 115 158 93 186 C 80 206 73 226 72 250 C 71 277 79 306 98 329 C 117 352 150 372 190 377 C 232 380 255 356 259 322 C 261 296 274 273 296 251 C 310 234 318 210 318 186 C 318 158 312 137 302 120 C 292 102 276 84 260 74 C 245 64 218 55 195 55 Z"
      />
    </svg>
  </div>

  <div class="col">
    <div class="modes">
      <button class="m" class:on={mode === 'remember'} onclick={() => (mode = 'remember')}>Remember</button>
      <button class="m" class:on={mode === 'ask'} onclick={() => (mode = 'ask')}>Ask</button>
      <button class="m" class:on={mode === 'buy'} onclick={() => (mode = 'buy')}>Buy</button>
    </div>

    <textarea
      bind:this={inputEl}
      bind:value={text}
      rows="2"
      {placeholder}
      onkeydown={onKey}
      disabled={busy}
    ></textarea>

    <div class="bar">
      <div class="opts">
        {#if mode === 'remember'}
          <label class="tag-in">
            <span>#</span>
            <input bind:value={shard} placeholder="notes" aria-label="Shard" />
          </label>
          {#if funded}
            <span class="auto-badge" title="Memories anchor onto BSV automatically">✓ Auto-anchor on</span>
          {/if}
        {:else if mode === 'buy'}
          <label class="tag-in">
            <span>#</span>
            <input bind:value={domain} placeholder="ops" aria-label="Domain" />
          </label>
          <label class="tag-in num">
            <span>≤</span>
            <input type="number" min="1" bind:value={maxSats} aria-label="Max satoshis" />
            <span>sats</span>
          </label>
        {/if}
      </div>

      <button class="post" onclick={submit} disabled={busy || !text.trim()}>{ctaLabel}</button>
    </div>
  </div>
</section>

<style>
  .composer {
    display: flex; gap: 0.9rem; padding: 1.05rem 1.25rem 0.95rem;
    border-bottom: 1px solid var(--line);
  }
  .avatar { flex-shrink: 0; padding-top: 0.1rem; }
  .col { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 0.5rem; }
  .modes { display: flex; gap: 0.25rem; }
  .m {
    background: none; border: 0; color: var(--mut2); font: inherit;
    font-size: 0.78rem; font-weight: 700; padding: 0.2rem 0.6rem; border-radius: 999px;
    cursor: pointer; transition: background 0.12s, color 0.12s;
  }
  .m:hover { color: var(--ink); background: var(--hover); }
  .m.on { color: var(--amber); background: rgb(247 153 27 / 13%); }
  textarea {
    width: 100%; background: transparent; border: 0; color: var(--ink);
    font: inherit; font-size: 1.04rem; line-height: 1.48; resize: none; padding: 0.15rem 0; outline: none;
  }
  textarea::placeholder { color: var(--mut2); }
  .bar {
    display: flex; align-items: center; justify-content: space-between; gap: 0.75rem;
    padding-top: 0.45rem;
  }
  .opts { display: flex; align-items: center; gap: 0.6rem; flex-wrap: wrap; }
  .tag-in {
    display: inline-flex; align-items: center; gap: 0.2rem;
    background: var(--surf); border: 1px solid var(--line); border-radius: 999px;
    padding: 0.2rem 0.6rem; font-size: 0.76rem; color: var(--mut);
  }
  .tag-in input {
    width: 68px; background: none; border: 0; color: var(--ink);
    font: inherit; font-size: 0.76rem; outline: none;
  }
  .tag-in.num input { width: 52px; font-variant-numeric: tabular-nums; }
  .auto-badge { font-size: 0.74rem; color: var(--ok); font-weight: 600; }
  .post {
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.46rem 1.15rem; font: inherit; font-size: 0.84rem; font-weight: 800;
    cursor: pointer; white-space: nowrap; transition: filter 0.14s;
  }
  .post:hover:not(:disabled) { filter: brightness(1.08); }
  .post:disabled { opacity: 0.38; cursor: default; }
</style>
