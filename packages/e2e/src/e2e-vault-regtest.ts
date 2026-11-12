// V1 NektarFeeVault covenant — live regtest E2E (NEKTAR_NETWORK=regtest):
// fund a vault UTXO → spend through the covenant → spend the continuation →
// negative: over-cap spend must be REFUSED by the node (real Teranode script
// evaluation, not the mock interpreter).
import { PrivateKey, Transaction, LockingScript, P2PKH } from '@bsv/sdk'
import { buildP2PKHScript } from 'runar-sdk'
import { FeeVault } from '@nektar/anchor'
import { TeranodeBroadcaster, TeranodeRPCTracker, ReserveFunder, bootstrapReserve, type RegtestConfig } from '@nektar/chain'
import { existsSync, readFileSync } from 'node:fs'

let failures = 0
const check = (cond: boolean, name: string) => {
  console.log(`${cond ? '✔' : '✖'} ${name}`)
  if (!cond) failures++
}

function readEnv(p: URL): Record<string, string> {
  const out: Record<string, string> = {}
  if (!existsSync(p)) return out
  for (const line of readFileSync(p, 'utf8').split(/\r?\n/)) {
    const m = line.trim().match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/)
    if (m) out[m[1]!] = m[2]!.trim()
  }
  return out
}
const localEnv = readEnv(new URL('../../../../.env', import.meta.url))
const qsEnv = readEnv(new URL('../../../../infra/teranode-quickstart/.env', import.meta.url))
const WIF = process.env.NEKTAR_REGTEST_WIF ?? localEnv.NEKTAR_REGTEST_WIF
if (!WIF) throw new Error('NEKTAR_REGTEST_WIF missing: set it in nektar/.env')
const cfg: RegtestConfig = {
  rpcUrl: process.env.NEKTAR_RPC_URL ?? 'http://localhost:9292/',
  rpcUser: process.env.NEKTAR_RPC_USER ?? qsEnv.rpc_user ?? 'teranode',
  rpcPass: process.env.NEKTAR_RPC_PASS ?? qsEnv.rpc_pass ?? '',
  propagationUrl: process.env.NEKTAR_PROPAGATION_URL ?? 'http://localhost:8833/tx',
  assetUrl: process.env.NEKTAR_ASSET_URL ?? 'http://localhost:8090/api/v1'
}
if (!cfg.rpcPass) throw new Error('rpc_pass missing')
const tracker = new TeranodeRPCTracker(cfg)
const broadcaster = new TeranodeBroadcaster(cfg.propagationUrl)
const auth = 'Basic ' + Buffer.from(`${cfg.rpcUser}:${cfg.rpcPass}`).toString('base64')
async function rpc(method: string, ...params: unknown[]): Promise<any> {
  const r = await fetch(cfg.rpcUrl, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: auth },
    body: JSON.stringify({ jsonrpc: '1.0', id: 1, method, params })
  })
  const j = (await r.json()) as any
  if (j.error) throw new Error(`rpc ${method}: ${JSON.stringify(j.error)}`)
  return j.result
}

const key = PrivateKey.fromWif(WIF)
const addr = key.toPublicKey().toAddress([0x6f])
const mineOne = async () => {
  await rpc('generatetoaddress', 1, addr)
}

// --- funding: reuse the persistent reserve, split fresh if empty ----------
const reserve = new ReserveFunder(key, '.state/reserve-vault.json')
if (reserve.utxos.length === 0) {
  await bootstrapReserve(cfg, key, reserve, rpc, { treasuryFile: '.state/treasury-vault.json', outputs: 12 })
  await mineOne()
}
const FUND_SATS = 10_000
const fundUtxo = await reserve.takeReserve(undefined, FUND_SATS + 2000)
check(!!fundUtxo, 'reserve has a spendable funding utxo')
if (!fundUtxo) {
  console.log('FATAL: no funding utxo')
  process.exit(1)
}

// --- the vault -----------------------------------------------------------
const owner = key // operator key = the WIF key for this E2E
const ownerPkh = Buffer.from(owner.toPublicKey().toHash()).toString('hex')
const destPkh = ownerPkh // self-pay for the test (dest = the anchoring wallet)
const vault = new FeeVault({
  destPkh,
  ownerPkh,
  perTxCap: 2_000n,
  windowCap: 5_000n,
  windowBlocks: 5n,
  maxFee: 10n
})

// fund tx: P2PKH utxo → vault locking script
const fundTx = new Transaction()
fundTx.addInput({ sourceTransaction: fundUtxo.tx, sourceOutputIndex: fundUtxo.vout, unlockingScriptTemplate: new P2PKH().unlock(key) })
fundTx.addOutput({ satoshis: FUND_SATS, lockingScript: LockingScript.fromHex(vault.lockingScript()) })
const changeSats = fundUtxo.satoshis - FUND_SATS - 100
if (changeSats > 546) fundTx.addOutput({ satoshis: changeSats, lockingScript: LockingScript.fromHex(buildP2PKHScript(ownerPkh)) })
await fundTx.sign()
const fundRes = await broadcaster.broadcast(fundTx)
check(fundRes.status === 'success', `vault funding tx broadcast (${fundTx.id('hex').slice(0, 12)}…) — ${fundRes.description ?? 'ok'}`)
reserve.markSpent(fundUtxo.tx.id('hex'), fundUtxo.vout)
if (fundRes.status !== 'success') {
  console.log('FATAL: funding failed')
  process.exit(1)
}
await mineOne()

let utxo = { txid: fundTx.id('hex'), vout: 0, satoshis: FUND_SATS, script: vault.lockingScript() }

// spend 1: pay 1000 ≤ perTxCap through the covenant
let lt = await tracker.currentHeight()
const s1 = vault.buildSpend(utxo, owner, 1_000n, lt)
const r1 = await broadcaster.broadcast(s1.tx)
check(r1.status === 'success', `spend 1 (1000 sats) accepted by node — ${r1.description ?? 'ok'}`)
if (r1.status !== 'success') {
  console.log('FATAL: first covenant spend refused')
  process.exit(1)
}
reserve.addOwned(s1.tx, 2) // the 1-sat change — cheap bookkeeping
await mineOne()

// spend 2: continuation carries window state on-chain
const cont = s1.tx.outputs[0]!
utxo = { txid: s1.tx.id('hex'), vout: 0, satoshis: cont.satoshis!, script: cont.lockingScript.toHex() }
lt = await tracker.currentHeight()
const s2 = vault.buildSpend(utxo, owner, 1_500n, lt)
const r2 = await broadcaster.broadcast(s2.tx)
check(r2.status === 'success', `spend 2 (continuation, 1500 sats) accepted — ${r2.description ?? 'ok'}`)
await mineOne()

// negative: over perTxCap on the FRESH continuation utxo — the node's script
// eval must refuse (not a double-spend refusal)
const cont2 = s2.tx.outputs[0]!
const utxo2 = { txid: s2.tx.id('hex'), vout: 0, satoshis: cont2.satoshis!, script: cont2.lockingScript.toHex() }
lt = await tracker.currentHeight()
const bad = vault.buildSpend(utxo2, owner, 5_000n, lt)
const rb = await broadcaster.broadcast(bad.tx)
check(rb.status !== 'success', `over-cap spend refused by node (${rb.description?.slice(0, 60) ?? 'refused'})`)

console.log(failures === 0 ? 'V1 vault regtest E2E: PASS' : `V1 vault regtest E2E: ${failures} FAILURES`)
process.exit(failures === 0 ? 0 : 1)
