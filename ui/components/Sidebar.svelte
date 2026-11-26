<script lang="ts">
  import type { NavTab } from '../lib/types'
  import { fmtSats } from '../lib/format'

  interface Props {
    tab: NavTab
    live: boolean
    network: string
    pub: string
    uptimeSec: number
    pendingApprovals: number
    balanceSats: number
    earnedSats: number
    onSelect: (t: NavTab) => void
    onSellQuick: () => void
    onOpenPalette: () => void
  }

  let { tab, live, pendingApprovals, balanceSats, earnedSats, onSelect, onSellQuick, onOpenPalette }: Props = $props()
</script>

<aside class="rail-left">
  <div class="top-group">
    <button class="brand" onclick={() => onSelect('home')} title="Nektar Market">
      <svg viewBox="0 0 400 400" width="28" height="28" class="logo">
        <rect width="400" height="400" fill="#181513" rx="84" />
        <path
          fill="#f7991b"
          d="M 195 55 C 178 55 164 63 157 80 C 153 100 149 112 143 124 C 136 139 115 158 93 186 C 80 206 73 226 72 250 C 71 277 79 306 98 329 C 117 352 150 372 190 377 C 232 380 255 356 259 322 C 261 296 274 273 296 251 C 310 234 318 210 318 186 C 318 158 312 137 302 120 C 292 102 276 84 260 74 C 245 64 218 55 195 55 Z"
        />
        <circle cx="307" cy="78" r="38" fill="#f7991b" />
        <ellipse cx="163" cy="190" rx="20" ry="21" fill="#181513" />
        <ellipse cx="231" cy="190" rx="20" ry="21" fill="#181513" />
        <path d="M 150 265.5 C 161 279 176 287 196 288 C 216 287 230 278 243 265" fill="none" stroke="#181513" stroke-width="24" stroke-linecap="round" />
      </svg>
      <span class="bname">nektar</span>
      <span class="dot" class:off={!live} title={live ? 'Live' : 'Reconnecting'}></span>
    </button>

    <nav class="nav" aria-label="Primary">
      <button class="ni" class:active={tab === 'home'} onclick={() => onSelect('home')}>
        <svg viewBox="0 0 24 24" class="ico">
          <path d="M13 3L4 14h7l-1 7 9-11h-7l1-7z" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linejoin="round" />
        </svg>
        <span class="lbl">Market</span>
        {#if pendingApprovals > 0}<span class="badge">{pendingApprovals}</span>{/if}
      </button>

      <button class="ni" class:active={tab === 'memory'} onclick={() => onSelect('memory')}>
        <svg viewBox="0 0 24 24" class="ico">
          <path d="M12 4L3.5 8.5 12 13l8.5-4.5L12 4z" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linejoin="round" />
          <path d="M3.5 12.5L12 17l8.5-4.5M3.5 16.5L12 21l8.5-4.5" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" />
        </svg>
        <span class="lbl">Memory</span>
      </button>

      <button class="ni" class:active={tab === 'wallet'} onclick={() => onSelect('wallet')}>
        <svg viewBox="0 0 24 24" class="ico">
          <rect x="3" y="6" width="18" height="13" rx="2.5" fill="none" stroke="currentColor" stroke-width="1.8" />
          <path d="M16 12.5h5" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" />
          <circle cx="16.5" cy="12.5" r="1" fill="currentColor" />
        </svg>
        <span class="lbl">Wallet</span>
      </button>

      <button class="ni" class:active={tab === 'node'} onclick={() => onSelect('node')}>
        <svg viewBox="0 0 24 24" class="ico">
          <circle cx="12" cy="12" r="3" fill="none" stroke="currentColor" stroke-width="1.8" />
          <path d="M12 3v3M12 18v3M3 12h3M18 12h3M5.6 5.6l2.1 2.1M16.3 16.3l2.1 2.1M18.4 5.6l-2.1 2.1M7.7 16.3l-2.1 2.1" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" />
        </svg>
        <span class="lbl">Node</span>
      </button>
    </nav>

    <button class="cta" onclick={onSellQuick}>
      <svg viewBox="0 0 20 20" width="15" height="15">
        <path d="M10 4v12M4 10h12" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" />
      </svg>
      <span class="lbl">Sell Data</span>
    </button>
  </div>

  <div class="bottom-group">
    <button class="mini-bal" onclick={() => onSelect('wallet')} title="Open Wallet">
      <span class="mb-lbl">WALLET</span>
      <span class="mb-val">{fmtSats(balanceSats)} <small>sats</small></span>
      {#if earnedSats > 0}
        <span class="mb-earn">+{fmtSats(earnedSats)} earned</span>
      {/if}
    </button>

    <button class="cmd-btn" onclick={onOpenPalette} title="Command Palette (Ctrl+K / ⌘K)">
      <span class="lbl">Commands</span>
      <kbd>⌘K</kbd>
    </button>
  </div>
</aside>

<style>
  .rail-left {
    position: sticky;
    top: 0;
    height: 100vh;
    display: flex;
    flex-direction: column;
    justify-content: space-between;
    padding: 1.1rem 0.9rem 1.1rem 0.5rem;
  }
  .top-group {
    display: flex;
    flex-direction: column;
    gap: 0.95rem;
  }
  .bottom-group {
    display: flex;
    flex-direction: column;
    gap: 0.55rem;
  }
  .brand {
    display: flex;
    align-items: center;
    gap: 0.6rem;
    background: none;
    border: 0;
    padding: 0.35rem 0.6rem;
    cursor: pointer;
    color: var(--ink);
    font: inherit;
    border-radius: 10px;
    width: fit-content;
  }
  .brand:hover { background: var(--hover); }
  .logo { border-radius: 8px; flex-shrink: 0; }
  .bname { font-size: 1.16rem; font-weight: 800; letter-spacing: -0.025em; }
  .dot {
    width: 7px; height: 7px; border-radius: 50%;
    background: var(--ok); box-shadow: 0 0 8px rgb(76 194 107 / 55%);
  }
  .dot.off { background: var(--bad); box-shadow: none; }
  .nav { display: flex; flex-direction: column; gap: 0.2rem; }
  .ni {
    display: flex; align-items: center; gap: 0.85rem;
    padding: 0.66rem 0.85rem; border-radius: 999px; border: 0; background: none;
    color: var(--mut); font: inherit; font-size: 0.96rem; font-weight: 600;
    cursor: pointer; text-align: left; transition: background 0.14s, color 0.14s;
  }
  .ni:hover { background: var(--hover); color: var(--ink); }
  .ni.active { background: #161412; color: var(--ink); font-weight: 800; }
  .ni.active .ico { color: var(--amber); }
  .ico { width: 20px; height: 20px; flex-shrink: 0; }
  .badge {
    margin-left: auto; background: var(--amber); color: #120e0c;
    font-size: 0.7rem; font-weight: 800; padding: 0.08rem 0.45rem; border-radius: 999px;
    font-variant-numeric: tabular-nums;
  }
  .cta {
    margin-top: 0.2rem;
    display: flex; align-items: center; justify-content: center; gap: 0.42rem;
    background: var(--amber); color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.68rem 0.95rem; font: inherit; font-size: 0.9rem; font-weight: 800;
    cursor: pointer; transition: filter 0.14s, transform 0.1s;
  }
  .cta:hover { filter: brightness(1.08); }
  .cta:active { transform: scale(0.98); }

  .mini-bal {
    display: flex;
    flex-direction: column;
    gap: 0.16rem;
    background: var(--surf);
    border: 1px solid var(--line);
    border-radius: 13px;
    padding: 0.72rem 0.82rem;
    color: var(--ink);
    font: inherit;
    text-align: left;
    cursor: pointer;
    transition: border-color 0.14s;
  }
  .mini-bal:hover { border-color: var(--amber); }
  .mb-lbl {
    font-size: 0.63rem;
    font-weight: 800;
    letter-spacing: 0.1em;
    color: var(--mut2);
  }
  .mb-val {
    font-family: ui-monospace, monospace;
    font-size: 1.08rem;
    font-weight: 800;
    font-variant-numeric: tabular-nums;
  }
  .mb-val small {
    font-size: 0.72rem;
    color: var(--amber);
    font-weight: 700;
  }
  .mb-earn {
    font-family: ui-monospace, monospace;
    font-size: 0.7rem;
    color: var(--ok);
    font-weight: 700;
  }

  .cmd-btn {
    display: flex; align-items: center; justify-content: space-between;
    background: transparent; border: 1px solid var(--line); border-radius: 10px;
    color: var(--mut2); padding: 0.46rem 0.75rem; font: inherit; font-size: 0.78rem;
    cursor: pointer; transition: border-color 0.14s, color 0.14s;
  }
  .cmd-btn:hover { border-color: var(--line2); color: var(--ink); }
  .cmd-btn kbd {
    font-family: ui-monospace, monospace; font-size: 0.68rem; color: var(--mut);
    background: var(--surf); border: 1px solid var(--line); border-radius: 5px; padding: 0.05rem 0.35rem;
  }

  @media (max-width: 1100px) and (min-width: 721px) {
    .rail-left { align-items: center; padding: 1rem 0.35rem; }
    .lbl, .bname, .cmd-btn, .mini-bal { display: none; }
    .brand { padding: 0.35rem; position: relative; }
    .dot { position: absolute; right: 2px; bottom: 2px; }
    .ni { padding: 0.72rem; justify-content: center; position: relative; }
    .badge { position: absolute; top: 4px; right: 4px; padding: 0.02rem 0.32rem; }
    .cta { width: 42px; height: 42px; padding: 0; }
  }

  @media (max-width: 720px) {
    .rail-left {
      position: fixed; inset: auto 0 0 0; height: 58px; z-index: 40;
      background: rgb(9 8 7 / 94%); backdrop-filter: blur(12px);
      border-top: 1px solid var(--line); flex-direction: row;
      align-items: center; justify-content: space-around; padding: 0 0.5rem;
    }
    .top-group { flex-direction: row; width: 100%; align-items: center; justify-content: space-around; gap: 0; }
    .brand, .bottom-group, .cta { display: none; }
    .nav { flex-direction: row; width: 100%; justify-content: space-around; }
    .ni { flex-direction: column; gap: 0.15rem; padding: 0.35rem 0.6rem; font-size: 0.68rem; border-radius: 10px; }
  }
</style>
