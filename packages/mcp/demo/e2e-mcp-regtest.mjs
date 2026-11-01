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
    const r = await client.callTool({ name, arguments: args }, undefined, { timeout: 120_000 })
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

// ZK membership (Groth16 over the anchored Poseidon root)
const q = { nonce: randomBytes(32).toString('hex'), verifierId: 'e2e-auditor' }
const t0 = Date.now()
const zkm = await call('proveMembership', { shard: S, seq: 1, ...q })
const proveMs = Date.now() - t0
const vm = await call('verifyMembership', { shard: S, seq: 1, bytes: zkm.lzkmem, ...q })
check(vm.ok === true, `ZK membership: Groth16 proof (${zkm.lzkmem.length / 2} B, ${proveMs} ms) verifies against the SPV-checked anchor ${vm.anchorTxid?.slice(0, 12)}…`)
const vmReplay = await call('verifyMembership', { shard: S, seq: 1, bytes: zkm.lzkmem, ...q })
check(vmReplay.ok === false && /nullifier/i.test(vmReplay.reason ?? ''), `ZK membership replay rejected (${vmReplay.reason})`)
const tampered = zkm.lzkmem.slice(0, -2) + (zkm.lzkmem.endsWith('00') ? '01' : '00')
const vmBad = await call('verifyMembership', { shard: S, seq: 1, bytes: tampered, nonce: randomBytes(32).toString('hex'), verifierId: 'e2e-auditor' }).catch(e => ({ ok: false, reason: e.message }))
check(vmBad.ok === false, `tampered / re-targeted membership proof rejected (${String(vmBad.reason).slice(0, 60)})`)

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
await client.close()

console.log(`  TXIDS ${Object.entries(txids).map(([k, v]) => `${k}=${v}`).join(' ')}`)
console.log(fails ? `\nE2E MCP FAIL (${fails})` : '\nE2E MCP PASS')
process.exit(fails ? 1 : 0)
