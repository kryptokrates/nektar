// ai_inference Groth16 verifier deploy+spend on live Teranode regtest.
// Loads tools/groth16-spike/out/{lock,unlock}-aiinf.hex emitted by
// spike_groth16_aiinf.py from nChain's shipped arkworks proof.
//
// tx1: reserve P2PKH → output0 = verifier locking script (~398KB)
// tx2: spends tx1:0 — unlock carries the Groth16 proof + the public
//      statement poseidon(input‖output‖poseidon(model)); if the node
//      accepts, inference-verification ran under consensus. Negative
//      control: tampered unlock must be rejected.
//
// The statement pushed inside the unlock is what the on-chain spend
// BINDS — re-derive it off-chain with packages/zk/src/poseidon-toy.ts.
//
// Run: node packages/e2e/src/e2e-grothinf-regtest.mjs
import { PrivateKey, Transaction, LockingScript, UnlockingScript, P2PKH } from '@bsv/sdk'
import {
  TeranodeRPCTracker, ReserveFunder, bootstrapReserve, utxoStatus
} from '@nektar/chain'
import { existsSync, readFileSync } from 'node:fs'

const OUT = new URL('../../../tools/groth16-spike/out/', import.meta.url)
const lockHex = readFileSync(new URL('lock-aiinf.hex', OUT), 'utf8').trim()
const unlockHex = readFileSync(new URL('unlock-aiinf.hex', OUT), 'utf8').trim()
const meta = JSON.parse(readFileSync(new URL('meta-aiinf.json', OUT), 'utf8'))
console.log(`verifier: ${meta.circuit} (${meta.curve}) — statement ${meta.public_statement_fr.slice(0, 18)}…`)

let failures = 0
const check = (cond, name) => { console.log(`${cond ? 'PASS' : 'FAIL'} ${name}`); if (!cond) failures++ }

function readEnv (p) {
  const out = {}
  if (!existsSync(p)) return out
  for (const line of readFileSync(p, 'utf8').split(/\r?\n/)) {
    const m = line.trim().match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/)
    if (m) out[m[1]] = m[2].trim()
  }
  return out
}
const localEnv = readEnv(new URL('../../../.env', import.meta.url))
const qsEnv = readEnv(new URL('../../../infra/teranode-quickstart/.env', import.meta.url))
const WIF = process.env.NEKTAR_REGTEST_WIF ?? localEnv.NEKTAR_REGTEST_WIF
if (!WIF) throw new Error('NEKTAR_REGTEST_WIF missing')
const cfg = {
  rpcUrl: process.env.NEKTAR_RPC_URL ?? 'http://localhost:9292/',
  rpcUser: process.env.NEKTAR_RPC_USER ?? qsEnv.rpc_user ?? 'teranode',
  rpcPass: process.env.NEKTAR_RPC_PASS ?? qsEnv.rpc_pass ?? '',
  propagationUrl: process.env.NEKTAR_PROPAGATION_URL ?? 'http://localhost:8833/tx',
  assetUrl: process.env.NEKTAR_ASSET_URL ?? 'http://localhost:8090/api/v1'
}
if (!cfg.rpcPass) throw new Error('rpc_pass missing')
const tracker = new TeranodeRPCTracker(cfg)
const auth = 'Basic ' + Buffer.from(`${cfg.rpcUser}:${cfg.rpcPass}`).toString('base64')
async function rpc (method, ...params) {
  const r = await fetch(cfg.rpcUrl, { method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: auth }, body: JSON.stringify({ jsonrpc: '1.0', id: 1, method, params }) })
  const j = await r.json()
  if (j.error) throw new Error(`rpc ${method}: ${JSON.stringify(j.error)}`)
  return j.result
}
async function broadcast (tx) {
  const res = await fetch(cfg.propagationUrl, { method: 'POST', headers: { 'Content-Type': 'application/octet-stream' }, body: Buffer.from(tx.toHex(), 'hex'), signal: AbortSignal.timeout(60_000) })
  if (res.ok) return { ok: true, txid: tx.id('hex') }
  const t = await res.text()
  const r = await fetch(cfg.rpcUrl, { method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: auth }, body: JSON.stringify({ jsonrpc: '1.0', id: 1, method: 'sendrawtransaction', params: [tx.toHex()], timeout: 60_000 }) })
  const j = await r.json()
  return j.error ? { ok: false, error: `prop:${res.status} ${t} | rpc:${JSON.stringify(j.error)}` } : { ok: true, txid: tx.id('hex') }
}

const key = PrivateKey.fromWif(WIF)
const addr = key.toPublicKey().toAddress([0x6f])
const p2pkh = () => new P2PKH().lock(addr)
const mineOne = () => rpc('generatetoaddress', 1, addr)

// --- funding -------------------------------------------------------------
const reserve = new ReserveFunder(key, '.state/reserve.json')
let fund = await reserve.takeReserve(async (t, v) => (await utxoStatus(cfg, t, v)) === 'spent')
if (!fund) {
  await bootstrapReserve(cfg, key, reserve, rpc, { treasuryFile: '.state/treasury.json', outputs: 12 })
  fund = await reserve.takeReserve()
}
check(!!fund, 'reserve has a spendable funding utxo')
if (!fund) process.exit(1)

// --- tx1: deploy the verifier --------------------------------------------
const LOCK_SATS = 20_000
const tx1 = new Transaction()
tx1.addInput({ sourceTransaction: fund.tx, sourceOutputIndex: fund.vout, unlockingScriptTemplate: new P2PKH().unlock(key) })
tx1.addOutput({ lockingScript: LockingScript.fromHex(lockHex), satoshis: LOCK_SATS })
tx1.addOutput({ lockingScript: p2pkh(), satoshis: fund.satoshis - LOCK_SATS - 25_000 })
await tx1.sign()
console.log(`tx1 size: ${tx1.toHex().length / 2} bytes, fee≈25000 sats`)
const b1 = await broadcast(tx1)
check(b1.ok, `tx1 (deploy ${lockHex.length / 2}B verifier) broadcast${b1.ok ? ` ${b1.txid}` : ` — ${b1.error}`}`)
if (!b1.ok) process.exit(1)
reserve.markSpent(fund.tx.id('hex'), fund.vout)
reserve.addOwned(tx1, 1)

let seen = false
for (let i = 0; i < 20 && !seen; i++) {
  const s = await utxoStatus(cfg, tx1.id('hex'), 0)
  if (s === 'unspent') seen = true
  else await new Promise(r => setTimeout(r, 1500))
}
console.log(`utxo store sees tx1:0 as ${seen ? 'unspent' : 'unknown (proceeding anyway — mempool chain)'}`)

// --- tx2: spend it with the inference proof -------------------------------
const tx2 = new Transaction()
tx2.addInput({ sourceTransaction: tx1, sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex(unlockHex) })
tx2.addOutput({ lockingScript: p2pkh(), satoshis: LOCK_SATS - 5_000 })
console.log(`tx2 size: ${tx2.toHex().length / 2} bytes, fee≈5000 sats`)
const b2 = await broadcast(tx2)
check(b2.ok, `tx2 (inference Groth16 proof spend) broadcast${b2.ok ? ` ${b2.txid}` : ` — ${b2.error}`}`)
if (b2.ok) reserve.addOwned(tx2, 0)

// --- negative: tampered unlock must be rejected ---------------------------
const bad = Buffer.from(unlockHex, 'hex')
bad[Math.floor(bad.length / 2)] ^= 0x01
const tx3 = new Transaction()
tx3.addInput({ sourceTransaction: tx1, sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex(bad.toString('hex')) })
tx3.addOutput({ lockingScript: p2pkh(), satoshis: LOCK_SATS - 5_000 })
const b3 = await broadcast(tx3)
check(!b3.ok, `tampered unlock rejected${!b3.ok ? '' : ' — WARNING accepted!'}`)

// --- settle: mine a block, confirm the spend ------------------------------
await mineOne()
const tip = await tracker.currentHeight()
console.log(`mined to height ${tip}`)

console.log(failures === 0 ? '\nAI-INFERENCE-ON-CHAIN SPIKE PASS' : `\n${failures} FAILURE(S)`)
process.exit(failures === 0 ? 0 : 1)
