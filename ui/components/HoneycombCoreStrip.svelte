<svelte:options namespace="svg" />

<script lang="ts">
  import {
    CORE_X,
    CORE_Y,
    NEKTAR_DROPLET_PATH,
    RING_CIRC,
    RING_R,
    SMOOTH_HEX_PATH,
    type StageHeartbeat,
    type StageStatItem
  } from './HoneycombModel'

  interface Props {
    uid: string
    leftArcLen: number
    rightArcLen: number
    ringMuted: boolean
    shockwaveR: number
    shockwaveOpacity: number
    coreValue: string
    coreLabel: string
    live: boolean
    heartbeats?: StageHeartbeat[]
    footerStats?: StageStatItem[]
    footerBrand: string
  }

  let {
    uid,
    leftArcLen,
    rightArcLen,
    ringMuted,
    shockwaveR,
    shockwaveOpacity,
    coreValue,
    coreLabel,
    live,
    heartbeats,
    footerStats,
    footerBrand
  }: Props = $props()
</script>

<!-- Center Smooth Rounded Hexagon Core (Pixel-Locked at 460, 114) -->
<g transform={`translate(${CORE_X}, ${CORE_Y})`}>
  <circle r={RING_R} fill="none" stroke="#1e1b18" stroke-width="3.2" />
  <circle
    r={RING_R}
    fill="none"
    stroke="#4cc26b"
    stroke-width="3.2"
    stroke-linecap="round"
    stroke-dasharray={`${leftArcLen.toFixed(1)} ${RING_CIRC.toFixed(1)}`}
    transform="rotate(90)"
    opacity={ringMuted ? 0.45 : 0.86}
  />
  <circle
    r={RING_R}
    fill="none"
    stroke="#f7991b"
    stroke-width="3.2"
    stroke-linecap="round"
    stroke-dasharray={`${rightArcLen.toFixed(1)} ${RING_CIRC.toFixed(1)}`}
    stroke-dashoffset={`-${leftArcLen.toFixed(1)}`}
    transform="rotate(90)"
    opacity={ringMuted ? 0.45 : 0.9}
  />

  {#if shockwaveOpacity > 0.01}
    <circle r={shockwaveR} fill="none" stroke="#ffd479" stroke-width="2.2" opacity={shockwaveOpacity} />
  {/if}

  <path d={SMOOTH_HEX_PATH} fill={`url(#${uid}-hex)`} stroke="#332e29" stroke-width="1.4" stroke-linejoin="round" />
  <path d={SMOOTH_HEX_PATH} transform="scale(0.92)" fill="none" stroke="#f7991b" stroke-opacity="0.14" stroke-width="1" />

  <path fill="#f7991b" transform="translate(-7,-38) scale(0.036)" d={NEKTAR_DROPLET_PATH} />
  <text x="0" y="9" text-anchor="middle" fill="#f1ede9" font-size="22" font-weight="800" font-family="ui-monospace, monospace">
    {coreValue}
  </text>
  <text x="0" y="27" text-anchor="middle" fill="#7d746b" font-size="10" font-weight="700" letter-spacing="0.08em" font-family="system-ui, sans-serif">
    {coreLabel}
  </text>
  <circle cx="0" cy="42" r="2.8" fill={live ? '#4cc26b' : '#e5534b'} class:live-dot={live} />
</g>

<!-- Bottom 49px Strip (identical height across Market & Agents) -->
<line x1="0" y1="218" x2="920" y2="218" stroke="#1e1b18" stroke-width="1" />
<rect x="0" y="219" width="920" height="49" fill="#0c0b09" />

{#if heartbeats && heartbeats.length > 0}
  <line x1="24" y1="243" x2="896" y2="243" stroke="#1f1c19" stroke-width="1" />
  {#each heartbeats as slot, i}
    {@const x = 24 + i * 20}
    {@const fade = 0.28 + (i / heartbeats.length) * 0.68}
    {#if slot.buy > 0}
      <rect {x} y={242 - slot.buy} width="9" height={slot.buy} rx="2.5" fill="#4cc26b" opacity={fade} />
    {/if}
    {#if slot.sell > 0}
      <rect {x} y="244" width="9" height={slot.sell} rx="2.5" fill="#f7991b" opacity={fade} />
    {/if}
    {#if slot.settled}
      <circle cx={x + 4.5} cy="243" r="3.6" fill="#ffd479" filter={`url(#${uid}-neon)`} />
    {/if}
  {/each}
{:else if footerStats && footerStats.length > 0}
  {#each footerStats as st, i}
    {@const x = 24 + i * 162}
    <text {x} y="247" fill="#7d746b" font-size="10.5" font-weight="700" letter-spacing="0.06em" font-family="system-ui, sans-serif">
      {st.label}
    </text>
    <text x={x + 56} y="247" fill={st.color || '#f1ede9'} font-size="13" font-weight="800" font-family="ui-monospace, monospace">
      {st.value}
    </text>
  {/each}
  <text x="896" y="247" text-anchor="end" fill="#5e5750" font-size="10.5" font-weight="700" letter-spacing="0.08em" font-family="system-ui, sans-serif">
    {footerBrand}
  </text>
{/if}

<style>
  @keyframes hc-pulse {
    0%, 100% { opacity: 1; }
    50% { opacity: 0.35; }
  }
  .live-dot {
    animation: hc-pulse 1.9s ease-in-out infinite;
  }
  @media (prefers-reduced-motion: reduce) {
    .live-dot { animation: none; }
  }
</style>
