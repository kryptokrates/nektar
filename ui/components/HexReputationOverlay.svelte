<svelte:options namespace="svg" />

<script lang="ts">
  import type { ReputationVector6D } from '../lib/types'
  import { hexVertex, REP_AXES, repPolygonPoints } from '../lib/reputationHex'

  interface Props {
    radius: number
    vector?: ReputationVector6D | null
    trusted?: boolean
    untrusted?: boolean
    showPips?: boolean
    showSpokes?: boolean
    cx?: number
    cy?: number
  }

  let {
    radius,
    vector = null,
    trusted = false,
    untrusted = false,
    showPips = true,
    showSpokes = true,
    cx = 0,
    cy = 0
  }: Props = $props()

  const strokeColor = $derived(untrusted ? '#e5534b' : trusted ? '#4cc26b' : '#f7991b')
  const polyPts = $derived(repPolygonPoints(vector, radius, cx, cy))
  const hasData = $derived(vector !== null && vector !== undefined)
</script>

<g pointer-events="none">
  {#if showSpokes}
    {#each REP_AXES as _, i}
      {@const [ex, ey] = hexVertex(i, radius, cx, cy)}
      <line x1={cx} y1={cy} x2={ex} y2={ey} stroke={strokeColor} stroke-opacity="0.09" stroke-width="0.9" />
    {/each}
  {/if}

  <polygon
    points={polyPts}
    fill={strokeColor}
    fill-opacity={hasData ? 0.16 : 0.05}
    stroke={strokeColor}
    stroke-opacity={hasData ? 0.62 : 0.24}
    stroke-width={radius >= 35 ? 1.3 : 1.0}
    stroke-linejoin="round"
  />

  {#if showPips}
    {#each REP_AXES as a, i}
      {@const bp = vector ? (vector[a.key] ?? 0) : 0}
      {@const ratio = hasData ? Math.max(0.18, Math.min(1, bp / 10000)) : 0.28}
      {@const [px, py] = hexVertex(i, radius * ratio, cx, cy)}
      <circle
        cx={px}
        cy={py}
        r={radius >= 35 ? 2.1 : 1.35}
        fill={strokeColor}
        opacity={hasData ? 0.45 + ratio * 0.55 : 0.28}
      />
    {/each}
  {/if}
</g>
