<script lang="ts">
  import type { ReputationVector6D } from '../lib/types'
  import { formatRepTooltip, hexPolygonPoints, hexVertex, REP_AXES } from '../lib/reputationHex'
  import HexReputationOverlay from './HexReputationOverlay.svelte'

  interface Props {
    size?: number
    vector?: ReputationVector6D | null
    trusted?: boolean
    untrusted?: boolean
    showLabels?: boolean
  }

  let { size = 20, vector = null, trusted = false, untrusted = false, showLabels = false }: Props = $props()

  const vb = $derived(showLabels ? 140 : 48)
  const mid = $derived(vb / 2)
  const outerR = $derived(showLabels ? 48 : 20)
  const innerR = $derived(showLabels ? 44 : 17)
  const frameColor = $derived(untrusted ? '#e5534b' : trusted ? '#4cc26b' : '#3a332d')
  const tip = $derived(formatRepTooltip(vector))
</script>

<svg
  width={size}
  height={size}
  viewBox={`0 0 ${vb} ${vb}`}
  class="mini-hex"
  role="img"
  aria-label={tip}
>
  <title>{tip}</title>
  <polygon
    points={hexPolygonPoints(outerR, mid, mid)}
    fill="#14110f"
    stroke={frameColor}
    stroke-width={showLabels ? 1.5 : 2}
    stroke-linejoin="round"
  />
  <HexReputationOverlay
    radius={innerR}
    cx={mid}
    cy={mid}
    {vector}
    {trusted}
    {untrusted}
    showPips={size >= 24}
    showSpokes={size >= 32}
  />
  {#if showLabels}
    {#each REP_AXES as a, i}
      {@const [lx, ly] = hexVertex(i, outerR + 13, mid, mid)}
      <text x={lx} y={ly} text-anchor="middle" dominant-baseline="central" class="hlbl">{a.short}</text>
    {/each}
  {/if}
</svg>

<style>
  .mini-hex {
    display: inline-block;
    flex-shrink: 0;
    vertical-align: middle;
    overflow: visible;
  }
  .hlbl {
    fill: var(--mut);
    font-size: 9px;
    font-weight: 700;
    font-family: system-ui, sans-serif;
  }
</style>
