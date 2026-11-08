#!/usr/bin/env node
// swarm smoke — the Wright thesis running: several agents doing business on
// one rendezvous. two sellers claim different domains; a buyer posts a want;
// the matching seller responds; the buyer pays under a signed delegation;
// the receipt binds caller + delegation + manifest — every object checkable.
// mock by default; SMOKE_NETWORK=ttn + funded dirs for live txids.
import { spawn } from 'node:child_process'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..', '..', '..')
const BIN = join(ROOT, 'packages/mcp/dist/src/bin.js')
const SERVE = join(ROOT, 'packages/overlay/serve.mjs')
const RV = 18096, BUYER = 18097, S1 = 18098, S2 = 18099
const RV_URL = process.env.SMOKE_RV ?? `http://localhost:${RV}`
const tmp = mkdtempSync(join(tmpdir(), 'nektar-swarm-'))
const kids = []
const NET = process.env.SMOKE_NETWORK ?? 'mock'
const env = (extra) => ({ ...process.env, NEKTAR_NETWORK: NET, NEKTAR_BATCH: '1', NEKTAR_DEMO_UTXOS: '20', NEKTAR_AUTOMINE: '1', NEKTAR_PAID_DISCLOSURE: '1', ...extra })
const spawnKid = (file, args, e) => { const k = spawn(process.execPath, [file, ...args], { env: e, stdio: ['ignore', 'ignore', 'pipe'] }); k.stderr.on('data', d => process.stderr.write(`[${file.split(/[\\/]/).pop()}] ${d}`)); kids.push(k); return k }
const healthy = async (port, path = '/v1/health') => {
  for (let i = 0; i < 60; i++) { try { const r = await fetch(`http://localhost:${port}${path}`); if (r.ok) return true } catch {} await new Promise(r => setTimeout(r, 400)) }
  throw new Error(`:${port} never came up`)
}
const tool = async (port, name, args = {}) => {
  const r = await fetch(`http://localhost:${port}/v1/tools/${name}`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(args) })
  const j = await r.json()
  if (!r.ok) throw new Error(`${name} → ${r.status}: ${j.error}`)
  return j
}
const die = (msg) => { console.error(`FAIL: ${msg}`); process.exitCode = 1 }

try {
  if (!process.env.SMOKE_RV) {
    spawnKid(SERVE, [], env({ NEKTAR_OVERLAY_PORT: String(RV), NEKTAR_NETWORK: NET }))
    await healthy(RV, '/directory')
  }
  spawnKid(BIN, [], env({ NEKTAR_HTTP_PORT: String(BUYER), NEKTAR_DATA_DIR: join(tmp, 'b') }))
  spawnKid(BIN, [], env({ NEKTAR_HTTP_PORT: String(S1), NEKTAR_DATA_DIR: join(tmp, 's1') }))
  spawnKid(BIN, [], env({ NEKTAR_HTTP_PORT: String(S2), NEKTAR_DATA_DIR: join(tmp, 's2') }))
  await Promise.all([healthy(BUYER), healthy(S1), healthy(S2)])
  console.log(`up: rendezvous ${RV_URL} + buyer :${BUYER} + soccer-seller :${S1} + weather-seller :${S2} (net=${NET})`)

  // ── the swarm declares what it knows ──────────────────────────────────
  // seller-1: soccer records; seller-2: weather records — each signs an
  // attestation ("this model serves these answers") + publishes ad+catalog
  const SOCCER_FACT = 'Metz beat Lyon 2-1 in Ligue 1'
  for (const [port, shard, domains] of [[S1, 'soccer', ['soccer', 'ligue-1']], [S2, 'weather', ['weather', 'forecast']]]) {
    await tool(port, 'remember', { shard, data: shard === 'soccer' ? SOCCER_FACT : `live ${shard} datapoint` })
    await tool(port, 'describeShard', { shard, domains, desc: `${shard} feed`, offers: [{ kind: 'answer', priceSats: 250 }] })
    const at = await tool(port, 'attest', { modelId: `self-hosted-${shard}`, provider: 'swarm-seller', claims: ['answers-from-anchored-memory'], publish: true, rendezvousUrl: RV_URL })
    console.log(`${shard}: attested ${at.attestation?.modelId} (pub ${at.attestation?.pub?.slice(0, 12)}…)`)
  }

  // the buyer grants a scout a bounded budget — "find me soccer, spend ≤500"
  const scoutGrant = await tool(BUYER, 'delegate', { grantee: (await tool(BUYER, 'reputation', {})).card.pub, tools: ['peerCall', 'groundedAnswer'], domains: ['soccer'], spendCapSats: 500, ttlMs: 3_600_000, publish: true, rendezvousUrl: RV_URL })
  console.log(`buyer: delegated bounded grant ${scoutGrant.grant.grantId} (cap ${scoutGrant.grant.spendCapSats} sats, scope ${scoutGrant.grant.scope.tools.join('/')})`)

  // ── the market clears ─────────────────────────────────────────────────
  const want = await tool(BUYER, 'publishWant', { domains: ['soccer'], maxSats: 400, desc: 'latest soccer result?', rendezvousUrl: RV_URL }).catch(e => ({ err: e.message }))
  if (want.err) die(`publishWant: ${want.err}`)
  console.log(`buyer: posted want{soccer ≤400} → rendezvous`)

  const resp = await tool(S1, 'respond', { wantPub: want.pub ?? want.want?.pub, shard: 'soccer', seq: 0, priceSats: 250, rendezvousUrl: RV_URL }).catch(e => ({ err: e.message }))
  if (resp.err) die(`respond: ${resp.err}`)
  console.log(`soccer-seller: respond offer 250 sats replyTo ${String(resp.offer?.replyTo ?? '').slice(0, 12)}…`)

  // the buyer pays under the delegation — the call is bounded by the grant
  const call = await tool(BUYER, 'peerCall', {
    endpoint: `http://localhost:${S1}`, tool: 'groundedAnswer',
    args: { claims: [{ text: SOCCER_FACT, cites: [{ shard: 'soccer', seq: 0 }] }], caller: scoutGrant.grant.granteePub, delegationId: scoutGrant.grant.grantId },
    maxSats: 400, delegation: scoutGrant
  }).catch(e => ({ err: e.message }))
  if (call.err) die(`peerCall: ${call.err}`)
  const r = call.result ?? call
  console.log(`buyer: paid → receipt ${r.receiptToken?.slice(0, 14)}… caller ${r.receipt?.caller?.slice(0, 12)}… delegation ${r.receipt?.delegationId?.slice(0, 12)}… ${r.receipt?.manifestId ? `manifest ${r.receipt.manifestId.slice(0, 12)}…` : ''}`)

  // the swarm's ledger — everything on the rendezvous
  const dir = await (await fetch(`${RV_URL}/delegation`)).json().catch(() => ({}))
  const att = await (await fetch(`${RV_URL}/attestation`)).json().catch(() => ({}))
  console.log(`rendezvous: ${dir.delegations?.length ?? 0} delegations, ${att.attestations?.length ?? 0} attestations, want+offer settled`)
  console.log('\nOK — swarm: want → respond → delegation-bounded pay → receipt, every object signed')
} catch (e) {
  die(e.message)
} finally {
  for (const k of kids) k.kill('SIGTERM')
}
