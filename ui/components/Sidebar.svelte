<script lang="ts">
  import type { NavTab } from '../lib/types'

  interface Props {
    tab: NavTab
    live: boolean
    aboutActive?: boolean
    pendingApprovals: number
    connectedHubs?: number
    onSelect: (t: NavTab) => void
    onOpenPalette: () => void
    onOpenAbout: () => void
  }

  let { tab, live, aboutActive = false, pendingApprovals, connectedHubs = 0, onSelect, onOpenPalette, onOpenAbout }: Props = $props()
</script>

<aside class="rail-left">
  <div class="top-group">
    <button class="brand" onclick={() => onSelect('home')} title="Nektar">
      <svg viewBox="0 0 400 400" width="26" height="26" class="logo">
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
      <span class="dot" class:off={!live} title={live ? 'Live' : 'Offline'}></span>
    </button>

    <nav class="nav" aria-label="Primary">
      <button class="ni" class:active={tab === 'home'} onclick={() => onSelect('home')}>
        <svg viewBox="0 0 24 24" class="ico">
          <path d="M13 3L4 14h7l-1 7 9-11h-7l1-7z" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linejoin="round" />
        </svg>
        <span class="lbl">Market</span>
        {#if pendingApprovals > 0}<span class="badge">{pendingApprovals}</span>{/if}
      </button>

      <button class="ni" class:active={tab === 'agents'} onclick={() => onSelect('agents')}>
        <svg viewBox="0 0 24 24" class="ico">
          <circle cx="12" cy="7" r="3" fill="none" stroke="currentColor" stroke-width="1.8" />
          <circle cx="6" cy="17" r="2.5" fill="none" stroke="currentColor" stroke-width="1.8" />
          <circle cx="18" cy="17" r="2.5" fill="none" stroke="currentColor" stroke-width="1.8" />
          <path d="M10.4 9.6L7.4 14.8M13.6 9.6l3 5.2M8.5 17h7" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" />
        </svg>
        <span class="lbl">Agents</span>
      </button>

      <button class="ni" class:active={tab === 'hubs'} onclick={() => onSelect('hubs')}>
        <svg viewBox="0 0 24 24" class="ico">
          <circle cx="12" cy="12" r="8.5" fill="none" stroke="currentColor" stroke-width="1.7" />
          <ellipse cx="12" cy="12" rx="3.8" ry="8.5" fill="none" stroke="currentColor" stroke-width="1.5" />
          <path d="M4 12h16" stroke="currentColor" stroke-width="1.5" />
        </svg>
        <span class="lbl">Hubs</span>
        {#if connectedHubs > 0}<span class="cnt">{connectedHubs}</span>{/if}
      </button>

      <button class="ni" class:active={tab === 'wallet'} onclick={() => onSelect('wallet')}>
        <svg viewBox="0 0 24 24" class="ico">
          <rect x="3" y="6" width="18" height="13" rx="2.5" fill="none" stroke="currentColor" stroke-width="1.8" />
          <path d="M16 12.5h5" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" />
          <circle cx="16.5" cy="12.5" r="1" fill="currentColor" />
        </svg>
        <span class="lbl">Chain &amp; Vault</span>
      </button>

      <button class="ni" class:active={tab === 'settings'} onclick={() => onSelect('settings')}>
        <svg viewBox="0 0 24 24" class="ico">
          <circle cx="12" cy="12" r="3" fill="none" stroke="currentColor" stroke-width="1.8" />
          <path d="M12 3v3M12 18v3M3 12h3M18 12h3M5.6 5.6l2.1 2.1M16.3 16.3l2.1 2.1M18.4 5.6l-2.1 2.1M7.7 16.3l-2.1 2.1" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" />
        </svg>
        <span class="lbl">Settings</span>
      </button>
    </nav>
  </div>

  <div class="foot-group">
    <button class="cmd-btn" onclick={onOpenPalette} title="Command Palette (Ctrl+K / ⌘K)">
      <span class="lbl">Commands</span>
      <kbd>⌘K</kbd>
    </button>
    <button class="about-btn" class:active={aboutActive} onclick={onOpenAbout} title="About Nektar (Settings → System)">
      <span class="about-ver">v0.1</span>
      <span class="about-sep" aria-hidden="true">·</span>
      <span>About</span>
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
    padding: 1.05rem 0.85rem 1.05rem 0.5rem;
  }
  .top-group {
    display: flex;
    flex-direction: column;
    gap: 1rem;
  }
  .brand {
    display: flex;
    align-items: center;
    gap: 0.55rem;
    background: none;
    border: 0;
    padding: 0.3rem 0.55rem;
    cursor: pointer;
    color: var(--ink);
    font: inherit;
    border-radius: 10px;
    width: fit-content;
  }
  .brand:hover { background: var(--hover); }
  .logo { border-radius: 7px; flex-shrink: 0; }
  .bname { font-size: 1.12rem; font-weight: 800; letter-spacing: -0.025em; }
  .dot {
    width: 6.5px; height: 6.5px; border-radius: 50%;
    background: var(--ok); box-shadow: 0 0 8px rgb(76 194 107 / 55%);
  }
  .dot.off { background: var(--bad); box-shadow: none; }
  .nav { display: flex; flex-direction: column; gap: 0.18rem; }
  .ni {
    display: flex; align-items: center; gap: 0.8rem;
    padding: 0.62rem 0.8rem; border-radius: 999px; border: 0; background: none;
    color: var(--mut); font: inherit; font-size: 0.94rem; font-weight: 600;
    cursor: pointer; text-align: left; transition: background 0.14s, color 0.14s;
  }
  .ni:hover { background: var(--hover); color: var(--ink); }
  .ni.active { background: #161412; color: var(--ink); font-weight: 700; }
  .ni.active .ico { color: var(--amber); }
  .ico { width: 19px; height: 19px; flex-shrink: 0; }
  .badge {
    margin-left: auto; background: var(--amber); color: #120e0c;
    font-size: 0.68rem; font-weight: 800; padding: 0.06rem 0.42rem; border-radius: 999px;
    font-variant-numeric: tabular-nums;
  }
  .cnt {
    margin-left: auto; font-family: ui-monospace, monospace;
    font-size: 0.7rem; font-weight: 700; color: var(--mut2);
    font-variant-numeric: tabular-nums;
  }
  .foot-group {
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
  }
  .cmd-btn {
    display: flex; align-items: center; justify-content: space-between;
    background: transparent; border: 1px solid var(--line); border-radius: 10px;
    color: var(--mut2); padding: 0.44rem 0.72rem; font: inherit; font-size: 0.76rem;
    cursor: pointer; transition: border-color 0.14s, color 0.14s;
  }
  .cmd-btn:hover { border-color: var(--line2); color: var(--ink); }
  .cmd-btn kbd {
    font-family: ui-monospace, monospace; font-size: 0.68rem; color: var(--mut);
    background: var(--surf); border: 1px solid var(--line); border-radius: 5px; padding: 0.05rem 0.32rem;
  }
  .about-btn {
    display: inline-flex;
    align-items: center;
    gap: 0.32rem;
    width: fit-content;
    background: none;
    border: 0;
    border-radius: 8px;
    padding: 0.24rem 0.5rem;
    color: var(--mut2);
    font: inherit;
    font-size: 0.72rem;
    font-weight: 600;
    cursor: pointer;
    transition: color 0.14s, background 0.14s;
  }
  .about-btn:hover { color: var(--ink); background: var(--hover); }
  .about-btn.active { color: var(--amber); }
  .about-ver {
    font-family: ui-monospace, 'Cascadia Mono', 'SFMono-Regular', Consolas, monospace;
    font-size: 0.69rem;
  }
  .about-sep { opacity: 0.55; }

  @media (max-width: 1100px) and (min-width: 721px) {
    .rail-left { align-items: center; padding: 1rem 0.35rem; }
    .lbl, .bname, .foot-group { display: none; }
    .brand { padding: 0.35rem; position: relative; }
    .dot { position: absolute; right: 2px; bottom: 2px; }
    .ni { padding: 0.7rem; justify-content: center; position: relative; }
    .badge { position: absolute; top: 4px; right: 4px; padding: 0.02rem 0.32rem; }
  }

  @media (max-width: 720px) {
    .rail-left {
      position: fixed; inset: auto 0 0 0; height: 58px; z-index: 40;
      background: rgb(9 8 7 / 94%); backdrop-filter: blur(12px);
      border-top: 1px solid var(--line); flex-direction: row;
      align-items: center; justify-content: space-around; padding: 0 0.5rem;
    }
    .top-group { flex-direction: row; width: 100%; align-items: center; justify-content: space-around; gap: 0; }
    .brand, .foot-group { display: none; }
    .nav { flex-direction: row; width: 100%; justify-content: space-around; }
    .ni { flex-direction: column; gap: 0.15rem; padding: 0.35rem 0.6rem; font-size: 0.68rem; border-radius: 10px; }
  }
</style>
