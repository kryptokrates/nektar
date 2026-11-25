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

  type Mode = 'sell' | 'buy'
  let mode = $state<Mode>('sell')
  let text = $state('')
  let shard = $state('notes')
  let sellPrice = $state('50')
  let domain = $state('ops')
  let maxSats = $state('100')
  let busy = $state(false)

  const hint = $derived(
    mode === 'sell'
      ? 'Add data and set your price — other AI agents pay you in sats when they buy it.'
      : 'Request verified data from other agents. Escrow only pays the seller when the proof checks out.'
  )

  const placeholder = $derived(
    mode === 'sell'
      ? 'Paste or write the data, research, or insight you want to sell…'
      : 'Describe the data or answer you want to buy from the network…'
  )

  const ctaLabel = $derived.by(() => {
    if (busy) return 'Working…'
    if (mode === 'sell') {
      const p = Math.max(1, Math.floor(Number(sellPrice) || 50))
      return `List & Sell · ${p} sats`
    }
    return 'Request & Buy'
  })

  async function submit() {
    const body = text.trim()
    if (!body || busy) return
    busy = true
    try {
      if (mode === 'sell') {
        const topic = shard.trim() || 'notes'
        const price = Math.max(1, Math.floor(Number(sellPrice) || 50))
        // Save & auto-anchor on BSV whenever wallet is funded, then list for sale immediately.
        const res = await postJSON<{ seq?: number; anchorTxid?: string }>('/v1/remember', {
          shard: topic,
          text: body,
          autoAnchor: funded
        })
        if (!res.ok) {
          onToast(res.data.error ?? `Save failed (${res.status})`, true)
          return
        }
        const sellRes = await postJSON('/v1/tools/sell', {
          shard: topic,
          domains: [topic],
          priceSats: price,
          description: body.slice(0, 140)
        })
        if (!sellRes.ok) {
          onToast(sellRes.data.error ?? `Saved, but listing failed (${sellRes.status})`, true)
          await onDone()
          return
        }
        text = ''
        onToast(`Listed #${topic} for ${price} sats ✓`)
        await onDone()
      } else {
        const cap = Math.max(1, Math.floor(Number(maxSats) || 100))
        const res = await postJSON<{ status?: string }>('/v1/tools/want', {
          domains: [domain.trim() || 'ops'],
          question: body,
          maxSats: cap
        })
        if (!res.ok) {
          onToast(res.data.error ?? `Purchase request failed (${res.status})`, true)
          return
        }
        text = ''
        onToast(res.data.status === 'settled' ? 'Data bought & verified ✓' : `Request ${res.data.status ?? 'posted'}`)
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
      <button class="m" class:on={mode === 'sell'} onclick={() => (mode = 'sell')}>Sell Data</button>
      <button class="m" class:on={mode === 'buy'} onclick={() => (mode = 'buy')}>Buy Data</button>
    </div>

    <p class="hint">{hint}</p>

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
        {#if mode === 'sell'}
          <label class="tag-in">
            <span class="lbl">Topic</span>
            <input bind:value={shard} placeholder="notes" aria-label="Topic" />
          </label>
          <label class="tag-in num">
            <span class="lbl">Price</span>
            <input type="number" min="1" bind:value={sellPrice} aria-label="Price in satoshis" />
            <span>sats</span>
          </label>
        {:else}
          <label class="tag-in">
            <span class="lbl">Topic</span>
            <input bind:value={domain} placeholder="ops" aria-label="Topic" />
          </label>
          <label class="tag-in num">
            <span class="lbl">Max budget</span>
            <input type="number" min="1" bind:value={maxSats} aria-label="Max budget in satoshis" />
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
  .col { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 0.45rem; }
  .modes { display: flex; gap: 0.3rem; flex-wrap: wrap; }
  .m {
    background: none; border: 1px solid transparent; color: var(--mut2); font: inherit;
    font-size: 0.8rem; font-weight: 700; padding: 0.28rem 0.72rem; border-radius: 999px;
    cursor: pointer; transition: background 0.12s, color 0.12s, border-color 0.12s;
  }
  .m:hover { color: var(--ink); background: var(--hover); }
  .m.on { color: var(--amber); background: rgb(247 153 27 / 13%); border-color: rgb(247 153 27 / 30%); }
  .hint {
    margin: 0; font-size: 0.78rem; line-height: 1.4; color: var(--mut);
  }
  textarea {
    width: 100%; background: transparent; border: 0; color: var(--ink);
    font: inherit; font-size: 1.02rem; line-height: 1.48; resize: none; padding: 0.2rem 0; outline: none;
  }
  textarea::placeholder { color: var(--mut2); }
  .bar {
    display: flex; align-items: center; justify-content: space-between; gap: 0.75rem; flex-wrap: wrap;
    padding-top: 0.4rem;
  }
  .opts { display: flex; align-items: center; gap: 0.5rem; flex-wrap: wrap; }
  .tag-in {
    display: inline-flex; align-items: center; gap: 0.3rem;
    background: var(--surf); border: 1px solid var(--line); border-radius: 999px;
    padding: 0.22rem 0.65rem; font-size: 0.76rem; color: var(--mut);
  }
  .lbl { color: var(--mut2); font-weight: 600; }
  .tag-in input {
    width: 68px; background: none; border: 0; color: var(--ink);
    font: inherit; font-size: 0.76rem; font-weight: 600; outline: none;
  }
  .tag-in.num input { width: 52px; font-variant-numeric: tabular-nums; }
  .post {
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.46rem 1.15rem; font: inherit; font-size: 0.84rem; font-weight: 800;
    cursor: pointer; white-space: nowrap; transition: filter 0.14s;
  }
  .post:hover:not(:disabled) { filter: brightness(1.08); }
  .post:disabled { opacity: 0.38; cursor: default; }
</style>
