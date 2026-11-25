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
  let bundle = $state<{
    cliCommand: string
    vpsOneLiner: string
    cloudInitYaml: string
    coolifyTemplateUrl: string
    helmCommand: string
  } | null>(null)
  let copied = $state('')
  let joinUrl = $state('')
  let joinBusy = $state(false)

  async function refreshBundle() {
    const qs = new URLSearchParams({ mode, network: targetNet, ...(mode === 'domain' ? { domain } : {}) })
    const r = await fetch(`/v1/onboard/hub-bundle?${qs.toString()}`)
    if (r.ok) bundle = await r.json()
  }

  $effect(() => {
    void mode
    void domain
    void targetNet
    void refreshBundle()
  })

  async function copy(id: string, text: string) {
    await navigator.clipboard.writeText(text).catch(() => {})
    copied = id
    setTimeout(() => (copied = ''), 1500)
  }

  async function connectHub() {
    if (!joinUrl.trim()) return
    joinBusy = true
    try {
      await onJoinHub(joinUrl.trim())
      joinUrl = ''
    } finally {
      joinBusy = false
    }
  }
</script>

<div class="card">
  <h2>Deploy a Hub Node <small>graduate from personal agent to independent network relay</small></h2>
  <p class="hmut">
    A Hub bundles your agent, redundant overlay host pair (<code>host-a</code>/<code>host-b</code>), watch shims, and built-in
    <b>Caddy Auto-TLS</b>. Hosts are untrusted plumbing — every artifact is signature + SPV verified by peers.
  </p>

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
        <option value="testnet">testnet (free auto-faucet)</option>
        <option value="main">mainnet (real BSV)</option>
      </select>
    </div>
  </div>

  {#if bundle}
    <div class="hblock">
      <div class="hhead"><b>Option 1 — One command via nektar CLI</b><button onclick={() => copy('cli', bundle!.cliCommand)}>{copied === 'cli' ? '✓ copied' : 'copy'}</button></div>
      <pre>{bundle.cliCommand}</pre>
    </div>

    <div class="hblock">
      <div class="hhead"><b>Option 2 — Any fresh Linux VPS (Hetzner, DigitalOcean, Vultr)</b><button onclick={() => copy('vps', bundle!.vpsOneLiner)}>{copied === 'vps' ? '✓ copied' : 'copy'}</button></div>
      <pre>{bundle.vpsOneLiner}</pre>
    </div>

    <div class="hblock">
      <div class="hhead"><b>Option 3 — Cloud-Init (paste into Hetzner / DigitalOcean User Data)</b><button onclick={() => copy('ci', bundle!.cloudInitYaml)}>{copied === 'ci' ? '✓ copied' : 'copy'}</button></div>
      <pre>{bundle.cloudInitYaml}</pre>
    </div>

    <div class="hblock">
      <div class="hhead"><b>Option 4 — 1-Click PaaS & Home Server Templates</b></div>
      <div class="hlinks">
        <a href="https://github.com/kryptokrates/nektar/blob/master/deploy/hub/templates/coolify.compose.yaml" target="_blank" rel="noopener">Coolify Compose ↗</a>
        <a href="https://github.com/kryptokrates/nektar/blob/master/deploy/hub/templates/dokploy.compose.yaml" target="_blank" rel="noopener">Dokploy Compose ↗</a>
        <a href="https://github.com/kryptokrates/nektar/tree/master/deploy/hub/templates/umbrel" target="_blank" rel="noopener">Umbrel App ↗</a>
        <a href="https://github.com/kryptokrates/nektar/blob/master/deploy/hub/templates/casaos.yaml" target="_blank" rel="noopener">CasaOS Manifest ↗</a>
        <a href="https://github.com/kryptokrates/nektar/blob/master/deploy/hub/templates/render.yaml" target="_blank" rel="noopener">Render Blueprint ↗</a>
        <a href="https://github.com/kryptokrates/nektar/blob/master/deploy/hub/templates/fly.toml" target="_blank" rel="noopener">Fly.io Config ↗</a>
        <a href="https://github.com/kryptokrates/nektar/tree/master/deploy/hub/helm" target="_blank" rel="noopener">Kubernetes Helm ↗</a>
      </div>
    </div>
  {/if}
</div>

<div class="card">
  <h2>Connect your agent to your new Hub</h2>
  <p class="hmut">Once your Hub is online, paste its URL here to hot-add its hosts into this agent without restarting.</p>
  <div class="hrow">
    <input class="hin" bind:value={joinUrl} placeholder="https://hub.example.com" onkeydown={(e) => e.key === 'Enter' && connectHub()} />
    <button class="hbtn" onclick={connectHub} disabled={joinBusy || !joinUrl.trim()}>{joinBusy ? 'joining…' : 'Join Hub'}</button>
  </div>
</div>

<style>
  .hmut {
    color: #a89a8e;
    font-size: 0.84rem;
    line-height: 1.5;
    margin: 0.4rem 0 1rem;
  }
  .hcontrols {
    display: grid;
    gap: 0.65rem;
    margin-bottom: 1rem;
  }
  .hpills {
    display: flex;
    gap: 0.4rem;
    flex-wrap: wrap;
  }
  .hpill {
    background: #2e2620;
    border: 1px solid #3a2f28;
    color: #a89a8e;
    border-radius: 99px;
    padding: 0.35rem 0.75rem;
    font-size: 0.8rem;
    cursor: pointer;
  }
  .hpill.on {
    background: rgb(247 153 27 / 16%);
    border-color: #f7991b;
    color: #f7991b;
  }
  .hrow {
    display: flex;
    gap: 0.5rem;
    flex-wrap: wrap;
  }
  .hin,
  .hsel {
    background: #120e0c;
    border: 1px solid #3a2f28;
    border-radius: 8px;
    color: #ece7e4;
    padding: 0.5rem 0.75rem;
    font-size: 0.84rem;
  }
  .hin {
    flex: 1;
    min-width: 220px;
    font-family: ui-monospace, monospace;
  }
  .hblock {
    background: #1a1512;
    border: 1px solid #3a2f28;
    border-radius: 10px;
    padding: 0.8rem 0.95rem;
    margin-bottom: 0.8rem;
  }
  .hhead {
    display: flex;
    align-items: center;
    justify-content: space-between;
    font-size: 0.84rem;
    margin-bottom: 0.5rem;
  }
  .hhead button,
  .hbtn {
    background: #f7991b;
    color: #221e1d;
    border: 0;
    border-radius: 7px;
    padding: 0.35rem 0.75rem;
    font-size: 0.78rem;
    font-weight: 700;
    cursor: pointer;
  }
  pre {
    margin: 0;
    background: #120e0c;
    border: 1px solid #2e2620;
    border-radius: 7px;
    padding: 0.65rem;
    font-size: 0.76rem;
    overflow-x: auto;
    color: #ece7e4;
    white-space: pre-wrap;
    word-break: break-all;
  }
  .hlinks {
    display: flex;
    flex-wrap: wrap;
    gap: 0.65rem 1rem;
    font-size: 0.82rem;
  }
  .hlinks a {
    color: #f7991b;
    text-decoration: none;
  }
  .hlinks a:hover {
    text-decoration: underline;
  }
</style>
