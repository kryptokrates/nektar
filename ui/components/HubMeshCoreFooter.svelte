<svelte:options namespace="svg" />

<script lang="ts">
  import { MESH_CX, MESH_CY, MESH_DROPLET_PATH, MESH_HEX_PATH, MESH_W } from './HubMeshModel'

  interface Props {
    uid: string
    network: string
    connectedCount: number
    totalHubs: number
    quorumOK: boolean
    avgPingMs: number
    totalTopics: number
  }

  let { uid, network, connectedCount, totalHubs, quorumOK, avgPingMs, totalTopics }: Props = $props()
</script>

<!-- 4. CENTER NODE: YOUR NODE (Rounded Honeycomb Core) -->
<g transform={`translate(${MESH_CX}, ${MESH_CY})`}>
  <circle r="50" fill="none" stroke={quorumOK ? '#4cc26b' : '#f7991b'} stroke-width="2.4" stroke-opacity="0.72" />
  <circle r="57" fill="none" stroke={quorumOK ? '#4cc26b' : '#f7991b'} stroke-width="1" stroke-opacity="0.22" class="pulse-ring" />
  <path d={MESH_HEX_PATH} fill={`url(#${uid}-hex)`} stroke="#36312c" stroke-width="1.4" stroke-linejoin="round" />
  <path fill="#f7991b" transform="translate(-5.5,-27) scale(0.028)" d={MESH_DROPLET_PATH} />
  <text x="0" y="6" text-anchor="middle" fill="#f1ede9" font-size="14" font-weight="800" font-family="ui-monospace, monospace">
    {connectedCount}/{totalHubs}
  </text>
  <text x="0" y="20" text-anchor="middle" fill="#8c8279" font-size="8.5" font-weight="800" letter-spacing="0.09em" font-family="system-ui, sans-serif">
    YOUR NODE
  </text>
</g>

<!-- 5. BOTTOM VIRAL TELEMETRY STRIP -->
<line x1="0" y1="382" x2={MESH_W} y2="382" stroke="#1e1b18" stroke-width="1" />
<rect x="0" y="383" width={MESH_W} height="49" fill="#0c0b09" />

<text x="24" y="411" fill="#7d746b" font-size="10.5" font-weight="700" letter-spacing="0.06em" font-family="system-ui, sans-serif">
  QUORUM
</text>
<text x="84" y="411" fill={quorumOK ? '#4cc26b' : '#f7991b'} font-size="13" font-weight="800" font-family="ui-monospace, monospace">
  {connectedCount}/{totalHubs}{quorumOK ? ' ✓' : ''}
</text>

<text x="190" y="411" fill="#7d746b" font-size="10.5" font-weight="700" letter-spacing="0.06em" font-family="system-ui, sans-serif">
  AVG PING
</text>
<text x="256" y="411" fill="#f1ede9" font-size="13" font-weight="800" font-family="ui-monospace, monospace">
  {avgPingMs > 0 ? `${avgPingMs}ms` : '—'}
</text>

<text x="352" y="411" fill="#7d746b" font-size="10.5" font-weight="700" letter-spacing="0.06em" font-family="system-ui, sans-serif">
  TOPICS
</text>
<text x="406" y="411" fill="#f7991b" font-size="13" font-weight="800" font-family="ui-monospace, monospace">
  {totalTopics}
</text>

<text x={MESH_W - 24} y="411" text-anchor="end" fill="#5e5750" font-size="10.5" font-weight="700" letter-spacing="0.08em" font-family="system-ui, sans-serif">
  NEKTAR · {network.toUpperCase()} BSV OVERLAY MESH
</text>

<style>
  @keyframes hm-breathe {
    0%, 100% { opacity: 0.22; }
    50% { opacity: 0.55; }
  }
  .pulse-ring {
    animation: hm-breathe 2.6s ease-in-out infinite;
  }
  @media (prefers-reduced-motion: reduce) {
    .pulse-ring { animation: none; }
  }
</style>
