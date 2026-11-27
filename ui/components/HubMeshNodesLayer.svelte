<svelte:options namespace="svg" />

<script lang="ts">
  import type { HubCatalogEntry } from '../lib/types'
  import { feeChipText, hubMatchesTopic, type RenderLink, type RenderNode } from './HubMeshModel'

  interface Props {
    uid: string
    hubs: HubCatalogEntry[]
    renderNodes: RenderNode[]
    renderLinks: RenderLink[]
    selectedURL: string
    hoverURL: string | null
    activeTopic: string
    reducedMotion: boolean
    onStartDrag: (e: PointerEvent, id: string) => void
    onHoverChange: (url: string | null) => void
    onSelectHub: (url: string) => void
  }

  let {
    uid,
    hubs,
    renderNodes,
    renderLinks,
    selectedURL,
    hoverURL,
    activeTopic,
    reducedMotion,
    onStartDrag,
    onHoverChange,
    onSelectHub
  }: Props = $props()
</script>

<!-- 1. FEDERATION EDGES (Hub <-> Spine / Peer Hub in Golden Amber) -->
{#each renderLinks.filter((l) => l.kind === 'federation') as l (l.id)}
  {@const focused = selectedURL === l.sourceId || selectedURL === l.targetId || hoverURL === l.sourceId || hoverURL === l.targetId}
  <path
    d={l.d}
    fill="none"
    stroke="#f7991b"
    stroke-width={focused ? 1.5 : 1}
    stroke-opacity={focused ? 0.55 : 0.18}
    stroke-dasharray="4 5"
    class="edge"
  />
{/each}

<!-- 2. YOUR NODE <-> HUB EDGES (Glowing Emerald + Live Animated Packets) -->
{#each renderLinks.filter((l) => l.kind === 'mesh') as l (l.id)}
  {@const targetHub = hubs.find((h) => h.url === l.targetId)}
  {@const topicMatch = hubMatchesTopic(targetHub, activeTopic)}
  {@const focused = selectedURL === l.targetId || hoverURL === l.targetId}
  {@const durSec = Math.max(1.4, Math.min(4.2, 1.4 + (l.latencyMs || 20) / 40))}
  <path
    id={`${uid}-p-${l.id.replace(/[^a-zA-Z0-9]/g, '_')}`}
    d={l.d}
    fill="none"
    stroke={l.connected ? '#4cc26b' : '#5e5750'}
    stroke-width={focused ? 2.3 : l.connected ? 1.5 : 1}
    stroke-opacity={!topicMatch ? 0.1 : focused ? 0.95 : l.connected ? 0.56 : 0.28}
    stroke-dasharray={l.connected ? undefined : '4 5'}
    class="edge"
  />
  {#if l.connected && topicMatch && !reducedMotion}
    <circle r={focused ? 3.2 : 2.3} fill="#4cc26b" opacity={focused ? 0.95 : 0.8} filter={`url(#${uid}-neon)`}>
      <animateMotion dur={`${durSec.toFixed(2)}s`} repeatCount="indefinite" path={l.d} />
    </circle>
  {/if}
{/each}

<!-- 3. HUB NODES (Draggable Interactive Constellations) -->
{#each renderNodes.filter((n) => n.kind === 'hub' && n.hub) as n (n.id)}
  {@const h = n.hub!}
  {@const isSel = selectedURL === h.url}
  {@const isHov = hoverURL === h.url}
  {@const match = hubMatchesTopic(h, activeTopic)}
  {@const feeTxt = feeChipText(h)}
  {@const ringColor = h.connected && h.reachable ? '#4cc26b' : h.reachable && !h.networkMismatch ? '#f7991b' : '#5e5750'}
  <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
  <g
    class="hnode"
    transform={`translate(${n.x.toFixed(1)}, ${n.y.toFixed(1)})`}
    opacity={match ? 1 : 0.2}
    onpointerdown={(e) => onStartDrag(e, n.id)}
    onmouseenter={() => onHoverChange(h.url)}
    onmouseleave={() => onHoverChange(null)}
    onclick={() => onSelectHub(h.url)}
  >
    <!-- Selection / Spine Ambient Halo -->
    {#if isSel || isHov}
      <circle r="33" fill={isSel ? '#f7991b' : '#4cc26b'} opacity="0.14" />
    {/if}

    <!-- Outer Status Ring -->
    <circle
      r="24"
      fill="#12100e"
      stroke={isSel ? '#f7991b' : ringColor}
      stroke-width={isSel ? 2.4 : h.connected ? 1.8 : 1.2}
      stroke-dasharray={h.connected ? undefined : '4 3'}
      filter={isSel || (h.connected && h.reachable) ? `url(#${uid}-neon)` : undefined}
    />

    <!-- Inner Ping Readout -->
    <text
      x="0"
      y="4"
      text-anchor="middle"
      fill={h.reachable ? '#f1ede9' : '#6d655d'}
      font-size="11"
      font-weight="800"
      font-family="ui-monospace, monospace"
    >
      {h.reachable && h.latencyMs > 0 ? `${h.latencyMs}ms` : 'OFF'}
    </text>

    <!-- Compact Fee Badge floating top-right of node -->
    {#if feeTxt}
      <g transform="translate(22, -20)">
        <rect
          x="-20"
          y="-9"
          width="40"
          height="16"
          rx="8"
          fill="#171412"
          stroke={feeTxt === 'Free' ? '#274730' : '#45321c'}
        />
        <text
          x="0"
          y="2.5"
          text-anchor="middle"
          fill={feeTxt === 'Free' ? '#4cc26b' : '#f7991b'}
          font-size="8.5"
          font-weight="800"
          font-family="ui-monospace, monospace"
        >
          {feeTxt}
        </text>
      </g>
    {/if}

    <!-- Hostname Label below circle -->
    <text
      x="0"
      y="40"
      text-anchor="middle"
      fill={isSel ? '#ffd479' : '#f1ede9'}
      stroke="#090807"
      stroke-width="4.5"
      stroke-linejoin="round"
      paint-order="stroke"
      font-size="11.5"
      font-weight="800"
      font-family="system-ui, sans-serif"
    >
      {h.name}
    </text>

    <!-- Hosted Topics Micro-Line -->
    {#if h.topics?.length}
      <text
        x="0"
        y="53"
        text-anchor="middle"
        fill="#8c8279"
        stroke="#090807"
        stroke-width="4"
        stroke-linejoin="round"
        paint-order="stroke"
        font-size="9.5"
        font-weight="600"
        font-family="system-ui, sans-serif"
      >
        {h.topics.slice(0, 3).map((t) => t.replace(/^cluster:/, '')).join(' · ')}
      </text>
    {/if}
  </g>
{/each}

<style>
  .hnode {
    cursor: grab;
    transition: opacity 0.2s ease;
  }
  .hnode:active {
    cursor: grabbing;
  }
  .edge {
    transition: stroke-opacity 0.2s ease, stroke-width 0.2s ease;
  }
</style>
