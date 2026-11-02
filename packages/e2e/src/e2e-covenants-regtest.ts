// V2 MamemCheckpointBond + V3 DisclosureSale covenants — live regtest E2E
// (MAMEM_NETWORK=regtest): deploy each contract, spend through it, and let the
// node refuse the negative cases (early close / wrong wrapped / early refund).
import { PrivateKey, Transaction, LockingScript, P2PKH, Hash } from '@bsv/sdk'
import { buildP2PKHScript } from 'runar-sdk'
import { CheckpointBond, DisclosureSaleHost } from '@mamem/anchor'
import {
  TeranodeBroadcaster, TeranodeRPCTracker, ReserveFunder, bootstrapReserve,
  type RegtestConfig
} from '@mamem/chain'
import { existsSync, readFileSync } from 'node:fs'

let failures = 0
const check = (cond: boolean, name: string) => { console.log(`${cond ? '✔' : '✖'} ${name}`); if (!cond) failures++ }

function readEnv (p: URL): Record<string, string> {
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
const WIF = process.env.MAMEM_REGTEST_WIF ?? localEnv.MAMEM_REGTEST_WIF
if (!WIF) throw new Error('MAMEM_REGTEST_WIF missing: set it in mamem/.env')
const cfg: RegtestConfig = {
  rpcUrl: process.env.MAMEM_RPC_URL ?? 'http://localhost:9292/',
  rpcUser: process.env.MAMEM_RPC_USER ?? qsEnv.rpc_user ?? 'teranode',
  rpcPass: process.env.MAMEM_RPC_PASS ?? qsEnv.rpc_pass ?? '',
  propagationUrl: process.env.MAMEM_PROPAGATION_URL ?? 'http://localhost:8833/tx',
  assetUrl: process.env.MAMEM_ASSET_URL ?? 'http://localhost:8090/api/v1'
}
if (!cfg.rpcPass) throw new Error('rpc_pass missing')
const tracker = new TeranodeRPCTracker(cfg)
const broadcaster = new TeranodeBroadcaster(cfg.propagationUrl)
const auth = 'Basic ' + Buffer.from(`${cfg.rpcUser}:${cfg.rpcPass}`).toString('base64')
async function rpc (method: string, ...params: unknown[]): Promise<any> {
  const r = await fetch(cfg.rpcUrl, { method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: auth }, body: JSON.stringify({ jsonrpc: '1.0', id: 1, method, params }) })
  const j = await r.json() as any
  if (j.error) throw new Error(`rpc ${method}: ${JSON.stringify(j.error)}`)
  return j.result
}

const key = PrivateKey.fromWif(WIF)
const addr = key.toPublicKey().toAddress([0x6f])
const mineOne = async () => { await rpc('generatetoaddress', 1, addr) }
const opPkh = Buffer.from(key.toPublicKey().toHash()).toString('hex')

async function fundTo (script: string, sats: number): Promise<{ txid: string, vout: number, satoshis: number, script: string }> {
  const fundUtxo = await reserve.takeReserve()
  if (!fundUtxo) throw new Error('no funding utxo')
  const tx = new Transaction()
  tx.addInput({ sourceTransaction: fundUtxo.tx, sourceOutputIndex: fundUtxo.vout, unlockingScriptTemplate: new P2PKH().unlock(key) })
  tx.addOutput({ satoshis: sats, lockingScript: LockingScript.fromHex(script) })
  const change = fundUtxo.satoshis - sats - 100
  if (change > 546) tx.addOutput({ satoshis: change, lockingScript: LockingScript.fromHex(buildP2PKHScript(opPkh)) })
  await tx.sign()
  const res = await broadcaster.broadcast(tx)
  if (res.status !== 'success') throw new Error(`funding refused: ${res.description}`)
  reserve.markSpent(fundUtxo.tx.id('hex'), fundUtxo.vout)
  return { txid: tx.id('hex'), vout: 0, satoshis: sats, script }
}

// --- funding reserve ------------------------------------------------------
const reserve = new ReserveFunder(key, '.state/reserve-vault.json')
if (reserve.utxos.length < 3) {
  await bootstrapReserve(cfg, key, reserve, rpc, { treasuryFile: '.state/treasury-vault.json', outputs: 12 })
  await mineOne()
}

// ============================ V2: CheckpointBond ==========================
const release = (await tracker.currentHeight()) + 2
const bond = new CheckpointBond({ operatorPkh: opPkh, bondReleaseHeight: BigInt(release), maxFee: 10n })
let bondUtxo = await fundTo(bond.lockingScript(0n, ''), 5_000)
check(true, `V2 bond funded (${bondUtxo.txid.slice(0, 12)}…)`)
await mineOne()

// commit 1: root1 → continuation {n=1}
const root1 = 'aa'.repeat(32)
const c1 = bond.buildCommit(bondUtxo, key, root1, 1n)
const rc1 = await broadcaster.broadcast(c1.tx)
check(rc1.status === 'success', `V2 commit#1 accepted — ${rc1.description?.slice(0, 60) ?? 'ok'}`)
if (rc1.status !== 'success') { console.log('FATAL: commit#1 refused'); process.exit(1) }
await mineOne()

// commit 2 on the continuation: root2 → {n=2}
bondUtxo = { txid: c1.tx.id('hex'), vout: 0, satoshis: c1.tx.outputs[0]!.satoshis!, script: c1.tx.outputs[0]!.lockingScript.toHex() }
const st = bond.stateOf(bondUtxo.script)
check(st.n === 1n && st.lastRoot === root1, 'V2 on-chain state reads back {n:1, root1}')
const root2 = 'bb'.repeat(32)
const c2 = bond.buildCommit(bondUtxo, key, root2, 1n)
const rc2 = await broadcaster.broadcast(c2.tx)
check(rc2.status === 'success', `V2 commit#2 accepted — ${rc2.description?.slice(0, 60) ?? 'ok'}`)
if (rc2.status !== 'success') { console.log('FATAL: commit#2 refused'); process.exit(1) }
await mineOne()

bondUtxo = { txid: c2.tx.id('hex'), vout: 0, satoshis: c2.tx.outputs[0]!.satoshis!, script: c2.tx.outputs[0]!.lockingScript.toHex() }

// negative: close with a locktime BELOW bondReleaseHeight — the tx is final
// (locktime <= tip) so the node's script eval itself must refuse it.
const hNow = await tracker.currentHeight()
const early = bond.buildClose(bondUtxo, key, Math.min(hNow, release - 1), 1n)
const rEarly = await broadcaster.broadcast(early.tx)
check(rEarly.status !== 'success', `V2 close before release refused (${rEarly.description?.slice(0, 50) ?? 'refused'})`)

// mine until past release, then close for real
while ((await tracker.currentHeight()) < release) await mineOne()
const ok = bond.buildClose(bondUtxo, key, release, 1n)
const rOk = await broadcaster.broadcast(ok.tx)
check(rOk.status === 'success', `V2 close at release accepted — ${rOk.description?.slice(0, 60) ?? 'ok'}`)
await mineOne()

// ============================ V3: DisclosureSale ==========================
const WRAPPED = Buffer.from('disclosure-envelope-payload').toString('hex')
const H_WRAPPED = Buffer.from(Hash.sha256(Buffer.from(WRAPPED, 'hex'))).toString('hex')
const buyer = PrivateKey.fromRandom()
const buyerPkh = Buffer.from(buyer.toPublicKey().toHash()).toString('hex')
const saleDeadline = (await tracker.currentHeight()) + 2
const sale = new DisclosureSaleHost({
  sellerPkh: opPkh, price: 3_000n, hWrapped: H_WRAPPED,
  refundAfter: BigInt(saleDeadline), maxFee: 10n
})

// listing 1 → buy reveals wrapped
let saleUtxo = await fundTo(sale.lockingScript(), 5_000)
check(true, `V3 listing funded (${saleUtxo.txid.slice(0, 12)}…)`)
await mineOne()
const buyTx = sale.buildBuy(saleUtxo, WRAPPED, buyerPkh, 1_990n) // fee = 10
const rBuy = await broadcaster.broadcast(buyTx.tx)
check(rBuy.status === 'success', `V3 buy accepted (seller paid 3000, wrapped revealed) — ${rBuy.description?.slice(0, 50) ?? 'ok'}`)
if (rBuy.status === 'success') {
  check(buyTx.tx.outputs[0]!.satoshis === 3_000, 'V3 seller output = exact price')
}
await mineOne()

// listing 2 → wrong wrapped refused; early refund refused; then real refund
saleUtxo = await fundTo(sale.lockingScript(), 5_000)
await mineOne()
const bad = sale.buildBuy(saleUtxo, Buffer.from('decoy-payload').toString('hex'), buyerPkh, 1_990n)
const rBad = await broadcaster.broadcast(bad.tx)
check(rBad.status !== 'success', `V3 wrong wrapped refused (${rBad.description?.slice(0, 50) ?? 'refused'})`)

const earlyRefund = sale.buildRefund(saleUtxo, key, saleDeadline - 1, 4_990n)
const rER = await broadcaster.broadcast(earlyRefund.tx)
check(rER.status !== 'success', `V3 refund before deadline refused (${rER.description?.slice(0, 50) ?? 'refused'})`)

while ((await tracker.currentHeight()) < saleDeadline) await mineOne()
const refund = sale.buildRefund(saleUtxo, key, saleDeadline, 4_990n) // fee = 10
const rRef = await broadcaster.broadcast(refund.tx)
check(rRef.status === 'success', `V3 refund at deadline accepted — ${rRef.description?.slice(0, 60) ?? 'ok'}`)

console.log(failures === 0 ? 'V2+V3 covenant regtest E2E: PASS' : `V2+V3 covenant regtest E2E: ${failures} FAILURES`)
process.exit(failures === 0 ? 0 : 1)
