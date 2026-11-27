<script lang="ts">
  import type { ReputationState, ReputationVector6D } from '../lib/types'
  import { fmtBpPct, shortHash } from '../lib/format'

  interface Props {
    reputation?: ReputationState
  }

  let { reputation }: Props = $props()

  let selectedOnt = $state<string>('')
  let selectedPub = $state<string>('')

  const ontologies = $derived(reputation?.ontologies?.length ? reputation.ontologies : ['global'])
  const activeOnt = $derived(
    selectedOnt && ontologies.includes(selectedOnt)
      ? selectedOnt
      : (reputation?.ontologyId || ontologies[0] || 'global')
  )

  const vectorsForOnt = $derived(
    (reputation?.vectors ?? []).filter((v) => !activeOnt || activeOnt === 'global' || v.ontologyId === activeOnt || v.ontologyId === 'global')
  )

  const activeVector: ReputationVector6D | null = $derived(
    vectorsForOnt.find((v) => v.subjectPub === selectedPub) ?? vectorsForOnt[0] ?? null
  )

  const trustedSet = $derived(new Set(reputation?.trustedSellers ?? []))
  const prioritySet = $derived(new Set(reputation?.priorityBuyers ?? []))
  const untrustedSet = $derived(new Set(reputation?.sybilOrUntrusted ?? []))

  const axes = [
    { key: 'accBp', label: 'Accuracy' },
    { key: 'consBp', label: 'Consistency' },
    { key: 'dispBp', label: 'No-Dispute' },
    { key: 'predBp', label: 'Predictability' },
    { key: 'collabBp', label: 'Collab' },
    { key: 'provBp', label: 'Provenance' }
  ] as const

  const cx = 110
  const cy = 104
  const maxR = 68

  function polar(idx: number, ratio: number): [number, number] {
    const angle = (-Math.PI / 2) + (idx * Math.PI * 2) / 6
    return [cx + Math.cos(angle) * maxR * ratio, cy + Math.sin(angle) * maxR * ratio]
  }

  function hexPoints(ratio: number): string {
    return axes.map((_, i) => polar(i, ratio).map((n) => n.toFixed(1)).join(',')).join(' ')
  }

  const dataPoints = $derived.by(() => {
    if (!activeVector) return hexPoints(0.08)
    return axes
      .map((a, i) => {
        const bp = activeVector[a.key] ?? 0
        const ratio = Math.max(0.06, Math.min(1, bp / 10000))
        return polar(i, ratio).map((n) => n.toFixed(1)).join(',')
      })
      .join(' ')
  })

  const labelCoords = axes.map((a, i) => {
    const [x, y] = polar(i, 1.24)
    return { ...a, x, y }
  })

  const advisoryNote = $derived(
    reputation?.advisoryNote ||
      'Advisory reputation: derived from verified bilateral nektarrep1.* deal attestations; proves signed history, not objective truth.'
  )
</script>

<section class="rep-card" aria-label="6D Domain Reputation">
  <header class="card-head">
    <div>
      <h3>6D Domain Reputation</h3>
      <p class="sub">Personalized EigenTrust · Mangle Datalog Trust</p>
    </div>
    <div class="filters">
      <select class="sel" bind:value={selectedOnt} aria-label="Ontology domain">
        {#each ontologies as ont}
          <option value={ont}>{ont}</option>
        {/each}
      </select>
      {#if vectorsForOnt.length > 0}
        <select class="sel" bind:value={selectedPub} aria-label="Peer identity">
          {#each vectorsForOnt as v}
            <option value={v.subjectPub}>{shortHash(v.subjectPub, 8, 6)}</option>
          {/each}
        </select>
      {/if}
    </div>
  </header>

  <div class="rep-body">
    <div class="svg-wrap">
      <svg viewBox="0 0 220 210" class="radar" role="img" aria-label="Six-dimensional reputation radar chart">
        {#each [0.25, 0.5, 0.75, 1] as r}
          <polygon points={hexPoints(r)} fill="none" stroke="rgba(255,255,255,0.08)" stroke-width="1" />
        {/each}
        {#each axes as _, i}
          {@const [ex, ey] = polar(i, 1)}
          <line x1={cx} y1={cy} x2={ex} y2={ey} stroke="rgba(255,255,255,0.08)" stroke-width="1" />
        {/each}

        <polygon
          points={dataPoints}
          fill="rgba(247, 153, 27, 0.24)"
          stroke="#f7991b"
          stroke-width="2"
          stroke-linejoin="round"
        />

        {#each labelCoords as lbl}
          <text
            x={lbl.x}
            y={lbl.y}
            text-anchor="middle"
            dominant-baseline="central"
            class="axis-lbl"
          >
            {lbl.label}
          </text>
        {/each}
      </svg>
    </div>

    <div class="rep-meta">
      {#if activeVector}
        <div class="peer-id" title={activeVector.subjectPub}>
          <span class="mono">{shortHash(activeVector.subjectPub, 10, 8)}</span>
          <span class="ont-tag">{activeVector.ontologyId}</span>
        </div>

        <div class="badges">
          {#if trustedSet.has(activeVector.subjectPub)}
            <span class="b ok">Trusted Seller</span>
          {/if}
          {#if prioritySet.has(activeVector.subjectPub)}
            <span class="b amber">Priority Buyer</span>
          {/if}
          {#if untrustedSet.has(activeVector.subjectPub)}
            <span class="b bad">Untrusted / Sybil</span>
          {/if}
          {#if !trustedSet.has(activeVector.subjectPub) && !prioritySet.has(activeVector.subjectPub) && !untrustedSet.has(activeVector.subjectPub)}
            <span class="b mut">Observed Peer</span>
          {/if}
        </div>

        <div class="scores-grid">
          <div class="sc"><span>Composite</span><strong>{fmtBpPct(activeVector.compositeBp)}</strong></div>
          <div class="sc"><span>EigenTrust</span><strong>{fmtBpPct(activeVector.eigenTrustBp)}</strong></div>
          <div class="sc"><span>Accuracy</span><strong>{fmtBpPct(activeVector.accBp)}</strong></div>
          <div class="sc"><span>Consistency</span><strong>{fmtBpPct(activeVector.consBp)}</strong></div>
          <div class="sc"><span>No-Dispute</span><strong>{fmtBpPct(activeVector.dispBp)}</strong></div>
          <div class="sc"><span>Collab</span><strong>{fmtBpPct(activeVector.collabBp)}</strong></div>
        </div>

        <div class="counts">
          {activeVector.directDeals} direct deals · {activeVector.attestCount} verified attestations
        </div>
      {:else}
        <div class="empty">
          No peer 6D reputation vectors recorded yet. Complete a deal or run a task outcome hook to populate bilateral <code>nektarrep1.*</code> scores.
        </div>
      {/if}
    </div>
  </div>

  <footer class="honesty-note">
    {advisoryNote}
  </footer>
</section>

<style>
  .rep-card {
    background: var(--surf);
    border: 1px solid var(--line);
    border-radius: 14px;
    padding: 0.9rem 1rem;
    display: flex;
    flex-direction: column;
    gap: 0.7rem;
  }

  .card-head {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 0.6rem;
    flex-wrap: wrap;
  }

  h3 {
    margin: 0;
    font-size: 0.9rem;
    font-weight: 800;
    color: var(--ink);
  }

  .sub {
    margin: 0.14rem 0 0;
    font-size: 0.72rem;
    color: var(--mut2);
  }

  .filters {
    display: flex;
    gap: 0.4rem;
    flex-wrap: wrap;
  }

  .sel {
    background: var(--bg);
    color: var(--ink);
    border: 1px solid var(--line);
    border-radius: 8px;
    padding: 0.24rem 0.5rem;
    font: inherit;
    font-size: 0.72rem;
  }

  .rep-body {
    display: grid;
    grid-template-columns: 185px minmax(0, 1fr);
    gap: 0.85rem;
    align-items: center;
  }

  .svg-wrap {
    display: flex;
    justify-content: center;
  }

  .radar {
    width: 185px;
    height: 176px;
    overflow: visible;
  }

  .axis-lbl {
    fill: var(--mut);
    font-size: 8.5px;
    font-weight: 600;
  }

  .rep-meta {
    display: flex;
    flex-direction: column;
    gap: 0.45rem;
    min-width: 0;
  }

  .peer-id {
    display: flex;
    align-items: center;
    gap: 0.45rem;
    flex-wrap: wrap;
  }

  .mono {
    font-family: ui-monospace, 'Cascadia Mono', Consolas, monospace;
    font-size: 0.74rem;
    color: var(--ink);
  }

  .ont-tag {
    font-size: 0.66rem;
    font-weight: 700;
    padding: 0.08rem 0.42rem;
    border-radius: 999px;
    background: var(--bg);
    border: 1px solid var(--line);
    color: var(--mut);
  }

  .badges {
    display: flex;
    gap: 0.35rem;
    flex-wrap: wrap;
  }

  .b {
    font-size: 0.65rem;
    font-weight: 800;
    padding: 0.12rem 0.48rem;
    border-radius: 999px;
  }

  .b.ok { background: rgb(76 194 107 / 14%); color: var(--ok); }
  .b.amber { background: rgb(247 153 27 / 14%); color: var(--amber); }
  .b.bad { background: rgb(235 87 87 / 16%); color: var(--bad); }
  .b.mut { background: var(--bg); color: var(--mut); border: 1px solid var(--line); }

  .scores-grid {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 0.35rem;
  }

  .sc {
    background: var(--bg);
    border: 1px solid var(--line);
    border-radius: 8px;
    padding: 0.3rem 0.45rem;
    display: flex;
    flex-direction: column;
    gap: 0.08rem;
  }

  .sc span {
    font-size: 0.64rem;
    color: var(--mut2);
  }

  .sc strong {
    font-family: ui-monospace, monospace;
    font-size: 0.78rem;
    color: var(--ink);
  }

  .counts {
    font-size: 0.7rem;
    color: var(--mut2);
  }

  .empty {
    font-size: 0.76rem;
    line-height: 1.45;
    color: var(--mut);
  }

  .honesty-note {
    font-size: 0.68rem;
    line-height: 1.4;
    color: var(--mut2);
    border-top: 1px solid var(--line);
    padding-top: 0.55rem;
  }

  @media (max-width: 640px) {
    .rep-body { grid-template-columns: 1fr; }
  }
</style>
