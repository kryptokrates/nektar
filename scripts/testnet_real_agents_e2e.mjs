#!/usr/bin/env node
/**
 * Real-world heterogeneous agent harness drivers (Node.js v24, zero external deps):
 *   - Agent 2 (Beta):  REAL Claude Code / Codex CLI stdio MCP client (`nektar.exe mcp`) + native `nektar hook`!
 *   - Agent 3 (Gamma): REAL Cursor / OpenAI / Anthropic Streamable HTTP MCP (`/mcp`) + L0 Transparent LLM Proxy!
 *   - Agent 4 (Delta): REAL NAT-only A2A Agent Protocol (`/.well-known/agent.json` + JSON-RPC 2.0 `POST /a2a`)!
 */

import { spawn, spawnSync } from 'node:child_process'
import http from 'node:http'
import readline from 'node:readline'

const [, , agentRole, phase, ...rest] = process.argv

function die(msg) {
  console.error(msg)
  process.exit(1)
}

// =============================================================================
// AGENT 2 (BETA): REAL CLAUDE CODE / CODEX STDIO MCP CLIENT + NATIVE HOOKS
// =============================================================================
class StdioMCPClient {
  constructor(exePath, dataDir) {
    this.proc = spawn(exePath, ['mcp', '-data-dir', dataDir], { stdio: ['pipe', 'pipe', 'pipe'] })
    this.rl = readline.createInterface({ input: this.proc.stdout })
    this.pending = new Map()
    this.nextId = 1
    this.stderr = ''
    this.proc.stderr.on('data', (d) => { this.stderr += d.toString() })
    this.rl.on('line', (line) => {
      const s = line.trim()
      if (!s) return
      try {
        const msg = JSON.parse(s)
        const resolver = this.pending.get(msg.id)
        if (resolver) {
          this.pending.delete(msg.id)
          resolver(msg)
        }
      } catch {}
    })
  }
  request(method, params = {}, timeoutMs = 45000) {
    const id = this.nextId++
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id)
        reject(new Error(`MCP stdio timeout on ${method}; stderr=${this.stderr}`))
      }, timeoutMs)
      this.pending.set(id, (msg) => {
        clearTimeout(timer)
        if (msg.error) reject(new Error(`MCP error ${method}: ${JSON.stringify(msg.error)}`))
        else resolve(msg.result)
      })
      this.proc.stdin.write(JSON.stringify({ jsonrpc: '2.0', id, method, params }) + '\n')
    })
  }
  async callTool(name, args = {}) {
    const res = await this.request('tools/call', { name, arguments: args })
    if (res.isError) throw new Error(`MCP tool ${name} error: ${JSON.stringify(res)}`)
    const txt = res.content?.[0]?.text ?? '{}'
    return JSON.parse(txt)
  }
  close() {
    this.rl.close()
    this.proc.stdin.end()
    this.proc.kill()
  }
}

async function runBetaCodexMCP(phase, exePath, dataDir, runTag) {
  const mcp = new StdioMCPClient(exePath, dataDir)
  try {
    const init = await mcp.request('initialize', {
      protocolVersion: '2024-11-05',
      capabilities: {},
      clientInfo: { name: 'codex-claude-live-e2e', version: '1.0.0' },
    })
    const tools = await mcp.request('tools/list', {})
    const toolNames = (tools.tools || []).map((t) => t.name)
    if (!toolNames.includes('want') || !toolNames.includes('sell') || !toolNames.includes('recall')) {
      die(`Codex/Claude MCP tools/list missing expected tools: ${toolNames.join(',')}`)
    }

    if (phase === 'setup') {
      // Run real SessionStart hook!
      const ss = spawnSync(exePath, ['hook', 'session-start', '-data-dir', dataDir], { encoding: 'utf8' })
      if (ss.status !== 0 || !ss.stdout.includes('welcome')) {
        die(`Beta native hook session-start failed: ${ss.stdout} ${ss.stderr}`)
      }
      // Remember & anchor TWO shards over real stdio MCP (competing on ops @ 55 sats + selling bsv @ 65 sats)!
      const r1 = await mcp.callTool('remember', {
        shard: 'codex-ops',
        text: `CodexBeta[${runTag}]: Prod Kubernetes etcd snapshots rotate every 15m using zstd-3 compression.`,
        source: 'codex:beta',
        autoAnchor: true,
      })
      const s1 = await mcp.callTool('sell', {
        shard: 'codex-ops',
        domains: ['ops'],
        priceSats: 55,
        description: 'Codex Beta cheaper ops runbook',
      })
      const r2 = await mcp.callTool('remember', {
        shard: 'codex-bsv',
        text: `CodexBeta[${runTag}]: GorillaPool Arcade testnet accepts EF transactions at 100 sat/kB.`,
        source: 'codex:beta',
        autoAnchor: true,
      })
      const s2 = await mcp.callTool('sell', {
        shard: 'codex-bsv',
        domains: ['bsv'],
        priceSats: 65,
        description: 'Codex Beta BSV Arcade policy',
      })
      console.log(JSON.stringify({
        ok: true,
        serverInfo: init.serverInfo,
        mcpToolsCount: toolNames.length,
        hookSessionStartOK: true,
        opsAnchorTxid: r1.anchorTxid,
        bsvAnchorTxid: r2.anchorTxid,
        listings: [s1.shard, s2.shard],
      }))
      return
    }

    if (phase === 'trade') {
      // Buy "nat" knowledge from NAT-only Delta (Agent 4, A2A behind Hub /bbox) over real stdio MCP!
      const t0 = Date.now()
      const deal = await mcp.callTool('want', {
        domains: ['nat'],
        question: 'NAT-only A2A agents bbox relay',
        maxSats: 150,
        targetShard: 'bought/nat',
      })
      const totalMs = Date.now() - t0
      if (deal.status !== 'settled' || !String(deal.deliveredText || '').includes('bbox')) {
        die(`Codex Beta stdio MCP want failed: ${JSON.stringify(deal)}`)
      }
      // Run real UserPromptSubmit prefetch hook!
      const pref = spawnSync(exePath, ['hook', 'prefetch', '-data-dir', dataDir], {
        input: JSON.stringify({ prompt: 'bbox' }),
        encoding: 'utf8',
      })
      if (pref.status !== 0 || !pref.stdout.includes('bbox')) {
        die(`Beta native hook prefetch failed: ${pref.stdout} ${pref.stderr}`)
      }
      // Ground answer via stdio MCP & verify with real Stop hook `nektar hook verify-receipt`!
      const rec = await mcp.callTool('recall', { shard: 'bought/nat' })
      const item = rec.items[rec.items.length - 1]
      const ga = await mcp.callTool('groundedAnswer', {
        claims: [{ text: item.text, cites: [{ shard: item.shard, seq: item.seq }] }],
        autoAnchor: true,
      })
      const vr = spawnSync(exePath, ['hook', 'verify-receipt', '-data-dir', dataDir], {
        input: ga.footer,
        encoding: 'utf8',
      })
      const parsedHook = JSON.parse(vr.stdout)
      if (!parsedHook.verified?.[0]?.ok) {
        die(`Beta native hook verify-receipt failed: ${vr.stdout}`)
      }
      console.log(JSON.stringify({
        ok: true,
        deal: {
          status: deal.status,
          priceSats: deal.priceSats,
          firstOfferMs: deal.firstOfferMs,
          totalMs,
          paymentTxid: deal.paymentTxid,
          deliveredText: deal.deliveredText,
        },
        hookPrefetchOK: true,
        receiptToken: ga.receiptToken,
        hookVerdict: parsedHook.verified[0].verdict,
      }))
    }
  } finally {
    mcp.close()
  }
}

// =============================================================================
// AGENT 3 (GAMMA): REAL STREAMABLE HTTP MCP (`/mcp`) + L0 OPENAI/ANTHROPIC PROXY
// =============================================================================
async function streamableMCPCall(baseUrl, method, params = {}, id = 1) {
  const resp = await fetch(`${baseUrl}/mcp`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Accept: 'application/json, text/event-stream',
    },
    body: JSON.stringify({ jsonrpc: '2.0', id, method, params }),
  })
  const text = await resp.text()
  if (!resp.ok) throw new Error(`Streamable HTTP MCP HTTP ${resp.status}: ${text}`)
  let payload = text.trim()
  if (resp.headers.get('content-type')?.includes('text/event-stream')) {
    const dataLine = text.split('\n').find((l) => l.startsWith('data: '))
    payload = dataLine ? dataLine.slice(6).trim() : '{}'
  }
  const msg = JSON.parse(payload)
  if (msg.error) throw new Error(`Streamable HTTP MCP error: ${JSON.stringify(msg.error)}`)
  return msg.result
}

async function streamableTool(baseUrl, name, args = {}) {
  const res = await streamableMCPCall(baseUrl, 'tools/call', { name, arguments: args })
  if (res.isError) throw new Error(`Streamable MCP tool ${name} failed: ${JSON.stringify(res)}`)
  return JSON.parse(res.content?.[0]?.text ?? '{}')
}

async function runGammaHTTPMCPAndProxy(phase, baseUrl, runTag, peerPubHex) {
  await streamableMCPCall(baseUrl, 'initialize', {
    protocolVersion: '2024-11-05',
    capabilities: {},
    clientInfo: { name: 'cursor-openai-l0-e2e', version: '1.0.0' },
  })

  if (phase === 'setup') {
    const r = await streamableTool(baseUrl, 'remember', {
      shard: 'gamma-agents',
      text: `GammaHTTPMCP[${runTag}]: MCP Streamable HTTP multiplexes concurrent AI agents cleanly over one daemon.`,
      source: 'cursor:gamma',
      autoAnchor: true,
    })
    const s = await streamableTool(baseUrl, 'sell', {
      shard: 'gamma-agents',
      domains: ['agents'],
      priceSats: 60,
      description: 'Gamma Streamable HTTP MCP architecture',
    })
    console.log(JSON.stringify({ ok: true, anchorTxid: r.anchorTxid, listing: s.shard }))
    return
  }

  if (phase === 'trade') {
    // 1. Competitive buy on "ops": Codex Beta (55 sats) MUST beat Hermes Alpha (70 sats) & Hub (90 sats)!
    const t0 = Date.now()
    const deal = await streamableTool(baseUrl, 'want', {
      domains: ['ops'],
      question: 'Kubernetes etcd snapshots rotation',
      maxSats: 150,
      targetShard: 'bought/ops',
    })
    const totalMs = Date.now() - t0
    if (deal.status !== 'settled' || deal.priceSats !== 55 || !String(deal.deliveredText || '').includes('zstd-3')) {
      die(`Gamma competitive want expected Beta (55 sats, zstd-3), got: ${JSON.stringify(deal)}`)
    }

    // 2. Real L0 OpenAI & Anthropic Transparent Proxy verification (zero-code SDK injection of purchased fact)!
    const oaResp = await fetch(`${baseUrl}/v1/proxy/openai/v1/chat/completions`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-Nektar-Shard': 'bought/ops' },
      body: JSON.stringify({ model: 'gpt-4o', messages: [{ role: 'user', content: 'How do etcd snapshots rotate?' }] }),
    })
    const oaJson = await oaResp.json()
    const anResp = await fetch(`${baseUrl}/v1/proxy/anthropic/v1/messages`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-Nektar-Shard': 'bought/ops' },
      body: JSON.stringify({ model: 'claude-3-7-sonnet', messages: [{ role: 'user', content: 'How do etcd snapshots rotate?' }] }),
    })
    const anJson = await anResp.json()
    if (!oaJson.injectedHasBoughtFact || !anJson.injectedHasBoughtFact) {
      die(`L0 Proxy did not inject purchased Testnet memory! oa=${JSON.stringify(oaJson)} an=${JSON.stringify(anJson)}`)
    }

    // 3. Borg BRC-369 Group Epoch Rotation via Streamable HTTP MCP!
    const st = await streamableTool(baseUrl, 'status', {})
    const gid = `borg-${runTag}`
    await streamableTool(baseUrl, 'borgGroup', { op: 'create', groupId: gid, name: 'Gamma Sec', pubHex: st.identityPub })
    await streamableTool(baseUrl, 'borgGroup', { op: 'add', groupId: gid, pubHex: peerPubHex, role: 'member' })
    await streamableTool(baseUrl, 'remember', { shard: `sec-${runTag}`, text: 'Epoch1 secret', borgGroup: gid, callerPub: st.identityPub })
    const rev = await streamableTool(baseUrl, 'borgGroup', { op: 'revoke', groupId: gid, pubHex: peerPubHex })
    if (rev.newEpoch !== 2) die(`Expected Borg epoch 2, got ${JSON.stringify(rev)}`)

    console.log(JSON.stringify({
      ok: true,
      deal: {
        status: deal.status,
        priceSats: deal.priceSats,
        firstOfferMs: deal.firstOfferMs,
        totalMs,
        paymentTxid: deal.paymentTxid,
        deliveredText: deal.deliveredText,
      },
      l0OpenAIProxyInjected: true,
      l0AnthropicProxyInjected: true,
      borgRevokedEpoch: rev.newEpoch,
    }))
  }
}

// =============================================================================
// AGENT 4 (DELTA-NAT): REAL A2A PROTOCOL (`/.well-known/agent.json` + `/a2a`)
// =============================================================================
async function a2aCall(baseUrl, method, params = {}, id = 1) {
  const resp = await fetch(`${baseUrl}/a2a`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ jsonrpc: '2.0', id, method, params }),
  })
  const msg = await resp.json()
  if (msg.error) throw new Error(`A2A ${method} error: ${JSON.stringify(msg.error)}`)
  return msg.result
}

async function runDeltaA2ANAT(phase, baseUrl, runTag) {
  const cardResp = await fetch(`${baseUrl}/.well-known/agent.json`)
  const card = await cardResp.json()
  if (card.name !== 'nektar' || !card.capabilities?.paidKnowledgeDisclosure) {
    die(`Delta A2A card invalid: ${JSON.stringify(card)}`)
  }

  if (phase === 'setup') {
    const r = await a2aCall(baseUrl, 'memory/remember', {
      shard: 'a2a-nat',
      text: `DeltaA2A[${runTag}]: NAT-only A2A agents settle BRC-369 trades over Hub bbox relay with zero open ports.`,
      source: 'a2a:delta-nat',
      autoAnchor: true,
    })
    const s = await a2aCall(baseUrl, 'market/sell', {
      shard: 'a2a-nat',
      domains: ['nat'],
      priceSats: 50,
      description: 'Delta A2A NAT-relayed knowledge',
    })
    console.log(JSON.stringify({ ok: true, a2aCardName: card.name, anchorTxid: r.anchorTxid, listing: s.shard }))
    return
  }

  if (phase === 'trade') {
    const peers = await a2aCall(baseUrl, 'market/peers', { refresh: true })
    if ((peers.peers || []).length < 5) {
      die(`Delta A2A expected >=5 peers on dir lane, got ${(peers.peers || []).length}`)
    }
    const t0 = Date.now()
    const deal = await a2aCall(baseUrl, 'market/want', {
      domains: ['bsv'],
      question: 'GorillaPool Arcade testnet EF 100 sat/kB',
      maxSats: 150,
      targetShard: 'bought/bsv',
    })
    const totalMs = Date.now() - t0
    if (deal.status !== 'settled' || !String(deal.deliveredText || '').includes('100 sat/kB')) {
      die(`Delta A2A want failed: ${JSON.stringify(deal)}`)
    }
    const rec = await a2aCall(baseUrl, 'memory/recall', { shard: 'bought/bsv' })
    const item = rec.items[rec.items.length - 1]
    const ga = await a2aCall(baseUrl, 'memory/answer', {
      claims: [{ text: item.text, cites: [{ shard: item.shard, seq: item.seq }] }],
      autoAnchor: true,
    })
    const ver = await a2aCall(baseUrl, 'memory/verify', { token: ga.receiptToken })
    if (!ver.ok) die(`Delta A2A receipt verify failed: ${JSON.stringify(ver)}`)
    console.log(JSON.stringify({
      ok: true,
      peersDiscovered: peers.peers.length,
      bountiesDiscovered: (peers.bounties || []).length,
      deal: {
        status: deal.status,
        priceSats: deal.priceSats,
        firstOfferMs: deal.firstOfferMs,
        totalMs,
        paymentTxid: deal.paymentTxid,
        deliveredText: deal.deliveredText,
      },
      receiptToken: ga.receiptToken,
      receiptVerdict: ver.verdict,
    }))
  }
}

// =============================================================================
// MOCK UPSTREAM LLM FOR GAMMA'S REAL L0 PROXY VERIFICATION
// =============================================================================
function runMockLLM(port) {
  const srv = http.createServer((req, res) => {
    let raw = ''
    req.on('data', (c) => { raw += c })
    req.on('end', () => {
      const hasFact = raw.includes('Nektar Verified Memory') && raw.includes('zstd-3')
      res.writeHead(200, { 'Content-Type': 'application/json' })
      res.end(JSON.stringify({ id: 'mock-llm', path: req.url, injectedHasBoughtFact: hasFact }))
    })
  })
  srv.listen(Number(port), '127.0.0.1', () => {
    console.log(`MOCK_LLM_READY:${port}`)
  })
}

async function main() {
  if (agentRole === 'mock-llm') return runMockLLM(phase)
  if (agentRole === 'beta') return runBetaCodexMCP(phase, rest[0], rest[1], rest[2])
  if (agentRole === 'gamma') return runGammaHTTPMCPAndProxy(phase, rest[0], rest[1], rest[2])
  if (agentRole === 'delta') return runDeltaA2ANAT(phase, rest[0], rest[1])
  die(`unknown role: ${agentRole}`)
}

main().catch((err) => {
  console.error(err?.stack || String(err))
  process.exit(1)
})
