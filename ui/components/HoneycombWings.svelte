<svelte:options namespace="svg" />

<script lang="ts">
  import { rowY, type StageArrow, type StageBar } from './HoneycombModel'

  interface Props {
    uid: string
    flowMode: 'inward' | 'through'
    leftBars: StageBar[]
    rightBars: StageBar[]
    leftArrows: StageArrow[]
    rightArrows: StageArrow[]
    rightEmptyText: string
    hoverSide: 'left' | 'right' | null
    hoverId: string | null
    onHover: (side: 'left' | 'right' | null, id: string | null, label: string | null) => void
    onBarClick: (side: 'left' | 'right', rowIdx: number, bar: StageBar) => void
  }

  let {
    uid,
    flowMode,
    leftBars,
    rightBars,
    leftArrows,
    rightArrows,
    rightEmptyText,
    hoverSide,
    hoverId,
    onHover,
    onBarClick
  }: Props = $props()
</script>

<!-- Left Curved Flow Arrows -->
{#each leftArrows as a (a.id)}
  {@const focused = a.highlighted || (hoverSide === 'left' && hoverId === a.id)}
  {@const dimmed = a.dimmed || (hoverSide === 'left' && hoverId !== a.id)}
  <path
    d={a.d}
    fill="none"
    stroke={`url(#${uid}-flow-l)`}
    stroke-width={focused ? 2.1 : a.matched ? 1.6 : 1.35}
    stroke-linecap="round"
    stroke-opacity={dimmed ? 0.14 : focused ? 0.96 : a.matched ? 0.78 : 0.62}
    marker-end={focused || a.matched || flowMode === 'through' ? `url(#${uid}-arr-l)` : `url(#${uid}-arr-l-dim)`}
    class="flow-arrow"
  />
{/each}

<!-- Right Curved Flow Arrows -->
{#each rightArrows as a (a.id)}
  {@const focused = a.highlighted || (hoverSide === 'right' && hoverId === a.id)}
  {@const dimmed = a.dimmed || (hoverSide === 'right' && hoverId !== a.id)}
  <path
    d={a.d}
    fill="none"
    stroke={`url(#${uid}-flow-r)`}
    stroke-width={focused ? 2.1 : a.matched ? 1.6 : 1.35}
    stroke-linecap="round"
    stroke-opacity={dimmed ? 0.14 : focused ? 0.96 : a.matched ? 0.78 : 0.65}
    marker-end={focused || a.matched || flowMode === 'through' ? `url(#${uid}-arr-r)` : `url(#${uid}-arr-r-dim)`}
    class="flow-arrow"
  />
{/each}

<!-- Left Wing Bars -->
{#each leftBars as b, i (b.id)}
  {@const y = rowY(i)}
  {@const focused = b.highlighted || (hoverSide === 'left' && hoverId === b.id)}
  {@const dim = b.dimmed || (hoverSide === 'left' && hoverId !== b.id)}
  <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
  <g
    class="bar"
    opacity={dim ? 0.28 : 1}
    onmouseenter={() => onHover('left', b.id, b.hoverText)}
    onmouseleave={() => onHover(null, null, null)}
    onclick={() => onBarClick('left', i, b)}
  >
    <rect
      x="24"
      {y}
      width={b.width}
      height="26"
      rx="8"
      fill={`url(#${uid}-bar-l)`}
      stroke={focused ? '#4cc26b' : '#1f3325'}
      stroke-width={focused ? 1.3 : 1}
    />
    {#if b.dotColor}
      <circle cx="36" cy={y + 13} r="3.2" fill={b.dotColor} />
    {/if}
    <text x={b.dotColor ? 46 : 34} y={y + 17} fill="#f1ede9" font-size="11.5" font-weight="700" font-family="system-ui, sans-serif">
      {b.label.slice(0, 16)}
    </text>
    {#if b.badge}
      <text x={24 + b.width - 10} y={y + 17} text-anchor="end" fill={b.badgeColor || '#4cc26b'} font-size="11" font-weight="800" font-family="ui-monospace, monospace">
        {b.badge}
      </text>
    {/if}
  </g>
{/each}

<!-- Right Wing Bars -->
{#if rightBars.length === 0 && rightEmptyText}
  <text x="760" y="118" text-anchor="middle" fill="#6d655d" font-size="11" font-family="system-ui, sans-serif">
    {rightEmptyText}
  </text>
{:else}
  {#each rightBars as b, i (b.id)}
    {@const y = rowY(i)}
    {@const x = 896 - b.width}
    {@const focused = b.highlighted || (hoverSide === 'right' && hoverId === b.id)}
    {@const dim = b.dimmed || (hoverSide === 'right' && hoverId !== b.id)}
    <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
    <g
      class="bar"
      opacity={dim ? 0.28 : 1}
      onmouseenter={() => onHover('right', b.id, b.hoverText)}
      onmouseleave={() => onHover(null, null, null)}
      onclick={() => onBarClick('right', i, b)}
    >
      <rect
        {x}
        {y}
        width={b.width}
        height="26"
        rx="8"
        fill={`url(#${uid}-bar-r)`}
        stroke={focused ? '#f7991b' : '#382917'}
        stroke-width={focused ? 1.3 : 1}
      />
      {#if b.badge}
        <text x={x + 10} y={y + 17} fill={b.badgeColor || '#f7991b'} font-size="11" font-weight="800" font-family="ui-monospace, monospace">
          {b.badge}
        </text>
      {/if}
      <text x="886" y={y + 17} text-anchor="end" fill="#f1ede9" font-size="11.5" font-weight="700" font-family="system-ui, sans-serif">
        {b.label.slice(0, 16)}
      </text>
    </g>
  {/each}
{/if}

<style>
  .bar {
    cursor: pointer;
    transition: opacity 0.16s ease;
  }
  .flow-arrow {
    transition: stroke-opacity 0.18s ease, stroke-width 0.18s ease;
  }
</style>
