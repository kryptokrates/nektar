<script lang="ts">
  import type { BountyInfo, MarketTapeEvent, UnmatchedWant } from '../lib/types'
  import { postJSON } from '../lib/api'
  import { agoMs, fmtSats } from '../lib/format'
  import { fetchTopicPills, type TopicPill } from './MarketHomeHelpers'

  interface Props {
    buyText?: string
    buyDomain?: string
    buyMaxSats?: string
    buyInputEl?: HTMLTextAreaElement | null
    bounties: BountyInfo[]
    demand: UnmatchedWant[]
    liveWants: MarketTapeEvent[]
    onRefresh: () => Promise<void>
    onToast: (msg: string, isErr?: boolean) => void
    onLocalPulse: (event: string, tape?: MarketTapeEvent) => void
    onPickSell: (domain: string, rewardSats: number) => void
  }

  let {
    buyText = $bindable(''),
    buyDomain = $bindable(''),
    buyMaxSats = $bindable('100'),
    buyInputEl = $bindable(null),
    bounties,
    demand,
    liveWants,
    onRefresh,
    onToast,
    onLocalPulse,
    onPickSell
  }: Props = $props()

  let buyBusy = $state(false)
  let buyPills = $state<TopicPill[]>([])

  $effect(() => {
    const h = buyText.trim()
    if (h.length < 3) {
      buyPills = []
      return
    }
    let cancelled = false
    const t = setTimeout(async () => {
      try {
        const res = await fetchTopicPills(h)
        if (!cancelled) buyPills = res
      } catch {}
    }, 280)
    return () => {
      cancelled = true
      clearTimeout(t)
    }
  })

  async function submitBuy() {
    const body = buyText.trim()
    if (!body || buyBusy) return
    buyBusy = true
    try {
      const cap = Math.max(1, Math.floor(Number(buyMaxSats) || 100))
      const inferred = buyPills.map((p) => p.domainTag).filter(Boolean)
      const cleanDom = buyDomain.trim().replace(/^#/, '')
      const domains = cleanDom ? [cleanDom, ...inferred.filter((d) => d !== cleanDom)] : inferred.length > 0 ? inferred : ['general']

      onLocalPulse('tool:want', {
        id: `local-buy-${Date.now()}`,
        kind: 'want',
        side: 'buy',
        domains,
        summary: body.slice(0, 120),
        sats: cap,
        status: 'open',
        isSelf: true,
        at: Date.now()
      })

      const res = await postJSON<{ status?: string; priceSats?: number; standingOrderId?: string }>('/v1/tools/want', {
        domains,
        question: body,
        maxSats: cap
      })
      if (!res.ok) {
        onToast(res.data.error ?? `Buy failed (${res.status})`, true)
        return
      }
      const st = res.data.status ?? ''
      if (st === 'settled') {
        buyText = ''
        buyDomain = ''
        onLocalPulse('deal_settled', {
          id: `local-clear-${Date.now()}`,
          kind: 'deal',
          side: 'clear',
          domains,
          summary: body.slice(0, 120),
          sats: res.data.priceSats ?? cap,
          status: 'settled',
          isSelf: true,
          at: Date.now()
        })
        onToast('Bought & verified ✓')
      } else if (st === 'approval_pending') {
        buyText = ''
        buyDomain = ''
        onToast(res.data.error ?? 'Waiting for operator approval — review in the Approve bar above or Settings → Policy')
      } else if (st === 'watching') {
        buyText = ''
        buyDomain = ''
        onToast('Standing Order active — watching market for verified offers ✓')
      } else if (st === 'expired') {
        onToast(res.data.error ?? 'No matching offers received yet — widen topic or max sats and retry', true)
      } else if (st === 'failed') {
        onToast(res.data.error ?? 'Buy request failed — check policy, funding, or hub connectivity', true)
      } else {
        buyText = ''
        buyDomain = ''
        onToast('Buy request posted ✓')
      }
      await onRefresh()
    } catch (e) {
      onToast(String(e), true)
    } finally {
      buyBusy = false
    }
  }
</script>

<!-- LEFT: BUY -->
<div class="col col-buy">
  <div class="col-head">
    <span class="side-dot buy"></span>
    <h2>Buy</h2>
  </div>

  <div class="order-box buy-box">
    <textarea
      bind:this={buyInputEl}
      bind:value={buyText}
      rows="2"
      placeholder="What verified data do you want to buy?"
      disabled={buyBusy}
      onkeydown={(e) => {
        if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
          e.preventDefault()
          void submitBuy()
        }
      }}
    ></textarea>

    <div class="order-bar">
      <div class="order-opts">
        <label class="chip-in">
          <input class="num-in" type="number" min="1" bind:value={buyMaxSats} aria-label="Max satoshis" />
          <span>sats</span>
        </label>
        <label class="chip-in topic-in">
          <span>#</span>
          <input bind:value={buyDomain} placeholder="topic" aria-label="Topic" />
        </label>
        {#each buyPills as p}
          <span class="auto-pill buy">{p.text}</span>
        {/each}
      </div>
      <button class="act-btn buy" onclick={submitBuy} disabled={buyBusy || !buyText.trim()}>
        {buyBusy ? '…' : 'Buy'}
      </button>
    </div>
  </div>

  {#if bounties.length || demand.length || liveWants.length}
    <div class="book-list">
      {#each bounties as b}
        <article class="book-row">
          <div class="row-top">
            <span class="tag buy">#{b.domain}</span>
            <span class="price buy">+{fmtSats(b.rewardSats)}s</span>
          </div>
          <p class="row-txt">{b.question}</p>
          <div class="row-foot">
            <button class="cross-btn to-sell" onclick={() => onPickSell(b.domain, b.rewardSats)}>Sell →</button>
          </div>
        </article>
      {/each}

      {#each demand as uw}
        <article class="book-row">
          <div class="row-top">
            <span class="tag buy">#{uw.domain}</span>
            <span class="price buy">{fmtSats(uw.maxSats)}s</span>
          </div>
          <p class="row-txt">{uw.query || 'Verified facts requested'}</p>
          <div class="row-foot">
            <span class="time">{agoMs(uw.seenAt * 1000)}</span>
            <button class="cross-btn to-sell" onclick={() => onPickSell(uw.domain, uw.maxSats)}>Sell →</button>
          </div>
        </article>
      {/each}

      {#each liveWants as w}
        {@const dom = (w.domains?.[0] || 'general').replace(/^#/, '')}
        <article class="book-row">
          <div class="row-top">
            <span class="tag buy">#{dom}</span>
            <span class="price buy">{fmtSats(w.sats)}s</span>
          </div>
          <p class="row-txt">{w.summary || 'Live buy request'}</p>
          <div class="row-foot">
            <span class="time">{agoMs(w.at)}</span>
            <button class="cross-btn to-sell" onclick={() => onPickSell(dom, w.sats)}>Sell →</button>
          </div>
        </article>
      {/each}
    </div>
  {/if}
</div>

<style>
  .col {
    padding: 1.15rem 1.25rem 1.4rem;
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
    min-width: 0;
  }
  .col-buy { border-right: 1px solid var(--line); }
  .col-head { display: flex; align-items: center; gap: 0.5rem; }
  .col-head h2 { margin: 0; font-size: 0.94rem; font-weight: 800; letter-spacing: -0.01em; }
  .side-dot { width: 8px; height: 8px; border-radius: 50%; flex-shrink: 0; }
  .side-dot.buy { background: var(--ok); }

  /* Clean Dark Order Boxes */
  .order-box {
    background: var(--surf);
    border: 1px solid var(--line);
    border-radius: 13px;
    padding: 0.8rem 0.95rem;
    display: flex;
    flex-direction: column;
    gap: 0.6rem;
    transition: border-color 0.15s;
  }
  .buy-box:focus-within { border-color: rgb(76 194 107 / 55%); }
  .order-box textarea {
    width: 100%; background: transparent; border: 0; color: var(--ink);
    font: inherit; font-size: 0.94rem; line-height: 1.45; resize: none; outline: none; padding: 0;
  }
  .order-box textarea::placeholder { color: var(--mut2); }
  .order-bar {
    display: flex; align-items: center; justify-content: space-between; gap: 0.5rem; flex-wrap: wrap;
    padding-top: 0.4rem; border-top: 1px solid rgb(255 255 255 / 4%);
  }
  .order-opts { display: flex; align-items: center; gap: 0.4rem; flex-wrap: wrap; }
  .chip-in {
    display: inline-flex; align-items: center; gap: 0.25rem;
    background: var(--bg); border: 1px solid var(--line); border-radius: 999px;
    padding: 0.22rem 0.62rem; font-size: 0.76rem; color: var(--mut);
  }
  .chip-in input {
    background: transparent; border: 0; color: var(--ink);
    font: inherit; font-size: 0.78rem; font-weight: 700; outline: none;
  }
  .chip-in .num-in {
    width: 46px;
    font-family: ui-monospace, monospace;
    font-variant-numeric: tabular-nums;
    -moz-appearance: textfield;
    appearance: textfield;
  }
  .chip-in .num-in::-webkit-outer-spin-button,
  .chip-in .num-in::-webkit-inner-spin-button {
    -webkit-appearance: none;
    margin: 0;
  }
  .chip-in.topic-in input { width: 72px; }
  .auto-pill {
    font-size: 0.7rem; font-weight: 700; border-radius: 999px; padding: 0.14rem 0.5rem; white-space: nowrap;
  }
  .auto-pill.buy { color: var(--ok); background: rgb(76 194 107 / 10%); }
  .act-btn {
    border: 0; border-radius: 999px; padding: 0.42rem 1.05rem;
    font: inherit; font-size: 0.82rem; font-weight: 800; cursor: pointer;
    transition: filter 0.14s, transform 0.1s;
  }
  .act-btn.buy { background: var(--ok); color: #08120a; }
  .act-btn:hover:not(:disabled) { filter: brightness(1.08); }
  .act-btn:disabled { opacity: 0.35; cursor: default; }

  /* Compact Order Lists */
  .book-list { display: flex; flex-direction: column; gap: 0.45rem; }
  .book-row {
    background: var(--surf); border: 1px solid var(--line); border-radius: 10px;
    padding: 0.65rem 0.85rem; display: flex; flex-direction: column; gap: 0.32rem;
    transition: background 0.12s, border-color 0.12s;
  }
  .book-row:hover { background: var(--hover); border-color: var(--line2); }
  .row-top { display: flex; align-items: center; gap: 0.5rem; flex-wrap: wrap; }
  .tag { font-size: 0.84rem; font-weight: 800; }
  .tag.buy { color: var(--ok); }
  .price {
    margin-left: auto; font-family: ui-monospace, monospace;
    font-size: 0.82rem; font-weight: 800; font-variant-numeric: tabular-nums;
  }
  .price.buy { color: var(--ok); }
  .row-txt { margin: 0; font-size: 0.84rem; line-height: 1.4; color: var(--ink); }
  .row-foot { display: flex; align-items: center; justify-content: space-between; gap: 0.5rem; }
  .cross-btn {
    margin-left: auto; border-radius: 999px; padding: 0.18rem 0.62rem;
    font: inherit; font-size: 0.72rem; font-weight: 700; cursor: pointer;
    background: transparent;
  }
  .cross-btn.to-sell { color: var(--amber); border: 1px solid rgb(247 153 27 / 35%); }
  .cross-btn.to-sell:hover { background: var(--amber); color: #120e0c; }
  .time { color: var(--mut2); font-family: ui-monospace, monospace; font-size: 0.72rem; white-space: nowrap; }

  @media (max-width: 820px) {
    .col-buy { border-right: 0; border-bottom: 1px solid var(--line); }
  }
</style>
