// V1 — MamemGrothClaim VALIDITY settle on live Teranode regtest.
// The claim can only settle by co-spending the pinned Groth16-verifier UTXO
// in the same tx: input1's locking script IS the zkscript-generated BLS12-381
// Groth16 verifier for OUR sha256d-membership circuit (root eda3ef76..,
// index 5, baked as a circuit constant → the vk commits to the statement).
//
//   tx1: reserve → verifier UTXO (398KB lock, doc-push <root><idx> OP_2DROP)
//   tx2: reserve → MamemGrothClaim UTXO (state pins verifierOutpoint=tx1:0)
//   tx3: [claim, verifier] — settleValid + the Groth16 proof unlock;
//        outputs [ClaimVerdict @out0, payout @out1, change @out2].
//        If input1's proof verifies, the verdict exists under consensus.
//
// Negatives (before the good spend): claim-only spend rejected (prevouts
// pin fails), and claim+verifier spend with a tampered proof rejected
// (the verifier script itself fails under consensus).
//
// Run: node packages/e2e/src/e2e-grothclaim-regtest.mjs
import { PrivateKey, Transaction, LockingScript, UnlockingScript, P2PKH, Hash, Transaction as BsvTransaction } from '@bsv/sdk'
import { TeranodeBroadcaster, TeranodeRPCTracker, ReserveFunder, bootstrapReserve, utxoStatus } from '@mamem/chain'
import { MamemProvider } from '@mamem/anchor'
import { RunarContract, LocalSigner, serializeState } from 'runar-sdk'
import { existsSync, readFileSync } from 'node:fs'

const OUT = new URL('../../../tools/groth16-spike/out/', import.meta.url)
const lockHex = readFileSync(new URL('lock.hex', OUT), 'utf8').trim()
const unlockHex = readFileSync(new URL('unlock.hex', OUT), 'utf8').trim()
const params = JSON.parse(readFileSync(new URL('../../../circuits/membership/parameters.json', import.meta.url), 'utf8'))
const ROOT = params.root                                  // eda3ef76.. — the real bound statement
const RH = Buffer.from(Hash.sha256(Buffer.concat([
  Buffer.from(ROOT, 'hex'), Buffer.from([params.index])
]))).toString('hex') // sha256(root‖index) — the certified statement hash

let failures = 0
const check = (cond, name) => { console.log(`${cond ? '✔' : '✖'} ${name}`); if (!cond) failures++ }

function readEnv (p) {
  const out = {}
  if (!existsSync(p)) return out
  for (const line of readFileSync(p, 'utf8').split(/\r?\n/)) {
    const m = line.trim().match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/)
    if (m) out[m[1]] = m[2].trim()
  }
  return out
}
const localEnv = { ...readEnv(new URL('../../../.env', import.meta.url)), ...readEnv(new URL('../../../../.env', import.meta.url)) }
const qsEnv = readEnv(new URL('../../../infra/teranode-quickstart/.env', import.meta.url))
const WIF = process.env.MAMEM_REGTEST_WIF ?? localEnv.MAMEM_REGTEST_WIF
if (!WIF) throw new Error('MAMEM_REGTEST_WIF missing')
const cfg = {
  rpcUrl: process.env.MAMEM_RPC_URL ?? 'http://localhost:9292/',
  rpcUser: process.env.MAMEM_RPC_USER ?? qsEnv.rpc_user ?? 'teranode',
  rpcPass: process.env.MAMEM_RPC_PASS ?? qsEnv.rpc_pass ?? '',
  propagationUrl: process.env.MAMEM_PROPAGATION_URL ?? 'http://localhost:8833/tx',
  assetUrl: process.env.MAMEM_ASSET_URL ?? 'http://localhost:8090/api/v1'
}
if (!cfg.rpcPass) throw new Error('rpc_pass missing')
const broadcaster = new TeranodeBroadcaster(cfg.propagationUrl)
const tracker = new TeranodeRPCTracker(cfg)
const auth = 'Basic ' + Buffer.from(`${cfg.rpcUser}:${cfg.rpcPass}`).toString('base64')
const rpc = async (method, ...params) => {
  const r = await fetch(cfg.rpcUrl, { method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: auth }, body: JSON.stringify({ jsonrpc: '1.0', id: 1, method, params }) })
  const j = await r.json()
  if (j.error) throw new Error(`rpc ${method}: ${JSON.stringify(j.error)}`)
  return j.result
}
const key = PrivateKey.fromWif(WIF)
const addr = key.toPublicKey().toAddress([0x6f])
const opPkh = Buffer.from(key.toPublicKey().toHash()).toString('hex')
const mineOne = async () => { await rpc('generatetoaddress', 1, addr) }
const p2pkh = p => '76a914' + p + '88ac'

const reserve = new ReserveFunder(key, '.state/reserve-vault.json')
for (const u of [...reserve.utxos]) reserve.markSpent(u.tx.id('hex'), u.vout)
const tip0 = (await rpc('getblockchaininfo')).blocks
const scanned = await reserve.scanCoinbases(cfg, rpc, Math.max(1, tip0 - 150), tip0 - 101)
console.log(`reserve: scanned ${scanned} mature coinbase outputs (tip ${tip0})`)
if (reserve.utxos.length < 6) {
  await bootstrapReserve(cfg, key, reserve, rpc, { treasuryFile: '.state/treasury-vault.json', outputs: 16 })
  await mineOne()
}
const signer = new LocalSigner(WIF)
const pubKeyHex = await signer.getPublicKey()

// ── 1. deploy the Groth16 verifier UTXO ─────────────────────────────
console.log('── deploy verifier UTXO ──')
const LOCK_SATS = 20_000
const fund = await reserve.takeReserve()
const tx1 = new Transaction()
tx1.addInput({ sourceTransaction: fund.tx, sourceOutputIndex: fund.vout, unlockingScriptTemplate: new P2PKH().unlock(key) })
tx1.addOutput({ lockingScript: LockingScript.fromHex(lockHex), satoshis: LOCK_SATS })
const ch1 = fund.satoshis - LOCK_SATS - 25_000
if (ch1 > 0) tx1.addOutput({ lockingScript: LockingScript.fromHex(p2pkh(opPkh)), satoshis: ch1 })
await tx1.sign()
const b1 = await broadcaster.broadcast(tx1)
check(b1.status === 'success', `verifier deploy (${tx1.toHex().length / 2}B — ${tx1.id('hex').slice(0, 12)}…${b1.status !== 'success' ? ' ' + (b1.description ?? '').slice(0, 80) : ''})`)
if (b1.status !== 'success') process.exit(1)
reserve.markSpent(fund.tx.id('hex'), fund.vout)
reserve.addOwned(tx1, 1)
const verifierOp = Buffer.concat([Buffer.from(tx1.id('hex'), 'hex').reverse(), Buffer.from([0, 0, 0, 0])]).toString('hex')
// statement sanity: the doc-push prefix is literally <root><index> at the head
check(lockHex.slice(0, 2) === '20' && lockHex.slice(2, 66) === ROOT, `verifier doc-push binds root ${ROOT.slice(0, 12)}… on-chain`)

// ── 2. deploy the claim, bound to tx1:0 ─────────────────────────────
console.log('── deploy claim ──')
const art = JSON.parse(readFileSync(new URL('../../contracts/MamemGrothClaim.json', import.meta.url), 'utf8'))
const vArt = JSON.parse(readFileSync(new URL('../../contracts/ClaimVerdict.json', import.meta.url), 'utf8'))
const VCR = vArt.script
const QID = 'cc'.repeat(32)
const c = new RunarContract(art, [RH, ROOT, 16n, QID, opPkh, verifierOp, 1000n, VCR])
const mprovider = new MamemProvider(broadcaster, reserve, addr)
c.connect(mprovider, signer)
await c.deploy({ satoshis: 5000 })
const cUtxo = c.currentUtxo
const cOp = Buffer.concat([Buffer.from(cUtxo.txid, 'hex').reverse(), Buffer.from([0, 0, 0, 0])]).toString('hex')
console.log(`claim deployed ${cUtxo.txid.slice(0, 12)}… pins verifier ${tx1.id('hex').slice(0, 12)}…:0`)

// ── shared settle-tx builder ─────────────────────────────────────────
const encPush = h => { const l = h.length / 2
  if (l === 0) return '00'; if (l <= 75) return l.toString(16).padStart(2, '0') + h
  if (l <= 0xff) return '4c' + l.toString(16).padStart(2, '0') + h
  return '4d' + (l & 0xff).toString(16).padStart(2, '0') + ((l >> 8) & 0xff).toString(16).padStart(2, '0') + h }
const encNum = n => { if (n === 0n) return '00'; if (n >= 1n && n <= 16n) return (0x50 + Number(n)).toString(16)
  if (n === -1n) return '4f'
  const neg = n < 0n; let a = neg ? -n : n; const b = []
  while (a > 0n) { b.push(Number(a & 0xffn)); a >>= 8n }
  if (b[b.length - 1] & 0x80) b.push(neg ? 0x80 : 0); else if (neg) b[b.length - 1] |= 0x80
  return encPush(b.map(x => x.toString(16).padStart(2, '0')).join('')) }
const encArg = v => typeof v === 'bigint' ? encNum(v) : encPush(v)

const VERDICT_SATS = 10n, PAYOUT_SATS = 24_000n
const verdictScript = VCR + '6a' + serializeState(vArt.stateFields, { receiptHash: RH, journalRoot: ROOT, queryId: QID, payeePkh: opPkh })

const buildSettleTx = async ({ withVerifier, prevoutsArg, tamperUnlock }) => {
  const tx = new BsvTransaction()
  tx.addInput({ sourceTXID: cUtxo.txid, sourceOutputIndex: cUtxo.outputIndex, unlockingScript: UnlockingScript.fromHex('00'), sequence: 0xffffffff })
  if (withVerifier) tx.addInput({ sourceTXID: tx1.id('hex'), sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex('00'), sequence: 0xffffffff })
  tx.addOutput({ satoshis: Number(VERDICT_SATS), lockingScript: LockingScript.fromHex(verdictScript) })
  tx.addOutput({ satoshis: Number(PAYOUT_SATS), lockingScript: LockingScript.fromHex(p2pkh(opPkh)) })
  tx.addOutput({ satoshis: 1, lockingScript: LockingScript.fromHex(p2pkh(opPkh)) })
  const mIdx = 0 // single public method
  const { sigHex: opSig, preimageHex: pre } = c.computeOpPushTxWithCodeSep(tx, 0, cUtxo.script, cUtxo.satoshis, mIdx)
  const sigSub = c.getSubscriptForSigning(cUtxo.script, mIdx)
  const userSig = await signer.sign(tx.toHex(), 0, sigSub, cUtxo.satoshis)
  tx.inputs[0].unlockingScript = UnlockingScript.fromHex(
    c.buildStatefulPrefix(opSig, true)
    + encArg(userSig) + encArg(pubKeyHex) + encArg(prevoutsArg)
    + encArg(BigInt(LOCK_SATS)) + encArg(VERDICT_SATS) + encArg(PAYOUT_SATS)
    + encPush(opPkh) + encNum(1n) + encPush(pre))
  if (withVerifier) {
    let u = unlockHex
    if (tamperUnlock) { const b = Buffer.from(u, 'hex'); b[Math.floor(b.length / 2)] ^= 0x01; u = b.toString('hex') }
    tx.inputs[1].unlockingScript = UnlockingScript.fromHex(u)
  }
  return tx
}

// ── 3. negatives ─────────────────────────────────────────────────────
console.log('── negatives ──')
{
  const badTx = await buildSettleTx({ withVerifier: false, prevoutsArg: cOp, tamperUnlock: false })
  const r = await broadcaster.broadcast(badTx)
  check(r.status !== 'success', `claim-only spend rejected (prevouts pin — ${r.status}${r.description ? ': ' + r.description.slice(0, 60) : ''})`)
}
{
  const tamTx = await buildSettleTx({ withVerifier: true, prevoutsArg: cOp + verifierOp, tamperUnlock: true })
  const r = await broadcaster.broadcast(tamTx)
  check(r.status !== 'success', `tampered-proof settle rejected (verifier script fails — ${r.status}${r.description ? ': ' + r.description.slice(0, 60) : ''})`)
}

// ── 4. the validity settle ───────────────────────────────────────────
console.log('── settleValid: claim + verifier co-spend ──')
{
  const tx = await buildSettleTx({ withVerifier: true, prevoutsArg: cOp + verifierOp, tamperUnlock: false })
  const r = await broadcaster.broadcast(tx)
  check(r.status === 'success', `settleValid broadcast (${tx.id('hex').slice(0, 12)}…${r.status !== 'success' ? ' ' + (r.description ?? '').slice(0, 80) : ''})`)
  if (r.status === 'success') {
    mprovider.markAccepted?.(tx)
    // the ClaimVerdict UTXO at out0 is the consensus-certified statement
    const vs = await utxoStatus(cfg, tx.id('hex'), 0).catch(() => 'unknown')
    console.log(`  verdict UTXO (${tx.id('hex').slice(0, 12)}…:0) status: ${vs}`)
  }
}

await mineOne()
const tip = await tracker.currentHeight()
console.log(`mined to height ${tip}`)
console.log(`\n${failures === 0 ? 'ALL PASS' : failures + ' FAILURES'} — Groth16 validity claim on regtest`)
process.exit(failures === 0 ? 0 : 1)
