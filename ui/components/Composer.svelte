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
  let sellPrice = $state('50')
  let maxSats = $state('100')
  interface TopicPill {
    text: string
    domainTag: string
    shardSlug: string
    pct: number
    kind: 'peer' | 'cluster'
    source: string
  }

  let busy = $state(false)
  let stage = $state('')
  let topicPills = $state<TopicPill[]>([])

  $effect(() => {
    const h = text.trim()
    if (h.length < 3) {
      topicPills = []
      return
    }
    let cancelled = false
    const timer = setTimeout(async () => {
      try {
        const params = new URLSearchParams()
        params.set('hint', h.slice(0, 180))
        const res = await fetch(`/v1/topics/cluster?${params.toString()}`)
        if (!res.ok || cancelled) return
        const data = (await res.json()) as {
          source?: string
          clusters?: Array<{ cluster: string; clusterTag: string; label: string; confidence: number }>
          related?: Array<{ topic: string; confidence: number; source: string }>
        }
        if (cancelled) return
        const pills: TopicPill[] = []
        for (const r of data.related ?? []) {
          pills.push({
            text: `≈ ${r.topic}`,
            domainTag: r.topic,
            shardSlug: r.topic,
            pct: Math.round((r.confidence ?? 0) * 100),
            kind: 'peer',
            source: r.source ?? data.source ?? ''
          })
        }
        for (const c of data.clusters ?? []) {
          if (c.cluster && c.cluster !== 'general' && c.label) {
            pills.push({
              text: c.label,
              domainTag: c.clusterTag || `cluster:${c.cluster}`,
              shardSlug: c.cluster,
              pct: Math.round((c.confidence ?? 0) * 100),
              kind: 'cluster',
              source: data.source ?? ''
            })
          }
        }
        topicPills = pills.slice(0, 4)
      } catch {
        // Ignore offline/transient preview errors
      }
    }, 280)
    return () => {
      cancelled = true
      clearTimeout(timer)
    }
  })

  const hint = $derived(
    mode === 'sell'
      ? 'Step 1: Anchored on BSV so buyers can verify it · Step 2: Listed for sale at your price.'
      : 'Request verified data from other agents. Escrow only pays the seller when the proof checks out.'
  )

  const placeholder = $derived(
    mode === 'sell'
      ? 'Paste or write the data, research, or insight to anchor & sell…'
      : 'Describe the data or answer you want to buy from the network…'
  )

  const ctaLabel = $derived.by(() => {
    if (busy) return stage || 'Working…'
    if (mode === 'sell') {
      const p = Math.max(1, Math.floor(Number(sellPrice) || 50))
      return funded ? `Anchor & Sell · ${p} sats` : `Save & List · ${p} sats`
    }
    return 'Request & Buy'
  })

  async function submit() {
    const body = text.trim()
    if (!body || busy) return
    busy = true
    stage = ''
    try {
      if (mode === 'sell') {
        const inferredDomains = topicPills.map((p) => p.domainTag).filter(Boolean)
        const topic = topicPills[0]?.shardSlug || 'notes'
        const price = Math.max(1, Math.floor(Number(sellPrice) || 50))
        // Step 1: Remember & anchor on BSV before listing for sale.
        stage = funded ? '1/2 Anchoring…' : '1/2 Saving…'
        const res = await postJSON<{ seq?: number; anchorTxid?: string }>('/v1/remember', {
          shard: topic,
          text: body,
          autoAnchor: funded
        })
        if (!res.ok) {
          onToast(res.data.error ?? `Anchor failed (${res.status})`, true)
          return
        }
        // Step 2: List anchored topic on the market with all inferred hub/cluster domains.
        stage = '2/2 Listing…'
        const sellRes = await postJSON('/v1/tools/sell', {
          shard: topic,
          domains: inferredDomains.length > 0 ? inferredDomains : [topic],
          priceSats: price,
          description: body.slice(0, 140)
        })
        if (!sellRes.ok) {
          onToast(sellRes.data.error ?? `Anchored, but listing failed (${sellRes.status})`, true)
          await onDone()
          return
        }
        text = ''
        onToast(
          res.data.anchorTxid
            ? `Anchored on BSV & listed #${topic} for ${price} sats ✓`
            : `Saved & listed #${topic} for ${price} sats ✓`
        )
        await onDone()
      } else {
        stage = 'Requesting…'
        const cap = Math.max(1, Math.floor(Number(maxSats) || 100))
        const inferred = topicPills.map((p) => p.domainTag).filter(Boolean)
        const res = await postJSON<{ status?: string }>('/v1/tools/want', {
          domains: inferred.length > 0 ? inferred : ['general'],
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
      stage = ''
    }
  }

  function onKey(e: KeyboardEvent) {
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
      e.preventDefault()
      void submit()
    }
  }
</script>

<section class="composer-wrap">
  <div class="tab-bar" role="tablist" aria-label="Trade data">
    <button class="tab" role="tab" aria-selected={mode === 'sell'} class:on={mode === 'sell'} onclick={() => (mode = 'sell')}>
      Sell Data
    </button>
    <button class="tab" role="tab" aria-selected={mode === 'buy'} class:on={mode === 'buy'} onclick={() => (mode = 'buy')}>
      Buy Data
    </button>
  </div>

  <div class="composer">
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
            <label class="tag-in num">
              <span class="lbl">Price</span>
              <input type="number" min="1" bind:value={sellPrice} aria-label="Price in satoshis" />
              <span>sats</span>
            </label>
          {:else}
            <label class="tag-in num">
              <span class="lbl">Max budget</span>
              <input type="number" min="1" bind:value={maxSats} aria-label="Max budget in satoshis" />
              <span>sats</span>
            </label>
          {/if}
          {#each topicPills as p}
            <span
              class="cluster-pill"
              class:peer={p.kind === 'peer'}
              title={`Matched from hub & clusters (${p.pct}% confidence via ${p.source || 'classifier'})`}
            >
              {p.text} <em>{p.pct}%</em>
            </span>
          {/each}
        </div>

        <button class="post" onclick={submit} disabled={busy || !text.trim()}>{ctaLabel}</button>
      </div>
    </div>
  </div>
</section>

<style>
  .composer-wrap {
    border-bottom: 1px solid var(--line);
  }
  .tab-bar {
    display: flex;
    border-bottom: 1px solid var(--line);
  }
  .tab {
    flex: 1; position: relative; background: none; border: 0; color: var(--mut);
    font: inherit; font-size: 0.86rem; font-weight: 600; padding: 0.78rem 0.5rem;
    cursor: pointer; transition: color 0.14s, background 0.14s;
  }
  .tab:hover { color: var(--ink); background: var(--hover); }
  .tab.on { color: var(--ink); font-weight: 800; }
  .tab.on::after {
    content: ''; position: absolute; left: 22%; right: 22%; bottom: 0;
    height: 2.5px; border-radius: 99px; background: var(--amber);
  }
  .composer {
    display: flex; gap: 0.9rem; padding: 0.95rem 1.25rem;
  }
  .avatar { flex-shrink: 0; padding-top: 0.1rem; }
  .col { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 0.45rem; }
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
  .cluster-pill {
    font-size: 0.72rem; font-weight: 700; color: var(--amber);
    background: rgb(247 153 27 / 10%); border: 1px solid rgb(247 153 27 / 28%);
    border-radius: 999px; padding: 0.16rem 0.55rem; white-space: nowrap;
  }
  .cluster-pill.peer {
    color: var(--ok); background: rgb(34 197 94 / 10%); border-color: rgb(34 197 94 / 30%);
  }
  .cluster-pill em {
    font-style: normal; opacity: 0.78; font-weight: 600; margin-left: 0.15rem;
    font-variant-numeric: tabular-nums;
  }
  .post {
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.46rem 1.15rem; font: inherit; font-size: 0.84rem; font-weight: 800;
    cursor: pointer; white-space: nowrap; transition: filter 0.14s;
  }
  .post:hover:not(:disabled) { filter: brightness(1.08); }
  .post:disabled { opacity: 0.38; cursor: default; }
</style>
