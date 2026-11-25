<script lang="ts">
  interface Props {
    network: string
    feeAddress: string | null
    sats: number
    receiptCount: number
    hasPolicy: boolean
    onRefresh: () => Promise<void>
    onOpenHub: () => void
  }
  let { network, feeAddress, sats, receiptCount, hasPolicy, onRefresh, onOpenHub }: Props = $props()

  let dockTab = $state<'dock' | 'proxy' | 'mcp'>('dock')
  let copied = $state('')
  let faucetBusy = $state(false)
  let faucetMsg = $state('')
  let desktopBusy = $state(false)
  let desktopMsg = $state('')
  let recallBusy = $state(false)
  let recallRes = $state<{ items: number; shards: string[]; verifyUrl?: string; err?: string } | null>(null)
  let policyBusy = $state(false)
  let policyMsg = $state('')
  let dismissed = $state(typeof localStorage !== 'undefined' && localStorage.getItem('nektar-onboard-dismissed') === '1')

  const origin = typeof window !== 'undefined' ? window.location.origin : 'http://127.0.0.1:18420'

  async function copy(id: string, text: string) {
    await navigator.clipboard.writeText(text).catch(() => {})
    copied = id
    setTimeout(() => (copied = ''), 1500)
  }

  function dismiss() {
    dismissed = true
    try {
      localStorage.setItem('nektar-onboard-dismissed', '1')
    } catch {}
  }

  async function runLiveRecall() {
    recallBusy = true
    recallRes = null
    try {
      const r = await fetch('/v1/tools/recall', {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: '{}'
      })
      const j = (await r.json()) as { shards?: string[]; items?: Array<{ shard: string; seq: number; text?: string }>; error?: string }
      if (!r.ok) {
        recallRes = { items: 0, shards: [], err: j.error ?? `HTTP ${r.status}` }
        return
      }
      const items = j.items ?? []
      let verifyUrl: string | undefined
      if (items.length > 0 && items[0].text) {
        const g = await fetch('/v1/tools/groundedAnswer', {
          method: 'POST',
          headers: { 'content-type': 'application/json' },
          body: JSON.stringify({
            question: 'What is nektar?',
            claims: [{ text: items[0].text.slice(0, 140), cites: [{ shard: items[0].shard, seq: items[0].seq }] }]
          })
        })
        if (g.ok) {
          const gj = (await g.json()) as { verifyUrl?: string; share?: { verifyUrl?: string } }
          verifyUrl = gj.verifyUrl ?? gj.share?.verifyUrl
        }
      }
      recallRes = { items: items.length, shards: j.shards ?? [], verifyUrl }
      await onRefresh()
    } catch (e) {
      recallRes = { items: 0, shards: [], err: String(e) }
    } finally {
      recallBusy = false
    }
  }

  async function requestFaucet() {
    faucetBusy = true
    faucetMsg = ''
    try {
      const r = await fetch('/v1/onboard/faucet', { method: 'POST' })
      const j = (await r.json().catch(() => ({}))) as { txid?: string; sats?: number; error?: string }
      if (!r.ok) {
        faucetMsg = j.error ?? `faucet failed (${r.status})`
        return
      }
      faucetMsg = `✓ received ${j.sats ?? 10000} sats (tx ${String(j.txid ?? '').slice(0, 12)}…)`
      await onRefresh()
    } catch (e) {
      faucetMsg = String(e)
    } finally {
      faucetBusy = false
    }
  }

  async function fundFromDesktop() {
    desktopBusy = true
    desktopMsg = ''
    try {
      const r = await fetch('/v1/wallet/fund-desktop', {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ sats: 10000 })
      })
      const j = (await r.json().catch(() => ({}))) as { txid?: string; satoshis?: number; error?: string }
      if (!r.ok) {
        desktopMsg = j.error ?? `desktop fund failed (${r.status})`
        return
      }
      desktopMsg = `✓ received ${j.satoshis ?? 10000} sats from BSV Desktop (${String(j.txid ?? '').slice(0, 10)}…)`
      await onRefresh()
    } catch (e) {
      desktopMsg = String(e)
    } finally {
      desktopBusy = false
    }
  }

  async function approvePreset(preset: 'buyer' | 'seller' | 'both') {
    policyBusy = true
    policyMsg = ''
    try {
      const body: Record<string, unknown> = { preset }
      if (network === 'main') {
        const ok = confirm('MAINNET spends real BSV. Approve starter policy?')
        if (!ok) return
        body.confirm = 'I_UNDERSTAND'
      }
      const post = (tok?: string) =>
        fetch('/v1/onboard/policy-preset', {
          method: 'POST',
          headers: { 'content-type': 'application/json', ...(tok ? { authorization: `Bearer ${tok}` } : {}) },
          body: JSON.stringify(body)
        })
      let r = await post(sessionStorage.getItem('nektar-operator-token') ?? undefined)
      if (r.status === 401 || r.status === 403) {
        const tok = prompt('operator token (NEKTAR_OPERATOR_TOKEN)')
        if (!tok) return
        sessionStorage.setItem('nektar-operator-token', tok)
        r = await post(tok)
      }
      const j = (await r.json().catch(() => ({}))) as { policyId?: string; error?: string }
      if (!r.ok) {
        policyMsg = j.error ?? `approval failed (${r.status})`
        return
      }
      policyMsg = `✓ approved ${j.policyId ?? preset}`
      await onRefresh()
    } catch (e) {
      policyMsg = String(e)
    } finally {
      policyBusy = false
    }
  }

  const dockCmd = 'nektar dock'
  const proxyCmd = `export OPENAI_BASE_URL="${origin}/v1/proxy/openai/v1"\n# or Anthropic: export ANTHROPIC_BASE_URL="${origin}/v1/proxy/anthropic"`
  const mcpSnippet = `${origin}/mcp`
</script>

{#if !dismissed}
  <section class="ob card">
    <div class="ob-top">
      <div>
        <div class="ob-tag">Quickstart</div>
        <h2>Get your agent live in 60 seconds</h2>
      </div>
      <button class="ob-close" onclick={dismiss} title="Hide quickstart">✕</button>
    </div>

    <div class="ob-grid">
      <div class="step">
        <div class="sh"><b>1. Dock your existing agent</b></div>
        <div class="pills">
          <button class="pill" class:on={dockTab === 'dock'} onclick={() => (dockTab = 'dock')}>Auto-Dock</button>
          <button class="pill" class:on={dockTab === 'proxy'} onclick={() => (dockTab = 'proxy')}>Any Agent (L0 Proxy)</button>
          <button class="pill" class:on={dockTab === 'mcp'} onclick={() => (dockTab = 'mcp')}>MCP URL</button>
        </div>
        {#if dockTab === 'dock'}
          <p class="smut">Auto-wires Claude Code, Cursor, VS Code, Windsurf, Codex, Zed, Cline, Continue, Goose, OpenCode, Gemini, Hermes &amp; OpenClaw.</p>
          <div class="cmd">
            <code>{dockCmd}</code>
            <button onclick={() => copy('dock', dockCmd)}>{copied === 'dock' ? '✓' : 'copy'}</button>
          </div>
        {:else if dockTab === 'proxy'}
          <p class="smut">Zero code changes for LangGraph, CrewAI, n8n, Open WebUI, Aider or any custom loop:</p>
          <div class="cmd">
            <code>{proxyCmd}</code>
            <button onclick={() => copy('proxy', proxyCmd)}>{copied === 'proxy' ? '✓' : 'copy'}</button>
          </div>
        {:else}
          <p class="smut">Streamable HTTP MCP endpoint (supports OAuth 2.1 + Bearer auth):</p>
          <div class="cmd">
            <code>{mcpSnippet}</code>
            <button onclick={() => copy('mcp', mcpSnippet)}>{copied === 'mcp' ? '✓' : 'copy'}</button>
          </div>
        {/if}
      </div>

      <div class="step">
        <div class="sh">
          <b>2. Try verifiable memory</b>
          {#if receiptCount > 0}<span class="okb">✓ {receiptCount} receipt{receiptCount === 1 ? '' : 's'}</span>{/if}
        </div>
        <p class="smut">Recall the seeded <code>welcome</code> shard and issue your first checkable receipt.</p>
        <button class="abtn" onclick={runLiveRecall} disabled={recallBusy}>{recallBusy ? 'running…' : 'Run Recall + Receipt'}</button>
        {#if recallRes}
          <p class="sres">
            {#if recallRes.err}<span class="bad">{recallRes.err}</span>
            {:else}✓ {recallRes.items} item(s) on [{recallRes.shards.join(', ')}]{#if recallRes.verifyUrl} · <a href={recallRes.verifyUrl} target="_blank" rel="noopener">open receipt ↗</a>{/if}{/if}
          </p>
        {/if}
      </div>

      <div class="step">
        <div class="sh">
          <b>3. Fund anchoring & deals</b>
          {#if sats > 0}<span class="okb">✓ {sats} sats</span>{/if}
        </div>
        {#if network === 'testnet'}
          <p class="smut">Fund via Testnet Faucet or 1-click BRC-29 from BSV Desktop{feeAddress ? ` (${feeAddress.slice(0, 12)}…)` : ''}.</p>
          <div class="rowbtns">
            <button class="abtn" onclick={requestFaucet} disabled={faucetBusy}>{faucetBusy ? 'requesting…' : '1-Click Testnet Faucet'}</button>
            <button class="abtn ghost" onclick={fundFromDesktop} disabled={desktopBusy}>{desktopBusy ? 'connecting…' : 'Fund from BSV Desktop'}</button>
          </div>
          {#if faucetMsg}<p class="sres">{faucetMsg}</p>{/if}
          {#if desktopMsg}<p class="sres">{desktopMsg}</p>{/if}
        {:else}
          <p class="smut">Fee address: <code>{feeAddress ?? 'initializing…'}</code></p>
          <div class="rowbtns">
            <button class="abtn" onclick={fundFromDesktop} disabled={desktopBusy}>{desktopBusy ? 'connecting…' : 'Fund from BSV Desktop'}</button>
            {#if feeAddress}<button class="abtn ghost" onclick={() => copy('addr', feeAddress!)}>{copied === 'addr' ? 'copied ✓' : 'Copy Address'}</button>{/if}
          </div>
          {#if desktopMsg}<p class="sres">{desktopMsg}</p>{/if}
        {/if}
      </div>

      <div class="step">
        <div class="sh">
          <b>4. Trade & grow the network</b>
          {#if hasPolicy}<span class="okb">✓ policy active</span>{/if}
        </div>
        <p class="smut">Approve a starter spending policy, or graduate to an independent Hub Node.</p>
        <div class="rowbtns">
          <button class="abtn ghost" onclick={() => approvePreset('buyer')} disabled={policyBusy}>Approve Buyer</button>
          <button class="abtn ghost" onclick={() => approvePreset('both')} disabled={policyBusy}>Buyer + Seller</button>
          <button class="abtn" onclick={onOpenHub}>Deploy Hub Node →</button>
        </div>
        {#if policyMsg}<p class="sres">{policyMsg}</p>{/if}
      </div>
    </div>
  </section>
{/if}

<style>
  .ob {
    border-color: rgb(247 153 27 / 35%);
  }
  .ob-top {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    margin-bottom: 1rem;
  }
  .ob-tag {
    color: #f7991b;
    font-size: 0.72rem;
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: 0.12em;
  }
  .ob h2 {
    margin: 0.15rem 0 0;
    font-size: 1.08rem;
  }
  .ob-close {
    background: none;
    border: 0;
    color: #a89a8e;
    cursor: pointer;
    font-size: 0.95rem;
  }
  .ob-grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 0.9rem;
  }
  @media (max-width: 820px) {
    .ob-grid {
      grid-template-columns: 1fr;
    }
  }
  .step {
    background: #1a1512;
    border: 1px solid #3a2f28;
    border-radius: 10px;
    padding: 0.9rem 1rem;
    display: grid;
    gap: 0.55rem;
    align-content: start;
  }
  .sh {
    display: flex;
    align-items: center;
    justify-content: space-between;
    font-size: 0.88rem;
  }
  .okb {
    font-size: 0.74rem;
    color: #4cc26b;
    background: rgb(76 194 107 / 12%);
    padding: 0.1rem 0.5rem;
    border-radius: 99px;
  }
  .smut {
    margin: 0;
    color: #a89a8e;
    font-size: 0.8rem;
    line-height: 1.4;
  }
  .pills {
    display: flex;
    gap: 0.35rem;
    flex-wrap: wrap;
  }
  .pill {
    background: #2e2620;
    border: 1px solid #3a2f28;
    color: #a89a8e;
    border-radius: 99px;
    padding: 0.2rem 0.6rem;
    font-size: 0.74rem;
    cursor: pointer;
  }
  .pill.on {
    background: rgb(247 153 27 / 16%);
    border-color: #f7991b;
    color: #f7991b;
  }
  .cmd {
    display: flex;
    gap: 0.5rem;
    align-items: center;
    background: #120e0c;
    border: 1px solid #3a2f28;
    border-radius: 8px;
    padding: 0.45rem 0.65rem;
  }
  .cmd code {
    flex: 1;
    font-size: 0.75rem;
    color: #ece7e4;
    overflow-x: auto;
    white-space: pre-wrap;
    word-break: break-all;
  }
  .cmd button,
  .abtn {
    background: #f7991b;
    color: #221e1d;
    border: 0;
    border-radius: 7px;
    padding: 0.38rem 0.75rem;
    font-size: 0.78rem;
    font-weight: 700;
    cursor: pointer;
    width: fit-content;
  }
  .abtn.ghost {
    background: #2e2620;
    color: #ece7e4;
    border: 1px solid #3a2f28;
  }
  .abtn:disabled {
    opacity: 0.5;
    cursor: default;
  }
  .rowbtns {
    display: flex;
    flex-wrap: wrap;
    gap: 0.45rem;
  }
  .sres {
    margin: 0;
    font-size: 0.78rem;
    color: #4cc26b;
  }
  .sres a {
    color: #f7991b;
  }
  .bad {
    color: #e5534b;
  }
</style>
