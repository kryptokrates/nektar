<script lang="ts">
  import { untrack } from 'svelte'
  interface Props {
    network: string
    onJoinHub: (url: string) => Promise<void>
  }
  let { network, onJoinHub }: Props = $props()

  let mode = $state<'auto-ip' | 'domain' | 'tunnel'>('auto-ip')
  let domain = $state('hub.example.com')
  let targetNet = $state(untrack(() => (network === 'main' ? 'main' : 'testnet')))
  let hubRole = $state<'leaf' | 'spine'>('leaf')
  let feePreset = $state<'hybrid' | 'lightbsv_egress' | 'free'>(untrack(() => (network === 'main' ? 'hybrid' : 'free')))

  type HubBundle = {
    cliCommand: string
    nativeCommand: string
    localCommand: string
    vpsOneLiner: string
    cloudInitYaml: string
    coolifyTemplateUrl: string
    helmCommand: string
  }
  type HubInspect = {
    url: string
    reachable: boolean
    network?: string
    hubRole?: string
    feePreset?: string
    error?: string
  }

  let bundle = $state<HubBundle | null>(null)
  let copied = $state('')
  let joinUrl = $state('')
  let joinBusy = $state(false)
  let inspectBusy = $state(false)
  let inspected = $state<HubInspect | null>(null)

  async function refreshBundle() {
    const qs = new URLSearchParams({
      mode,
      network: targetNet,
      role: hubRole,
      feePreset,
      ...(mode === 'domain' ? { domain } : {})
    })
    const r = await fetch(`/v1/onboard/hub-bundle?${qs.toString()}`)
    if (r.ok) bundle = await r.json()
  }

  $effect(() => {
    void mode
    void domain
    void targetNet
    void hubRole
    void feePreset
    void refreshBundle()
  })

  async function copy(id: string, text: string) {
    await navigator.clipboard.writeText(text).catch(() => {})
    copied = id
    setTimeout(() => (copied = ''), 1500)
  }

  async function inspectHub() {
    if (!joinUrl.trim()) return
    inspectBusy = true
    inspected = null
    try {
      const r = await fetch(`/v1/hub/inspect?url=${encodeURIComponent(joinUrl.trim())}`)
      if (r.ok) inspected = await r.json()
    } finally {
      inspectBusy = false
    }
  }

  async function connectHub() {
    if (!joinUrl.trim()) return
    joinBusy = true
    try {
      await onJoinHub(joinUrl.trim())
      joinUrl = ''
      inspected = null
    } finally {
      joinBusy = false
    }
  }
</script>

<div class="card">
  <h2>Deploy a Hub Node <small>earn BSV sats relaying agent traffic — 1 binary, zero MariaDB</small></h2>
  <p class="hmut">
    A v0.1 Hub runs in a single zero-CGO Go binary bundling an Overlay Hub (16 sharded lanes + <code>bfinger</code> + NAT <code>bbox</code> relay + cursored SSE + Hub federation) and BRC-105 payment meters. Hubs are untrusted plumbing — peers SPV-verify every byte.
  </p>

  <div class="role-grid">
    <button class="rcard" class:on={hubRole === 'leaf'} onclick={() => (hubRole = 'leaf')}>
      <b>Leaf Hub (Recommended)</b>
      <span>Auto-connects to the Spine network, serves your agents/community, and earns relay + egress fees.</span>
    </button>
    <button class="rcard" class:on={hubRole === 'spine'} onclick={() => (hubRole = 'spine')}>
      <b>Spine Hub (Backbone)</b>
      <span>High-volume regional backbone peering bilaterally with other Spines and metering downstream Leaves.</span>
    </button>
  </div>

  <div class="hcontrols">
    <div class="hpills">
      <button class="hpill" class:on={mode === 'auto-ip'} onclick={() => (mode = 'auto-ip')}>Zero-Domain VPS (sslip.io)</button>
      <button class="hpill" class:on={mode === 'domain'} onclick={() => (mode = 'domain')}>Custom Domain (Caddy TLS)</button>
      <button class="hpill" class:on={mode === 'tunnel'} onclick={() => (mode = 'tunnel')}>Home / NAT (Cloudflare Tunnel)</button>
    </div>
    <div class="hrow">
      {#if mode === 'domain'}
        <input class="hin" bind:value={domain} placeholder="hub.yourdomain.com" />
      {/if}
      <select class="hsel" bind:value={targetNet}>
        <option value="main">mainnet (real BSV)</option>
        <option value="testnet">testnet (free faucet)</option>
      </select>
      <select class="hsel" bind:value={feePreset}>
        <option value="hybrid">hybrid fees (5 sat Want / 2 sat Offer / 2 sat NAT / metered egress)</option>
        <option value="lightbsv_egress">pure egress (free submit / metered egress only)</option>
        <option value="free">free community relay (0 sats)</option>
      </select>
    </div>
  </div>

  {#if bundle}
    <div class="hblock">
      <div class="hhead"><b>Option 1 — Docker 1-command via CLI (auto-TLS + Restate + live health &amp; QR)</b><button onclick={() => copy('cli', bundle!.cliCommand)}>{copied === 'cli' ? '✓ copied' : 'copy'}</button></div>
      <pre>{bundle.cliCommand}</pre>
    </div>

    <div class="hblock">
      <div class="hhead"><b>Option 2 — Zero-Docker native binary / systemd VPS</b><button onclick={() => copy('native', bundle!.nativeCommand)}>{copied === 'native' ? '✓ copied' : 'copy'}</button></div>
      <pre>{bundle.nativeCommand}</pre>
    </div>

    <div class="hblock">
      <div class="hhead"><b>Option 3 — Fresh Linux VPS one-liner (Hetzner, DO, Vultr)</b><button onclick={() => copy('vps', bundle!.vpsOneLiner)}>{copied === 'vps' ? '✓ copied' : 'copy'}</button></div>
      <pre>{bundle.vpsOneLiner}</pre>
    </div>

    <div class="hblock">
      <div class="hhead"><b>Option 4 — Run on THIS machine right now (no Docker)</b><button onclick={() => copy('local', bundle!.localCommand)}>{copied === 'local' ? '✓ copied' : 'copy'}</button></div>
      <pre>{bundle.localCommand}</pre>
    </div>

    <div class="hblock">
      <div class="hhead"><b>Option 5 — Cloud-Init (paste into Hetzner / DigitalOcean User Data)</b><button onclick={() => copy('ci', bundle!.cloudInitYaml)}>{copied === 'ci' ? '✓ copied' : 'copy'}</button></div>
      <pre>{bundle.cloudInitYaml}</pre>
    </div>

    <div class="hblock">
      <div class="hhead"><b>1-Click PaaS &amp; Home Server Templates</b></div>
      <div class="hlinks">
        <a href="https://github.com/kryptokrates/nektar/blob/master/deploy/hub/templates/coolify.compose.yaml" target="_blank" rel="noopener">Coolify ↗</a>
        <a href="https://github.com/kryptokrates/nektar/blob/master/deploy/hub/templates/dokploy.compose.yaml" target="_blank" rel="noopener">Dokploy ↗</a>
        <a href="https://github.com/kryptokrates/nektar/tree/master/deploy/hub/templates/umbrel" target="_blank" rel="noopener">Umbrel ↗</a>
        <a href="https://github.com/kryptokrates/nektar/blob/master/deploy/hub/templates/casaos.yaml" target="_blank" rel="noopener">CasaOS ↗</a>
        <a href="https://github.com/kryptokrates/nektar/blob/master/deploy/hub/templates/render.yaml" target="_blank" rel="noopener">Render ↗</a>
        <a href="https://github.com/kryptokrates/nektar/blob/master/deploy/hub/templates/fly.toml" target="_blank" rel="noopener">Fly.io ↗</a>
        <a href="https://github.com/kryptokrates/nektar/tree/master/deploy/hub/helm" target="_blank" rel="noopener">Kubernetes Helm ↗</a>
      </div>
    </div>
  {/if}
</div>

<div class="card">
  <h2>Connect your agent to any Hub</h2>
  <p class="hmut">Paste a Hub URL to verify its network, role &amp; fee terms first, then hot-join without restarting.</p>
  <div class="hrow">
    <input class="hin" bind:value={joinUrl} placeholder="https://hub.example.com" onkeydown={(e) => e.key === 'Enter' && inspectHub()} />
    <button class="hsec" onclick={inspectHub} disabled={inspectBusy || !joinUrl.trim()}>{inspectBusy ? 'Checking…' : 'Verify'}</button>
    <button class="hbtn" onclick={connectHub} disabled={joinBusy || !joinUrl.trim()}>{joinBusy ? 'Joining…' : 'Join Hub'}</button>
  </div>
  {#if inspected}
    <div class="inspect" class:bad={!inspected.reachable}>
      {#if inspected.reachable}
        <b>Reachable ✓</b> · network: <code>{inspected.network ?? 'unknown'}</code> · role: <code>{inspected.hubRole ?? 'leaf'}</code> · fees: <code>{inspected.feePreset ?? 'free'}</code>
      {:else}
        <b>Unreachable:</b> {inspected.error ?? 'Could not reach /.well-known/nektar or /hub/v1/terms'}
      {/if}
    </div>
  {/if}
</div>

<style>
  .hmut { color: #a89a8e; font-size: 0.84rem; line-height: 1.5; margin: 0.4rem 0 0.85rem; }
  .role-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(210px, 1fr)); gap: 0.55rem; margin-bottom: 0.85rem; }
  .rcard {
    text-align: left; background: var(--surf, #11100e); border: 1px solid var(--line, #23201d);
    border-radius: 10px; padding: 0.65rem 0.78rem; cursor: pointer; display: grid; gap: 0.2rem; color: var(--ink, #f1ede9);
  }
  .rcard b { font-size: 0.82rem; }
  .rcard span { font-size: 0.74rem; color: var(--mut, #9a9087); line-height: 1.4; }
  .rcard.on { border-color: #f7991b; background: rgb(247 153 27 / 10%); }
  .rcard.on b { color: #f7991b; }
  .hcontrols { display: grid; gap: 0.6rem; margin-bottom: 0.9rem; }
  .hpills { display: flex; gap: 0.4rem; flex-wrap: wrap; }
  .hpill {
    background: var(--surf, #11100e); border: 1px solid var(--line, #23201d);
    color: var(--mut, #9a9087); border-radius: 99px; padding: 0.32rem 0.72rem;
    font-size: 0.78rem; font-weight: 600; cursor: pointer;
  }
  .hpill.on { background: rgb(247 153 27 / 13%); border-color: #f7991b; color: #f7991b; }
  .hrow { display: flex; gap: 0.5rem; flex-wrap: wrap; }
  .hin, .hsel {
    background: var(--bg, #090807); border: 1px solid var(--line, #23201d);
    border-radius: 9px; color: var(--ink, #f1ede9); padding: 0.48rem 0.72rem; font-size: 0.8rem;
  }
  .hin { flex: 1; min-width: 200px; font-family: ui-monospace, monospace; }
  .hblock {
    background: var(--surf, #11100e); border: 1px solid var(--line, #23201d);
    border-radius: 12px; padding: 0.8rem 0.9rem; margin-bottom: 0.7rem;
  }
  .hhead { display: flex; align-items: center; justify-content: space-between; gap: 0.5rem; font-size: 0.8rem; margin-bottom: 0.45rem; }
  .hhead button, .hbtn {
    background: #f7991b; color: #120e0c; border: 0; border-radius: 999px;
    padding: 0.32rem 0.8rem; font-size: 0.76rem; font-weight: 800; cursor: pointer;
  }
  .hsec {
    background: var(--surf, #11100e); color: var(--ink, #f1ede9); border: 1px solid var(--line2, #342f2a);
    border-radius: 999px; padding: 0.32rem 0.8rem; font-size: 0.76rem; font-weight: 700; cursor: pointer;
  }
  .hsec:hover { border-color: #f7991b; color: #f7991b; }
  .inspect {
    margin-top: 0.65rem; padding: 0.55rem 0.75rem; border-radius: 9px; font-size: 0.78rem;
    background: rgb(76 194 107 / 12%); border: 1px solid rgb(76 194 107 / 35%); color: #b7f2c5;
  }
  .inspect.bad { background: rgb(229 83 75 / 12%); border-color: rgb(229 83 75 / 40%); color: #ffc6c3; }
  pre {
    margin: 0; background: var(--bg, #090807); border: 1px solid var(--line, #23201d);
    border-radius: 8px; padding: 0.6rem; font-size: 0.74rem; overflow-x: auto;
    color: var(--ink, #f1ede9); white-space: pre-wrap; word-break: break-all;
  }
  .hlinks { display: flex; flex-wrap: wrap; gap: 0.65rem 1rem; font-size: 0.82rem; }
  .hlinks a { color: #f7991b; text-decoration: none; }
  .hlinks a:hover { text-decoration: underline; }
</style>
