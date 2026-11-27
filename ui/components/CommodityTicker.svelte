<script lang="ts">
  import type { CommodityTickerResponse, CommodityTickerRow } from '../lib/types'
  import { fmtSats, fmtSatsPerTriple, shortHash } from '../lib/format'

  interface Props {
    ticker?: CommodityTickerResponse
    onPickBuy: (domain: string, priceSats: number, hint?: string) => void
  }

  let { ticker, onPickBuy }: Props = $props()

  let modeFilter = $state<'all' | 'spot' | 'per_query' | 'subscription'>('all')
  let gradeFilter = $state<'all' | 'S' | 'A' | 'B' | 'C'>('all')
  let expanded = $state(false)

  const allRows = $derived(ticker?.rows ?? [])
  const filteredRows = $derived(
    allRows.filter((r) => {
      if (modeFilter !== 'all' && r.pricingMode !== modeFilter) return false
      if (gradeFilter !== 'all' && r.grade !== gradeFilter) return false
      return true
    })
  )
  const visibleRows = $derived(expanded ? filteredRows : filteredRows.slice(0, 6))

  function modeLabel(m: string): string {
    if (m === 'per_query') return 'per_query'
    if (m === 'subscription') return 'sub'
    return 'spot'
  }

  function ontLabel(r: CommodityTickerRow): string {
    if (!r.ontologyId || r.ontologyId === 'unpinned') {
      return r.domains?.[0] ? `#${r.domains[0]}` : 'unpinned'
    }
    return shortHash(r.ontologyId, 8, 4)
  }

  function pickRow(r: CommodityTickerRow) {
    const dom = r.domains?.[0] || r.sampleShards?.[0] || 'ops'
    const ask = r.minAskSats > 0 ? r.minAskSats : Math.max(10, Math.ceil(r.minSatsPer1kUnits / 1000))
    onPickBuy(dom, ask, `Grade ${r.grade} ${r.pricingMode} facts`)
  }
</script>

<section class="nmu-ticker" aria-label="NMU-1 Commodity Ticker">
  <div class="t-head">
    <div class="t-title">
      <span class="t-badge">NMU-1</span>
      <strong>Knowledge Commodity Ticker</strong>
      <span class="t-sub">sats / verified triple</span>
    </div>

    {#if allRows.length > 0}
      <div class="t-filters">
        <div class="seg" role="group" aria-label="Grade filter">
          {#each ['all', 'S', 'A', 'B', 'C'] as g}
            <button
              class="seg-btn"
              class:on={gradeFilter === g}
              onclick={() => (gradeFilter = g as typeof gradeFilter)}
            >
              {g === 'all' ? 'All' : g}
            </button>
          {/each}
        </div>
        <div class="seg" role="group" aria-label="Mode filter">
          {#each [
            ['all', 'All'],
            ['spot', 'Spot'],
            ['per_query', 'Query'],
            ['subscription', 'Feed']
          ] as [k, lbl]}
            <button
              class="seg-btn"
              class:on={modeFilter === k}
              onclick={() => (modeFilter = k as typeof modeFilter)}
            >
              {lbl}
            </button>
          {/each}
        </div>
      </div>
    {/if}
  </div>

  {#if allRows.length === 0}
    <div class="t-empty">
      No standardized NMU-1 quotes live yet — list a shard (<code>spot</code>, <code>per_query</code>, or <code>subscription</code>) or join a Hub to populate apples-to-apples <code>sats/triple</code> price discovery.
    </div>
  {:else if filteredRows.length === 0}
    <div class="t-empty">No quotes match the active grade/mode filter.</div>
  {:else}
    <div class="t-table-wrap">
      <table class="t-table">
        <thead>
          <tr>
            <th>Grade</th>
            <th>Ontology / Domain</th>
            <th>Mode</th>
            <th class="num">Min s/triple</th>
            <th class="num">Med s/triple</th>
            <th class="num">Min Ask</th>
            <th class="num">Units</th>
            <th>SLA</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {#each visibleRows as r}
            <tr>
              <td>
                <span class="g-pill g-{r.grade.toLowerCase()}" title="NMU-1 Grade {r.grade}">{r.grade}</span>
              </td>
              <td class="ont-cell">
                <span class="ont-id">{ontLabel(r)}</span>
                {#if r.domains && r.domains.length > 0 && r.ontologyId !== 'unpinned'}
                  <span class="dom-tag">#{r.domains[0]}</span>
                {/if}
              </td>
              <td>
                <span class="m-pill m-{r.pricingMode}">{modeLabel(r.pricingMode)}</span>
              </td>
              <td class="num mono hi">{r.minSatsPerTriple || fmtSatsPerTriple(r.minSatsPer1kUnits)}</td>
              <td class="num mono">{r.medianSatsPerTriple || fmtSatsPerTriple(r.medianSatsPer1kUnits)}</td>
              <td class="num mono">{fmtSats(r.minAskSats)} <span class="u">sats</span></td>
              <td class="num mono">{fmtSats(r.totalUnitsAvailable)}</td>
              <td class="sla-cell">
                {#if r.bondedSellers > 0}
                  <span class="sla-tag bond" title="{r.bondedSellers} bonded seller(s)">Bond</span>
                {/if}
                {#if r.liveFeeds > 0}
                  <span class="sla-tag feed" title="{r.liveFeeds} live supersession SSE feed(s)">Live</span>
                {/if}
                {#if r.bondedSellers === 0 && r.liveFeeds === 0}
                  <span class="dim">—</span>
                {/if}
              </td>
              <td class="act-cell">
                <button class="pick-btn" onclick={() => pickRow(r)}>Buy</button>
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>

    {#if filteredRows.length > 6}
      <div class="t-more">
        <button class="more-btn" onclick={() => (expanded = !expanded)}>
          {expanded ? 'Show fewer' : `Show all ${filteredRows.length} rows`}
        </button>
      </div>
    {/if}
  {/if}

  {#if ticker?.advisoryNote}
    <p class="t-note">{ticker.advisoryNote}</p>
  {/if}
</section>

<style>
  .nmu-ticker {
    padding: 0.7rem 1.25rem 0.65rem;
    border-bottom: 1px solid var(--line);
    background: var(--bg);
    display: flex;
    flex-direction: column;
    gap: 0.45rem;
  }

  .t-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.65rem;
    flex-wrap: wrap;
  }

  .t-title {
    display: flex;
    align-items: center;
    gap: 0.45rem;
    font-size: 0.82rem;
  }

  .t-badge {
    font-family: ui-monospace, monospace;
    font-size: 0.66rem;
    font-weight: 800;
    letter-spacing: 0.04em;
    color: #120e0c;
    background: var(--amber);
    padding: 0.1rem 0.42rem;
    border-radius: 5px;
  }

  .t-sub {
    color: var(--mut);
    font-size: 0.73rem;
  }

  .t-filters {
    display: flex;
    align-items: center;
    gap: 0.45rem;
    flex-wrap: wrap;
  }

  .seg {
    display: inline-flex;
    background: var(--surf);
    border: 1px solid var(--line);
    border-radius: 999px;
    padding: 2px;
  }

  .seg-btn {
    border: 0;
    background: none;
    color: var(--mut);
    font: inherit;
    font-size: 0.68rem;
    font-weight: 700;
    padding: 0.12rem 0.46rem;
    border-radius: 999px;
    cursor: pointer;
  }
  .seg-btn.on {
    background: var(--amber);
    color: #120e0c;
  }

  .t-empty {
    font-size: 0.75rem;
    color: var(--mut);
    padding: 0.2rem 0;
  }
  .t-empty code {
    font-family: ui-monospace, monospace;
    color: var(--ink);
    font-size: 0.72rem;
  }

  .t-table-wrap {
    overflow-x: auto;
  }

  .t-table {
    width: 100%;
    border-collapse: collapse;
    font-size: 0.76rem;
  }

  .t-table th {
    text-align: left;
    color: var(--mut2);
    font-weight: 700;
    font-size: 0.68rem;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    padding: 0.26rem 0.45rem;
    border-bottom: 1px solid var(--line);
    white-space: nowrap;
  }

  .t-table td {
    padding: 0.34rem 0.45rem;
    border-bottom: 1px solid rgb(255 255 255 / 4%);
    white-space: nowrap;
    vertical-align: middle;
  }

  .t-table tbody tr:hover {
    background: var(--hover);
  }

  .num { text-align: right; }
  .mono {
    font-family: ui-monospace, monospace;
    font-variant-numeric: tabular-nums;
  }
  .hi { color: var(--ink); font-weight: 800; }
  .u { color: var(--mut); font-size: 0.68rem; }
  .dim { color: var(--mut2); }

  .g-pill {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    min-width: 20px;
    height: 18px;
    border-radius: 5px;
    font-family: ui-monospace, monospace;
    font-size: 0.7rem;
    font-weight: 900;
    padding: 0 0.3rem;
  }
  .g-s { background: rgb(247 153 27 / 22%); color: var(--amber); border: 1px solid rgb(247 153 27 / 55%); }
  .g-a { background: rgb(76 194 107 / 18%); color: var(--ok); border: 1px solid rgb(76 194 107 / 45%); }
  .g-b { background: rgb(120 170 255 / 16%); color: #8ec0ff; border: 1px solid rgb(120 170 255 / 38%); }
  .g-c { background: rgb(255 255 255 / 6%); color: var(--mut); border: 1px solid var(--line); }

  .ont-cell { display: flex; align-items: center; gap: 0.35rem; }
  .ont-id { font-family: ui-monospace, monospace; color: var(--ink); font-weight: 600; }
  .dom-tag { font-size: 0.7rem; color: var(--mut); }

  .m-pill {
    font-family: ui-monospace, monospace;
    font-size: 0.67rem;
    font-weight: 700;
    padding: 0.1rem 0.42rem;
    border-radius: 999px;
    border: 1px solid var(--line);
    color: var(--mut);
  }
  .m-spot { color: var(--ink); border-color: var(--line2); }
  .m-per_query { color: #8ec0ff; border-color: rgb(120 170 255 / 35%); }
  .m-subscription { color: var(--amber); border-color: rgb(247 153 27 / 40%); }

  .sla-cell { display: flex; align-items: center; gap: 0.25rem; }
  .sla-tag {
    font-size: 0.64rem;
    font-weight: 800;
    padding: 0.08rem 0.34rem;
    border-radius: 4px;
    text-transform: uppercase;
  }
  .sla-tag.bond { background: rgb(76 194 107 / 14%); color: var(--ok); }
  .sla-tag.feed { background: rgb(247 153 27 / 14%); color: var(--amber); }

  .act-cell { text-align: right; }
  .pick-btn {
    border: 1px solid rgb(76 194 107 / 40%);
    background: transparent;
    color: var(--ok);
    border-radius: 999px;
    padding: 0.14rem 0.55rem;
    font: inherit;
    font-size: 0.7rem;
    font-weight: 700;
    cursor: pointer;
  }
  .pick-btn:hover {
    background: var(--ok);
    color: #08120a;
  }

  .t-more { display: flex; justify-content: center; }
  .more-btn {
    background: none;
    border: 1px solid var(--line);
    color: var(--mut);
    border-radius: 999px;
    padding: 0.16rem 0.65rem;
    font: inherit;
    font-size: 0.7rem;
    cursor: pointer;
  }
  .more-btn:hover { color: var(--ink); border-color: var(--line2); }

  .t-note {
    margin: 0;
    font-size: 0.68rem;
    color: var(--mut2);
    line-height: 1.35;
  }
</style>
