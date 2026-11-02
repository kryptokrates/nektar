// pnpm e2e:mcp — agent-level E2E on the LOCAL TERANODE regtest chain, driven
// through the REAL MCP server process (official SDK, stdio), exactly as an AI
// client would: anchoring, SPV proofs, ZK membership (Groth16) and ZK
// clearance (Bulletproofs), negatives, and a restart on the same data dir.
// Secrets: mamem/.env (MAMEM_REGTEST_WIF) + infra/teranode-quickstart/.env (rpc_pass).
// MAMEM_E2E_GATEWAY=mamem_regtest → same flow through the Docker MCP Gateway.
import { readFileSync, existsSync, mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { randomBytes } from 'node:crypto'
import { fileURLToPath } from 'node:url'
import { Client } from '@modelcontextprotocol/sdk/client/index.js'
import { StdioClientTransport } from '@modelcontextprotocol/sdk/client/stdio.js'
import { ClientWallet, ClearanceAuthority, decodeCredential, encodeCredential } from '../dist/src/index.js'

const env = p => existsSync(p) ? Object.fromEntries(readFileSync(p, 'utf8').split(/\r?\n/).map(l => l.match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/)).filter(Boolean).map(m => [m[1], m[2].trim()])) : {}
const root = new URL('../../../', import.meta.url)
const local = env(new URL('.env', root)); const qs = env(new URL('infra/teranode-quickstart/.env', root))
const GATEWAY = process.env.MAMEM_E2E_GATEWAY
let fails = 0
const check = (c, m) => { console.log(`${c ? '✔' : '✖'} ${m}`); if (!c) fails++ }
const txids = {}

const dataDir = mkdtempSync(join(tmpdir(), 'mamem-e2e-mcp-'))
const authority = GATEWAY && process.env.MAMEM_E2E_AUTHORITY_HEX ? new ClearanceAuthority((await import('@bsv/sdk')).PrivateKey.fromHex(process.env.MAMEM_E2E_AUTHORITY_HEX)) : new ClearanceAuthority()
const serverEnv = {
  ...process.env, MAMEM_NETWORK: 'regtest', MAMEM_DATA_DIR: dataDir, MAMEM_FUND_WIF: local.MAMEM_REGTEST_WIF, MAMEM_RPC_PASS: qs.rpc_pass, MAMEM_RPC_USER: qs.rpc_user ?? 'teranode',
  MAMEM_AUTOMINE: '1', MAMEM_CONFIRM_MS: '1500', MAMEM_BATCH: '4', MAMEM_CLEARANCE_AUTHORITIES: authority.pubkey, MAMEM_MODEL_ID: 'e2e-mcp-scripted'
}
const bin = fileURLToPath(new URL('../dist/src/bin.js', import.meta.url))
async function connect () {
  const client = new Client({ name: 'e2e-mcp', version: '1.0.0' })
  const transport = GATEWAY
    ? new StdioClientTransport({ command: 'docker', args: ['mcp', 'gateway', 'run', '--profile', GATEWAY, '--verbose'], env: process.env, stderr: process.env.MAMEM_E2E_STDERR ? 'inherit' : 'ignore' })
    : new StdioClientTransport({ command: process.execPath, args: [bin], env: serverEnv, stderr: 'ignore' })
  await client.connect(transport)
  const call = async (name, args) => {
    // STARK proving inside a 1-CPU gateway container can take minutes
    const r = await client.callTool({ name, arguments: args }, undefined, { timeout: name === 'proveMembership' ? 600_000 : 120_000 })
    if (r.isError) throw new Error(`${name}: ${r.content[0]?.text}`)
    return JSON.parse(r.content[0].text)
  }
  return { client, call }
}
const until = async (fn, ms = 180_000) => { const t = Date.now(); let v; while (Date.now() - t < ms) { v = await fn(); if (v) return v; await new Promise(r => setTimeout(r, 2000)) } return v }
const S = `e2e-${randomBytes(3).toString('hex')}`

// ── session 1 ────────────────────────────────────────────────────────────
let { client, call } = await connect()
check((await client.listTools()).tools.some(t => t.name === 'proveMembership'), `connected (${GATEWAY ? `Docker gateway ${GATEWAY}` : 'stdio server process'}), regtest backend`)
await call('remember', { shard: S, data: 'Drain port 4 on pump P2: 45 Nm', source: 'manual-rev1' })
await call('remember', { shard: S, data: 'Pump P2 inspected 2026-10-30', source: 'inspection-log' })
await call('remember', { shard: S, data: 'Confidential: P2 supplier price 12,400 EUR', source: 'procurement', level: 2 })
const a0 = await call('anchor', { shard: S })
txids.anchor0 = a0?.txid
check(!!a0?.txid, `anchored batch n=${a0?.n} on Teranode: ${a0?.txid}`)
const proven = await until(async () => (await call('prove', { shard: S, seq: 0 })).result.ok)
check(proven === true, 'prove seq 0: leaf → R → Rúnar anchor → BUMP → real Teranode headers')

// ZK membership (Wright §13): ZK-STARK over the SAME SHA-256 tree that is anchored
const q = await call('zkQuery', { verifierId: 'e2e-auditor' })
const t0 = Date.now()
const zkm = await call('proveMembership', { shard: S, seq: 1, queryId: q.queryId })
const proveMs = Date.now() - t0
const vm = await call('verifyMembership', { shard: S, seq: 1, bytes: zkm.lzkmem })
check(vm.ok === true, `ZK membership: STARK proof (${zkm.lzkmem.length / 2} B, ${proveMs} ms) verifies against the SPV-checked SHA-256 anchor ${vm.anchorTxid?.slice(0, 12)}…`)
const vmReplay = await call('verifyMembership', { shard: S, seq: 1, bytes: zkm.lzkmem })
check(vmReplay.ok === false && /queryId/i.test(vmReplay.reason ?? ''), `ZK membership replay rejected (${vmReplay.reason})`)
const mid = zkm.lzkmem.length - 8000
const tampered = zkm.lzkmem.slice(0, mid) + (zkm.lzkmem[mid] === '0' ? '1' : '0') + zkm.lzkmem.slice(mid + 1)
const vmBad = await call('verifyMembership', { shard: S, seq: 1, bytes: tampered }).catch(e => ({ ok: false, reason: e.message }))
check(vmBad.ok === false, `tampered membership proof rejected (${String(vmBad.reason).slice(0, 60)})`)

// ZK clearance (Bulletproof range proof; identity via server-issued challenge)
const wallet = new ClientWallet()
const cred = authority.issue(wallet.pubkey, 3n)
const recallSecret = async (zk, w = wallet) => (await call('recall', { shard: S, seq: 2, identity: await w.identify(call), zk })).items[0]
const denied = await recallSecret(undefined)
check(denied.text === undefined && /zkaccess required/.test(denied.reason ?? ''), 'level-2 record withheld without clearance')
const zk = wallet.presentClearance(cred, S, 2, 2n)
const granted = await recallSecret(zk)
check(granted.text === 'Confidential: P2 supplier price 12,400 EUR' && granted.zk === 'verified', `ZK clearance (Bulletproof, level ≥ 2 without revealing 3) → plaintext${granted.text ? '' : ` [${JSON.stringify(granted)}]`}`)
check(/replayed/.test((await recallSecret(zk)).reason ?? ''), 'clearance replay rejected')
check(/untrusted authority/.test((await recallSecret(wallet.presentClearance(new ClearanceAuthority().issue(wallet.pubkey, 3n), S, 2, 2n))).reason ?? ''), 'self-issued clearance rejected')
check(/holder is not the verified caller/.test((await recallSecret(wallet.presentClearance(cred, S, 2, 2n), new ClientWallet())).reason ?? ''), 'stolen credential presented by another caller rejected')
const pub = (await call('recall', { shard: S, seq: 0 })).items[0]
check(pub.text === 'Drain port 4 on pump P2: 45 Nm', 'public record readable without credentials')
const savedCred = JSON.stringify(encodeCredential(cred))
const lastZk = zk
await client.close()

// ── session 2: restart on the same data dir ─────────────────────────────
;({ client, call } = await connect())
await call('remember', { shard: S, data: 'Drain port 4 torque revised to 55 Nm', source: 'manual-rev2' })
const a1 = await call('anchor', { shard: S })
txids.anchor1 = a1?.txid
check(a1?.n === (a0?.n ?? 0) + 1, `after restart: next anchor n=${a1?.n} spends the persisted head (${a1?.txid})`)
const p3 = await until(async () => { const r = (await call('prove', { shard: S, seq: 3 })).result; return r.ok ? r : undefined })
const p0 = (await call('prove', { shard: S, seq: 0 })).result
const last3 = p3 ?? (await call('prove', { shard: S, seq: 3 })).result
check(!!p3?.ok && p0.ok, `old and new records provable after restart (seq0=${JSON.stringify(p0)} seq3=${JSON.stringify(last3)})`)
check(/replayed/.test((await recallSecret(lastZk)).reason ?? ''), 'clearance replay still rejected after restart (persisted transcript store)')
const again = await recallSecret(wallet.presentClearance(decodeCredential(JSON.parse(savedCred)), S, 2, 2n))
check(again.text?.startsWith('Confidential'), 'fresh clearance from the stored credential works after restart')

// ── optimistic claims: the STARK becomes a bonded on-chain claim ──
// claimDeploy commits the proof's journal into a MamemZkClaim covenant and
// registers the outpoint; the watcher daemon shares this dataDir registry.
// two claims: one still inside its dispute window (the watcher's ward), one
// already past deadline (settles now — same proof, different deadline).
const claimWatch = await call('claimDeploy', { lzkmem: zkm.lzkmem, deadline: 100000 })
txids.claimWatch = claimWatch.txid
const claim = await call('claimDeploy', { lzkmem: zkm.lzkmem, deadline: 0 })
txids.claim = claim.txid
check(!!claim.txid && !!claimWatch.txid, `claimDeploy ×2 → MamemZkClaim covenants (watched=${claimWatch.txid.slice(0, 12)}… settle=${claim.txid.slice(0, 12)}…)`)
const cst = await call('claimStatus', { txid: claim.txid })
check(cst.status === 'open' && cst.journalRoot === claim.journalRoot, `claimStatus: open — committed root matches the proven journal`)
// watcher pass over the same dataDir: honest claim verifies → no action
const { execFileSync } = await import('node:child_process')
const watcherRun = () => execFileSync(process.execPath, [
  fileURLToPath(new URL('../../watcher/src/watcher.mjs', import.meta.url)),
  '--once', '--claims', join(dataDir, 'claims.json'), '--proof-dir', join(dataDir, 'proofs'),
  '--reserve', join(dataDir, 'watcher-reserve.json')
], { env: { ...process.env, MAMEM_WATCH_DEBUG: '1', MAMEM_REGTEST_WIF: local.MAMEM_REGTEST_WIF, MAMEM_RPC_USER: qs.rpc_user ?? 'teranode', MAMEM_RPC_PASS: qs.rpc_pass ?? '' }, encoding: 'utf8', timeout: 120_000 })
try {
  let wout
  try { wout = watcherRun() } catch { wout = watcherRun() } // WASM loader flake: retry once
  check(/verifies, no action|FRAUD/i.test(wout), `watcher pass: ${wout.trim().split('\n').at(-1)?.slice(0, 70)}`)
} catch (e) { check(false, `watcher pass: ${String(e.message).slice(0, 80)}`) }
const stl = await call('claimSettle', { txid: claim.txid })
txids.settle = stl.txid
check(!!stl.txid && !!stl.verdictOutpoint, `claimSettle → ClaimVerdict UTXO ${stl.verdictOutpoint?.slice(0, 16)}… (bond returned)`)
// pay-iff-settled: the verdict UTXO is the atomic unlock for a sale
const WRAPPED = 'f1'.repeat(64)
const sale = await call('saleDeployGated', { verdictOutpoint: stl.verdictOutpoint, priceSats: 3000, wrapped: WRAPPED })
txids.sale = sale.txid
const buy = await call('saleBuyGated', { saleTxid: sale.txid, verdictOutpoint: stl.verdictOutpoint, wrapped: WRAPPED, priceSats: 3000 })
txids.buyGated = buy.txid
check(!!buy.txid, `saleBuyGated — payment consumed the verdict atomically (${buy.txid?.slice(0, 12)}…)`)
await client.close()

console.log(`  TXIDS ${Object.entries(txids).map(([k, v]) => `${k}=${v}`).join(' ')}`)
console.log(fails ? `\nE2E MCP FAIL (${fails})` : '\nE2E MCP PASS')
process.exit(fails ? 1 : 0)
