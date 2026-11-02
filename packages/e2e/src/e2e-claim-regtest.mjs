// GOAL-v9 Stage 1 — MamemZkClaim full lifecycle on live Teranode regtest.
// Interpreter-green (claim-lifecycle.mjs, 10/10) → now through real mempool
// policy + block acceptance. One operator key plays all roles; tx locktime
// is always 0 so deadline=0 settles instantly and window=0 makes timeouts
// immediately claimable (same semantics as the mock eval).
import { PrivateKey, Transaction, LockingScript, P2PKH, Hash, Transaction as BsvTransaction, UnlockingScript } from '@bsv/sdk'
import { buildP2PKHScript } from 'runar-sdk'
import { MamemProvider } from '@mamem/anchor'
import { TeranodeBroadcaster, TeranodeRPCTracker, ReserveFunder, bootstrapReserve } from '@mamem/chain'
import { RunarContract, LocalSigner } from 'runar-sdk'
import { existsSync, readFileSync } from 'node:fs'

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
const broadcaster = new TeranodeBroadcaster(cfg.propagationUrl)
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

const reserve = new ReserveFunder(key, '.state/reserve-vault.json')
// Teranode has no utxo-status RPC — local marks go stale across runs.
// Purge everything, repopulate from mature coinbases (provably unspent),
// fall back to a treasury bootstrap split.
for (const u of [...reserve.utxos]) reserve.markSpent(u.tx.id('hex'), u.vout)
const tip = (await rpc('getblockchaininfo')).blocks
const scanned = await reserve.scanCoinbases(cfg, rpc, Math.max(1, tip - 150), tip - 101)
console.log(`reserve: scanned ${scanned} mature coinbase outputs (tip ${tip})`)
if (reserve.utxos.length < 6) {
  await bootstrapReserve(cfg, key, reserve, rpc, { treasuryFile: '.state/treasury-vault.json', outputs: 16 })
  await mineOne()
}
const provider = new MamemProvider(broadcaster, reserve, addr)
provider.onAttempt = tx => console.log(`  → broadcast ${tx.id('hex').slice(0, 10)} in:[${tx.inputs.map(i => (i.sourceTXID ?? i.sourceTransaction?.id('hex') ?? '?').slice(0, 8) + ':' + i.sourceOutputIndex).join(' ')}]`)
const signer = new LocalSigner(WIF)
const pubKeyHex = await signer.getPublicKey()

const art = JSON.parse(readFileSync(new URL('../../contracts/MamemZkClaim.json', import.meta.url), 'utf8'))
const Z = '00'.repeat(32)
const RH = 'aa'.repeat(32), JR = 'bb'.repeat(32), QID = 'cc'.repeat(32)
const p2pkh = p => '76a914' + p + '88ac'
const st0 = (over = {}) => ({ status: 0n, receiptHash: RH, journalRoot: JR, journalSize: 10n, queryId: QID, bondSats: 5000n, deadline: 1000n, responseDeadline: 0n, challengerPkh: Z, bisectLo: 0n, bisectHi: 4n, midCommit: Z, proverPkh: opPkh, maxFee: 1000n, window: 100n, stages: 4n, ...over })
const ctorFor = (deadline, window, stages = 4n) => [0n, RH, JR, 10n, QID, 5000n, deadline, 0n, Z, 0n, stages, Z, opPkh, 1000n, window, stages]
const deployClaim = async (deadline, window) => {
  const c = new RunarContract(art, ctorFor(deadline, window))
  c.connect(provider, signer)
  const d = await c.deploy({ satoshis: 5000 })
  return c
}
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

// Terminal methods: emitted script wants [codePart][opSig][args][changePKH][changeAmount][preimage][selector]
// + a trailing change output. Patch the prepared call like the mock eval does.
const callTerminal = async (c, method, args, outputs, changeAmt = 1n) => {
  const pubMethods = c.artifact.abi.methods.filter(m => m.isPublic)
  const mIdx = pubMethods.findIndex(m => m.name === method)
  const prepared = await c.prepareCall(method, args, { terminalOutputs: [...outputs, { scriptHex: p2pkh(opPkh), satoshis: Number(changeAmt) }] })
  prepared._methodNeedsChange = true
  prepared._changePKHHex = opPkh
  prepared._changeAmount = changeAmt
  const sigSub = c.getSubscriptForSigning(prepared._contractUtxo.script, mIdx)
  const sigs = {}
  const txHex = prepared.tx.toHex()
  for (const idx of prepared.sigIndices) sigs[idx] = await signer.sign(txHex, 0, sigSub, prepared._contractUtxo.satoshis)
  return c.finalizeCall(prepared, sigs)
}

// ── 1. settle (happy path, deadline=0) ─────────────────────────────
console.log('── settle happy path ──')
try {
  const c = await deployClaim(0n, 100n)
  const verdict = '6a' + RH + JR + QID
  const r = await callTerminal(c, 'settle', [null, pubKeyHex, 4988n], [
    { scriptHex: verdict, satoshis: 1 }, { scriptHex: p2pkh(opPkh), satoshis: 4988 }], 1n)
  check(!!r.txid, `settle broadcast (${r.txid?.slice(0, 12)}…)`)
} catch (e) { check(false, `settle: ${e.message.slice(0, 90)}`) }

// ── 2. dispute lifecycle: challenge → reveal → bisect → proveStage ──
console.log('── dispute: challenge → reveal → bisect → bisect → proveStage ──')
try {
  const c = await deployClaim(1000n, 0n) // window=0 → responseDeadline=0
  const ch = await c.call('challenge', [null, pubKeyHex], provider, signer, { newState: st0({ status: 1n, challengerPkh: opPkh, responseDeadline: 0n, window: 0n }) })
  check(!!ch.txid, `challenge (${ch.txid?.slice(0, 12)}…)`)
  const MID = 'dd'.repeat(32)
  const rv = await c.call('reveal', [null, pubKeyHex, MID], provider, signer, { newState: st0({ status: 2n, challengerPkh: opPkh, responseDeadline: 0n, window: 0n, midCommit: MID }) })
  check(!!rv.txid, `reveal (${rv.txid?.slice(0, 12)}…)`)
  const b1 = await c.call('bisect', [null, pubKeyHex, 1n], provider, signer, { newState: st0({ status: 1n, challengerPkh: opPkh, responseDeadline: 0n, window: 0n, bisectLo: 2n, bisectHi: 4n }) })
  check(!!b1.txid, `bisect [0,4]→[2,4] (${b1.txid?.slice(0, 12)}…)`)
  const MID2 = 'ee'.repeat(32)
  const rv2 = await c.call('reveal', [null, pubKeyHex, MID2], provider, signer, { newState: st0({ status: 2n, challengerPkh: opPkh, responseDeadline: 0n, window: 0n, bisectLo: 2n, bisectHi: 4n, midCommit: MID2 }) })
  check(!!rv2.txid, `reveal2 (${rv2.txid?.slice(0, 12)}…)`)
  const b2 = await c.call('bisect', [null, pubKeyHex, 0n], provider, signer, { newState: st0({ status: 2n, challengerPkh: opPkh, responseDeadline: 0n, window: 0n, bisectLo: 2n, bisectHi: 3n }) })
  check(!!b2.txid, `bisect [2,4]→[2,3] leaf (${b2.txid?.slice(0, 12)}…)`)
  // proveStage: manual 2-input tx — [claim, stage stand-in P2PKH]
  {
    const claimUtxo = c.currentUtxo
    const stageFund = await reserve.takeReserve()
    const stageTx = new Transaction()
    stageTx.addInput({ sourceTransaction: stageFund.tx, sourceOutputIndex: stageFund.vout, unlockingScriptTemplate: new P2PKH().unlock(key) })
    stageTx.addOutput({ satoshis: 2000, lockingScript: LockingScript.fromHex(p2pkh(opPkh)) })
    const sc = stageFund.satoshis - 2000 - 100
    if (sc > 0) stageTx.addOutput({ satoshis: sc, lockingScript: LockingScript.fromHex(p2pkh(opPkh)) })
    await stageTx.sign()
    const br = await broadcaster.broadcast(stageTx)
    if (br.status !== 'success') throw new Error('stage fund refused: ' + br.description)
    provider.markAccepted(stageTx) // book-keep: input spent + change owned
    const stageUtxo = { txid: stageTx.id('hex'), outputIndex: 0, satoshis: 2000, script: p2pkh(opPkh), parent: stageTx }

    const ourOp = Buffer.concat([Buffer.from(claimUtxo.txid, 'hex').reverse(), Buffer.from([0, 0, 0, 0])]).toString('hex')
    const stageOp = Buffer.concat([Buffer.from(stageUtxo.txid, 'hex').reverse(), Buffer.from([0, 0, 0, 0])]).toString('hex')
    const prevouts = ourOp + stageOp
    const tx = new BsvTransaction()
    tx.addInput({ sourceTXID: claimUtxo.txid, sourceOutputIndex: claimUtxo.outputIndex, unlockingScript: UnlockingScript.fromHex('00'), sequence: 0xffffffff })
    tx.addInput({ sourceTXID: stageUtxo.txid, sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex('00'), sequence: 0xffffffff })
    tx.addOutput({ satoshis: 4990, lockingScript: LockingScript.fromHex(p2pkh(opPkh)) })
    tx.addOutput({ satoshis: 1, lockingScript: LockingScript.fromHex(p2pkh(opPkh)) })
    const pubMethods = c.artifact.abi.methods.filter(m => m.isPublic)
    const mIdx = pubMethods.findIndex(m => m.name === 'proveStage')
    const { sigHex: opSig, preimageHex: preimage } = c.computeOpPushTxWithCodeSep(tx, 0, claimUtxo.script, claimUtxo.satoshis, mIdx)
    const sigSub = c.getSubscriptForSigning(claimUtxo.script, mIdx)
    const userSig = await signer.sign(tx.toHex(), 0, sigSub, claimUtxo.satoshis)
    const unlock = c.buildStatefulPrefix(opSig, true)
      + encArg(userSig) + encArg(pubKeyHex) + encArg(prevouts) + encArg(stageOp) + encArg(4990n)
      + encPush(opPkh) + encNum(1n)
      + encPush(preimage) + encNum(BigInt(mIdx))
    tx.inputs[0].unlockingScript = UnlockingScript.fromHex(unlock)
    // manual P2PKH for input1 — tx.sign() would need sourceTransaction on input0 too
    const stageSig = await signer.sign(tx.toHex(), 1, stageUtxo.script, stageUtxo.satoshis)
    tx.inputs[1].unlockingScript = UnlockingScript.fromHex(encPush(stageSig) + encPush(pubKeyHex))
    const pr = await broadcaster.broadcast(tx)
    check(pr.status === 'success', `proveStage leaf co-spend (${tx.id('hex').slice(0, 12)}…${pr.status !== 'success' ? ' ' + pr.description?.slice(0, 60) : ''})`)
  }
} catch (e) { check(false, `dispute path: ${e.message.slice(0, 90)}`) }

// ── 3. claimTimeout (challenger silent; window=0) ───────────────────
console.log('── claimTimeout ──')
try {
  const c = await deployClaim(1000n, 0n)
  await c.call('challenge', [null, pubKeyHex], provider, signer, { newState: st0({ status: 1n, challengerPkh: opPkh, responseDeadline: 0n, window: 0n }) })
  const r = await callTerminal(c, 'claimTimeout', [null, pubKeyHex, 4990n], [{ scriptHex: p2pkh(opPkh), satoshis: 4990 }], 1n)
  check(!!r.txid, `claimTimeout (${r.txid?.slice(0, 12)}…)`)
} catch (e) { check(false, `claimTimeout: ${e.message.slice(0, 90)}`) }

// ── 4. slashTimeout (prover silent after reveal) ────────────────────
console.log('── slashTimeout ──')
try {
  const c = await deployClaim(1000n, 0n)
  await c.call('challenge', [null, pubKeyHex], provider, signer, { newState: st0({ status: 1n, challengerPkh: opPkh, responseDeadline: 0n, window: 0n }) })
  const MID = 'dd'.repeat(32)
  await c.call('reveal', [null, pubKeyHex, MID], provider, signer, { newState: st0({ status: 2n, challengerPkh: opPkh, responseDeadline: 0n, window: 0n, midCommit: MID }) })
  const r = await callTerminal(c, 'slashTimeout', [null, pubKeyHex, 4990n], [{ scriptHex: p2pkh(opPkh), satoshis: 4990 }], 1n)
  check(!!r.txid, `slashTimeout (${r.txid?.slice(0, 12)}…)`)
} catch (e) { check(false, `slashTimeout: ${e.message.slice(0, 90)}`) }

await mineOne()
console.log(`\n${failures === 0 ? 'ALL PASS' : failures + ' FAILURES'} — claim lifecycle on regtest`)
process.exit(failures === 0 ? 0 : 1)
