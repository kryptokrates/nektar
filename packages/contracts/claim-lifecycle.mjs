import { RunarContract, MockProvider, LocalSigner } from 'runar-sdk'
import { readFileSync } from 'node:fs'
const { Spend, Transaction, Hash, Transaction: BsvTransaction, LockingScript, UnlockingScript } = await import('@bsv/sdk')
const art = JSON.parse(readFileSync('NektarZkClaim.json', 'utf8'))
const vArt = JSON.parse(readFileSync('ClaimVerdict.json', 'utf8'))
const VCR = vArt.script // ClaimVerdict codePart, baked into claim state
const prover = new LocalSigner('11'.repeat(32))
const challenger = new LocalSigner('22'.repeat(32))
const pk = await prover.getPublicKey()
const cpk = await challenger.getPublicKey()
const pkh = (x) => Buffer.from(Hash.hash160(Buffer.from(x, 'hex'))).toString('hex')
const p2pkhScript = (p) => '76a914' + p + '88ac'
const Z = '00'.repeat(32)
const RH = 'aa'.repeat(32),
  JR = 'bb'.repeat(32),
  QID = 'cc'.repeat(32)
let PASS = 0,
  FAILS = []

// --- local arg encoders (SDK internals aren't exported) ---
const encPush = (h) => {
  const l = h.length / 2
  if (l === 0) return '00'
  if (l <= 75) return l.toString(16).padStart(2, '0') + h
  if (l <= 0xff) return '4c' + l.toString(16).padStart(2, '0') + h
  return '4d' + (l & 0xff).toString(16).padStart(2, '0') + ((l >> 8) & 0xff).toString(16).padStart(2, '0') + h
}
const encNum = (n) => {
  if (n === 0n) return '00'
  if (n >= 1n && n <= 16n) return (0x50 + Number(n)).toString(16)
  if (n === -1n) return '4f'
  const neg = n < 0n
  let a = neg ? -n : n
  const b = []
  while (a > 0n) {
    b.push(Number(a & 0xffn))
    a >>= 8n
  }
  if (b[b.length - 1] & 0x80) b.push(neg ? 0x80 : 0)
  else if (neg) b[b.length - 1] |= 0x80
  return encPush(b.map((x) => x.toString(16).padStart(2, '0')).join(''))
}
const encArg = (v) => (typeof v === 'bigint' ? encNum(v) : encPush(v))

const ourPrevouts = (c) => {
  const u = c.currentUtxo
  return Buffer.concat([Buffer.from(u.txid, 'hex').reverse(), Buffer.from([0, 0, 0, 0])]).toString('hex')
}
const evalSpend = (prov, tag, idx = 0, srcIdx = 1) => {
  const hx = prov.getBroadcastedTxs()
  const st = Transaction.fromHex(hx.at(-1))
  const dp = Transaction.fromHex(hx.at(-1 - srcIdx))
  const srcOut = dp.outputs[st.inputs[idx].sourceOutputIndex]
  const sp = new Spend({
    sourceTXID: st.inputs[idx].sourceTXID,
    sourceOutputIndex: st.inputs[idx].sourceOutputIndex,
    sourceSatoshis: srcOut.satoshis ?? 5000,
    lockingScript: srcOut.lockingScript,
    transactionVersion: st.version,
    otherInputs: st.inputs.filter((_, i) => i !== idx),
    outputs: st.outputs,
    inputIndex: idx,
    unlockingScript: st.inputs[idx].unlockingScript,
    inputSequence: st.inputs[idx].sequence ?? 0xffffffff,
    lockTime: st.lockTime ?? 0,
    isRelaxed: true
  })
  try {
    sp.validate()
    console.log(tag, 'VALID ops:', sp.executedOpCount)
    PASS++
  } catch (e) {
    console.log(tag, 'FAIL:', e.message.slice(0, 80))
    FAILS.push(tag)
  }
}
const newProv = () => {
  const p = new MockProvider('testnet')
  p.addUtxo('1Q1pE5vPGEEMqRcVRMbtBK842Y6Pzo6nK9', { txid: 'ab'.repeat(32), outputIndex: 0, satoshis: 1e6, script: '' })
  return p
}
const deploy = async (prov, opts = {}) => {
  const { deadline = 1000n, window = 100n, stages = 4n } = opts
  const ctor = [0n, RH, JR, 10n, QID, 5000n, deadline, 0n, Z, 0n, stages, Z, pkh(pk), 1000n, window, stages, VCR, '']
  const c = new RunarContract(art, ctor)
  c.connect(prov, prover)
  await c.deploy({ satoshis: 5000 })
  return c
}
const st0 = (over = {}) => ({
  status: 0n,
  receiptHash: RH,
  journalRoot: JR,
  journalSize: 10n,
  queryId: QID,
  bondSats: 5000n,
  deadline: 1000n,
  responseDeadline: 0n,
  challengerPkh: Z,
  bisectLo: 0n,
  bisectHi: 4n,
  midCommit: Z,
  proverPkh: pkh(pk),
  maxFee: 1000n,
  window: 100n,
  stages: 4n,
  verdictCode: VCR,
  anchorOutpoint: '',
  ...over
})
const pkh_addr = await prover.getAddress()
const cpk_addr = await challenger.getAddress()
const fund = (prov, who, amt = 6000) =>
  prov.addUtxo(who === prover ? pkh_addr : cpk_addr, {
    txid: Math.random().toString(16).slice(2).padEnd(64, '0').slice(0, 64),
    outputIndex: 0,
    satoshis: amt,
    script: ''
  })

// Terminal methods: emitted script expects [codePart][opSig][args][changePKH][changeAmount][preimage][selector]
// and a trailing 0-sat change output — patch the prepared call flags.
const callTerminal = async (c, method, args, prov, signer, outputs, changePkh) => {
  const op = c._provider,
    os = c._signer
  c._provider = prov
  c._signer = signer
  try {
    const pubMethods = c.artifact.abi.methods.filter((m) => m.isPublic)
    const mIdx = pubMethods.findIndex((m) => m.name === method)
    const prepared = await c.prepareCall(method, args, { terminalOutputs: [...outputs, { scriptHex: p2pkhScript(changePkh), satoshis: 0 }] })
    prepared._methodNeedsChange = true
    prepared._changePKHHex = changePkh
    prepared._changeAmount = 0n
    const sigSubscript = c.getSubscriptForSigning(prepared._contractUtxo.script, mIdx)
    const signatures = {}
    const txHex = prepared.tx.toHex()
    for (const idx of prepared.sigIndices) signatures[idx] = await signer.sign(txHex, 0, sigSubscript, prepared._contractUtxo.satoshis)
    return await c.finalizeCall(prepared, signatures)
  } finally {
    c._provider = op
    c._signer = os
  }
}

// ── 1. settle (happy path): deadline=0 so locktime 0 passes ──
{
  const prov = newProv()
  const ctor = [0n, RH, JR, 10n, QID, 5000n, 0n, 0n, Z, 0n, 4n, Z, pkh(pk), 1000n, 100n, 4n, VCR, '']
  const c = new RunarContract(art, ctor)
  c.connect(prov, prover)
  await c.deploy({ satoshis: 5000 })
  // ClaimVerdict UTXO: codePart + 6a + len-prefixed {receiptHash, journalRoot, queryId, payeePkh}
  const { serializeState } = await import('runar-sdk')
  const vstate = serializeState(vArt.stateFields, { receiptHash: RH, journalRoot: JR, queryId: QID, payeePkh: pkh(pk) })
  const verdict = VCR + '6a' + vstate
  await callTerminal(
    c,
    'settle',
    [null, pk, ourPrevouts(c), 4980n, 10n],
    prov,
    prover,
    [
      { scriptHex: verdict, satoshis: 10 },
      { scriptHex: p2pkhScript(pkh(pk)), satoshis: 4980 }
    ],
    pkh(pk)
  )
  evalSpend(prov, 'settle')
  // the emitted verdict UTXO is a real ClaimVerdict — its authorize must also spend
  {
    const vtx = Transaction.fromHex(prov.getBroadcastedTxs().at(-1))
    const vc = new RunarContract(vArt, [RH, JR, QID, pkh(pk)])
    vc.currentUtxo = { txid: vtx.id('hex'), outputIndex: 0, satoshis: 10, script: verdict }
    await callTerminal(vc, 'authorize', [9n], prov, prover, [{ scriptHex: p2pkhScript(pkh(pk)), satoshis: 9 }], pkh(pk))
    evalSpend(prov, 'verdict-authorize')
  }
}
// ── 2. challenge + reveal + bisect + proveStage ──
{
  const prov = newProv()
  const c = await deploy(prov)
  fund(prov, challenger)
  await c.call('challenge', [null, cpk], prov, challenger, { newState: st0({ status: 1n, challengerPkh: pkh(cpk), responseDeadline: 100n }) })
  evalSpend(prov, 'challenge')
  fund(prov, prover)
  const MID = 'dd'.repeat(32)
  await c.call('reveal', [null, pk, MID], prov, prover, { newState: st0({ status: 2n, challengerPkh: pkh(cpk), responseDeadline: 100n, midCommit: MID }) })
  evalSpend(prov, 'reveal', 0, 1)
  fund(prov, challenger)
  await c.call('bisect', [null, cpk, 1n], prov, challenger, {
    newState: st0({ status: 1n, challengerPkh: pkh(cpk), responseDeadline: 100n, bisectLo: 2n, bisectHi: 4n })
  })
  evalSpend(prov, 'bisect1', 0, 1)
  fund(prov, prover)
  const MID2 = 'ee'.repeat(32)
  await c.call('reveal', [null, pk, MID2], prov, prover, {
    newState: st0({ status: 2n, challengerPkh: pkh(cpk), responseDeadline: 100n, bisectLo: 2n, bisectHi: 4n, midCommit: MID2 })
  })
  evalSpend(prov, 'reveal2', 0, 1)
  fund(prov, challenger)
  await c.call('bisect', [null, cpk, 1n], prov, challenger, {
    newState: st0({ status: 2n, challengerPkh: pkh(cpk), responseDeadline: 100n, bisectLo: 3n, bisectHi: 4n })
  })
  evalSpend(prov, 'bisect2', 0, 1)
  // proveStage: real Poseidon2Stage UTXO co-spend — [claim UTXO, stage UTXO].
  // Outputs = [stageCont(1sat), payout(4990), shared change(0)] — the stage's
  // addOutput emits its continuation @out0; its addRawOutput(post) re-emits
  // our payout; both contracts enumerate the same full output list.
  {
    const { computeNewState, serializeState } = await import('runar-sdk')
    const stageArt = JSON.parse(readFileSync('Poseidon2Stage.json', 'utf8'))
    const stIn = Object.fromEntries([...Array(24)].map((_, i) => [`s${i}`, BigInt(i)]))
    stIn.round = 0n
    const sc = new RunarContract(stageArt, [...[...Array(24)].map((_, i) => BigInt(i)), 0n])
    sc.connect(prov, prover)
    await sc.deploy({ satoshis: 1 })
    const stageUtxo = sc.currentUtxo
    const postScript = p2pkhScript(pkh(pk))
    const stOut = computeNewState(stageArt.anf, 'fullStep', stIn, { postSats: 4990n, postScript })
    const stageContScript = stageArt.script + '6a' + serializeState(stageArt.stateFields, stOut)

    const claimUtxo = c.currentUtxo
    const ourOp = Buffer.concat([Buffer.from(claimUtxo.txid, 'hex').reverse(), Buffer.from([0, 0, 0, 0])]).toString('hex')
    const stageOp = Buffer.concat([Buffer.from(stageUtxo.txid, 'hex').reverse(), Buffer.from([0, 0, 0, 0])]).toString('hex')
    const prevouts = ourOp + stageOp
    const tx = new BsvTransaction()
    tx.addInput({ sourceTXID: claimUtxo.txid, sourceOutputIndex: claimUtxo.outputIndex, unlockingScript: UnlockingScript.fromHex('00'), sequence: 0xffffffff })
    tx.addInput({ sourceTXID: stageUtxo.txid, sourceOutputIndex: stageUtxo.outputIndex, unlockingScript: UnlockingScript.fromHex('00'), sequence: 0xffffffff })
    tx.addOutput({ satoshis: 1, lockingScript: LockingScript.fromHex(stageContScript) })
    tx.addOutput({ satoshis: 4990, lockingScript: LockingScript.fromHex(postScript) })
    tx.addOutput({ satoshis: 0, lockingScript: LockingScript.fromHex(p2pkhScript(pkh(pk))) })
    const pubMethods = c.artifact.abi.methods.filter((m) => m.isPublic)
    const mIdx = pubMethods.findIndex((m) => m.name === 'proveStage')
    const { sigHex: opSig, preimageHex: preimage } = c.computeOpPushTxWithCodeSep(tx, 0, claimUtxo.script, claimUtxo.satoshis, mIdx)
    const sigSub = c.getSubscriptForSigning(claimUtxo.script, mIdx)
    const userSig = await prover.sign(tx.toHex(), 0, sigSub, claimUtxo.satoshis)
    const unlock =
      c.buildStatefulPrefix(opSig, true) +
      encArg(userSig) +
      encArg(pk) +
      encArg(prevouts) +
      encArg(stageOp) +
      encArg(4990n) +
      encArg(1n) +
      encArg(stageContScript) +
      encPush(pkh(pk)) +
      encNum(0n) +
      encPush(preimage) +
      encNum(BigInt(mIdx))
    tx.inputs[0].unlockingScript = UnlockingScript.fromHex(unlock)
    // stage leg: continuation unlock [codePart][opSig][postSats][postScript][changePKH][changeAmt][preimage][sel=fullStep]
    const sPub = stageArt.abi.methods.filter((m) => m.isPublic)
    const sIdx = sPub.findIndex((m) => m.name === 'fullStep')
    const { sigHex: opSig2, preimageHex: preimage2 } = sc.computeOpPushTxWithCodeSep(tx, 1, stageUtxo.script, stageUtxo.satoshis, sIdx)
    tx.inputs[1].unlockingScript = UnlockingScript.fromHex(
      sc.buildStatefulPrefix(opSig2, true) + encArg(4990n) + encArg(postScript) + encPush(pkh(pk)) + encNum(0n) + encPush(preimage2) + encNum(BigInt(sIdx))
    )
    await prov.broadcast(tx)
    c.currentUtxo = null
    evalSpend(prov, 'proveStage', 0, 2)
    // also validate the stage's own input — its script judges the round under consensus
    {
      const st = Transaction.fromHex(prov.getBroadcastedTxs().at(-1))
      const sp = new Spend({
        sourceTXID: stageUtxo.txid,
        sourceOutputIndex: stageUtxo.outputIndex,
        sourceSatoshis: stageUtxo.satoshis,
        lockingScript: LockingScript.fromHex(stageUtxo.script),
        transactionVersion: st.version,
        otherInputs: st.inputs.filter((_, i) => i !== 1),
        outputs: st.outputs,
        inputIndex: 1,
        unlockingScript: st.inputs[1].unlockingScript,
        inputSequence: st.inputs[1].sequence ?? 0xffffffff,
        lockTime: st.lockTime ?? 0,
        isRelaxed: true
      })
      try {
        sp.validate()
        console.log('stage-input VALID ops:', sp.executedOpCount)
        PASS++
      } catch (e) {
        console.log('stage-input FAIL:', e.message.slice(0, 80))
        FAILS.push('stage-input')
      }
    }
  }
}
// ── 3. claimTimeout (challenger silent; window=0 → responseDeadline=0) ──
{
  const prov = newProv()
  const c = await deploy(prov, { window: 0n })
  fund(prov, challenger)
  await c.call('challenge', [null, cpk], prov, challenger, { newState: st0({ status: 1n, challengerPkh: pkh(cpk), responseDeadline: 0n, window: 0n }) })
  evalSpend(prov, 'challenge(w0)', 0, 1)
  await callTerminal(c, 'claimTimeout', [null, pk, 4990n], prov, prover, [{ scriptHex: p2pkhScript(pkh(pk)), satoshis: 4990 }], pkh(pk))
  evalSpend(prov, 'claimTimeout', 0, 1)
}
// ── 4. slashTimeout (prover silent after reveal; window=0) ──
{
  const prov = newProv()
  const c = await deploy(prov, { window: 0n })
  fund(prov, challenger)
  await c.call('challenge', [null, cpk], prov, challenger, { newState: st0({ status: 1n, challengerPkh: pkh(cpk), responseDeadline: 0n, window: 0n }) })
  fund(prov, prover)
  const MID = 'dd'.repeat(32)
  await c.call('reveal', [null, pk, MID], prov, prover, {
    newState: st0({ status: 2n, challengerPkh: pkh(cpk), responseDeadline: 0n, midCommit: MID, window: 0n })
  })
  await callTerminal(c, 'slashTimeout', [null, cpk, 4990n], prov, challenger, [{ scriptHex: p2pkhScript(pkh(cpk)), satoshis: 4990 }], pkh(cpk))
  evalSpend(prov, 'slashTimeout', 0, 1)
}
// ── 5. consumer hook: DisclosureSale.buyGated on a settled verdict ──
// settle emits the ClaimVerdict UTXO → depositor deploys a sale gated on
// its outpoint → buyGated requires [sale, verdict] inputs; the verdict's
// authorize enforces the payee payout. Both scripts execute.
{
  const prov = newProv()
  const { serializeState } = await import('runar-sdk')
  const ctor = [0n, RH, JR, 10n, QID, 5000n, 0n, 0n, Z, 0n, 4n, Z, pkh(pk), 1000n, 100n, 4n, VCR, '']
  const c = new RunarContract(art, ctor)
  c.connect(prov, prover)
  await c.deploy({ satoshis: 5000 })
  const vstate = serializeState(vArt.stateFields, { receiptHash: RH, journalRoot: JR, queryId: QID, payeePkh: pkh(pk) })
  const verdict = VCR + '6a' + vstate
  await callTerminal(
    c,
    'settle',
    [null, pk, ourPrevouts(c), 4980n, 10n],
    prov,
    prover,
    [
      { scriptHex: verdict, satoshis: 10 },
      { scriptHex: p2pkhScript(pkh(pk)), satoshis: 4980 }
    ],
    pkh(pk)
  )
  const vtx = Transaction.fromHex(prov.getBroadcastedTxs().at(-1))
  const vUtxo = { txid: vtx.id('hex'), outputIndex: 0, satoshis: 10, script: verdict }
  const vOp = Buffer.concat([Buffer.from(vUtxo.txid, 'hex').reverse(), Buffer.from([0, 0, 0, 0])]).toString('hex')

  // deploy the gated sale (depositor=buyer; seller=prover whose claim settled)
  const sArt = JSON.parse(readFileSync('DisclosureSale.json', 'utf8'))
  const WRAPPED = 'f1'.repeat(64)
  const hw = Buffer.from(Hash.sha256(Buffer.from(WRAPPED, 'hex'))).toString('hex')
  const saleCtor = [pkh(pk), 3000n, hw, 0n, 1000n, pkh(cpk), vOp]
  const sale = new RunarContract(sArt, saleCtor)
  fund(prov, challenger)
  sale.connect(prov, challenger)
  await sale.deploy({ satoshis: 4000 })
  const sUtxo = sale.currentUtxo
  const sOp = Buffer.concat([Buffer.from(sUtxo.txid, 'hex').reverse(), Buffer.from([0, 0, 0, 0])]).toString('hex')
  const prevouts = sOp + vOp

  const tx = new BsvTransaction()
  tx.addInput({ sourceTXID: sUtxo.txid, sourceOutputIndex: sUtxo.outputIndex, unlockingScript: UnlockingScript.fromHex('00'), sequence: 0xffffffff })
  tx.addInput({ sourceTXID: vUtxo.txid, sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex('00'), sequence: 0xffffffff })
  tx.addOutput({ satoshis: 3000, lockingScript: LockingScript.fromHex(p2pkhScript(pkh(pk))) }) // pay seller (verdict payee)
  tx.addOutput({ satoshis: 900, lockingScript: LockingScript.fromHex(p2pkhScript(pkh(cpk))) }) // change → depositor
  // sale leg: [wrapped][changeSats][prevouts][preimage][sel=buyGated]
  const sPub = sArt.abi.methods.filter((m) => m.isPublic)
  const sIdx = sPub.findIndex((m) => m.name === 'buyGated')
  const { sigHex: sSig, preimageHex: sPre } = sale.computeOpPushTxWithCodeSep(tx, 0, sUtxo.script, sUtxo.satoshis, sIdx)
  tx.inputs[0].unlockingScript = UnlockingScript.fromHex(encPush(sSig) + sale.buildUnlockingScript('buyGated', [WRAPPED, 900n, prevouts, sPre]))
  // verdict leg: [codePart][opSig][paySats][changePKH][changeAmt][preimage][sel=authorize]
  const vc = new RunarContract(vArt, [RH, JR, QID, pkh(pk)])
  const vPub = vArt.abi.methods.filter((m) => m.isPublic)
  const vIdx = vPub.findIndex((m) => m.name === 'authorize')
  const { sigHex: vSig2, preimageHex: vPre2 } = vc.computeOpPushTxWithCodeSep(tx, 1, vUtxo.script, vUtxo.satoshis, vIdx)
  // single-method contract → no selector push (SDK emits 6 items exactly)
  tx.inputs[1].unlockingScript = UnlockingScript.fromHex(
    vc.buildStatefulPrefix(vSig2, true) + encArg(3000n) + encPush(pkh(cpk)) + encNum(900n) + encPush(vPre2)
  )
  await prov.broadcast(tx)
  evalSpend(prov, 'buyGated', 0, 1)
  // verdict input executes too — enforces payee payout under consensus
  {
    const st = Transaction.fromHex(prov.getBroadcastedTxs().at(-1))
    const sp = new Spend({
      sourceTXID: vUtxo.txid,
      sourceOutputIndex: 0,
      sourceSatoshis: 10,
      lockingScript: LockingScript.fromHex(vUtxo.script),
      transactionVersion: st.version,
      otherInputs: st.inputs.filter((_, i) => i !== 1),
      outputs: st.outputs,
      inputIndex: 1,
      unlockingScript: st.inputs[1].unlockingScript,
      inputSequence: 0xffffffff,
      lockTime: 0,
      isRelaxed: true
    })
    try {
      sp.validate()
      console.log('verdict-in-sale VALID ops:', sp.executedOpCount)
      PASS++
    } catch (e) {
      console.log('verdict-in-sale FAIL:', e.message.slice(0, 80))
      FAILS.push('verdict-in-sale')
    }
  }
}
// ── 6. anchored settle: claim bound to a NektarAnchor UTXO ──
// The anchor's witnessSettle re-emits the verdict with its OWN R — the
// tx only validates if claim.journalRoot == anchor.R (§4.5 binding).
{
  const prov = newProv()
  const { serializeState } = await import('runar-sdk')
  const aArt = JSON.parse(readFileSync('NektarAnchor.json', 'utf8'))
  // anchor state: owner=prover, n=0,size=10,R=JR,Rp=Z — journalRoot must equal R
  const aCtor = [pk, pkh(pk), '4d455441', 0n, 10n, JR, '22'.repeat(32), Buffer.from('RPRIME').toString('hex')]
  const a = new RunarContract(aArt, aCtor)
  a.connect(prov, prover)
  await a.deploy({ satoshis: 1 })
  const aUtxo = a.currentUtxo
  const aOp = Buffer.concat([Buffer.from(aUtxo.txid, 'hex').reverse(), Buffer.from([0, 0, 0, 0])]).toString('hex')
  // claim bound to that anchor
  const ctor = [0n, RH, JR, 10n, QID, 5000n, 0n, 0n, Z, 0n, 4n, Z, pkh(pk), 1000n, 100n, 4n, VCR, aOp]
  const c = new RunarContract(art, ctor)
  c.connect(prov, prover)
  await c.deploy({ satoshis: 5000 })
  const cUtxo = c.currentUtxo
  const cOp = Buffer.concat([Buffer.from(cUtxo.txid, 'hex').reverse(), Buffer.from([0, 0, 0, 0])]).toString('hex')
  const prevouts = cOp + aOp
  const vstate = serializeState(vArt.stateFields, { receiptHash: RH, journalRoot: JR, queryId: QID, payeePkh: pkh(pk) })
  const verdict = VCR + '6a' + vstate
  // manual 2-input settleAnchored: [claim, anchor] → [anchorCont, verdict, payout, change]
  const tx = new BsvTransaction()
  tx.addInput({ sourceTXID: cUtxo.txid, sourceOutputIndex: cUtxo.outputIndex, unlockingScript: UnlockingScript.fromHex('00'), sequence: 0xffffffff })
  tx.addInput({ sourceTXID: aUtxo.txid, sourceOutputIndex: aUtxo.outputIndex, unlockingScript: UnlockingScript.fromHex('00'), sequence: 0xffffffff })
  tx.addOutput({ satoshis: 1, lockingScript: LockingScript.fromHex(aUtxo.script) })
  tx.addOutput({ satoshis: 10, lockingScript: LockingScript.fromHex(verdict) })
  tx.addOutput({ satoshis: 4980, lockingScript: LockingScript.fromHex(p2pkhScript(pkh(pk))) })
  tx.addOutput({ satoshis: 0, lockingScript: LockingScript.fromHex(p2pkhScript(pkh(pk))) })
  const mIdx = art.abi.methods.filter((m) => m.isPublic).findIndex((m) => m.name === 'settleAnchored')
  const { sigHex: opSig, preimageHex: pre } = c.computeOpPushTxWithCodeSep(tx, 0, cUtxo.script, cUtxo.satoshis, mIdx)
  const sigSub = c.getSubscriptForSigning(cUtxo.script, mIdx)
  const userSig = await prover.sign(tx.toHex(), 0, sigSub, cUtxo.satoshis)
  tx.inputs[0].unlockingScript = UnlockingScript.fromHex(
    c.buildStatefulPrefix(opSig, true) +
      encArg(userSig) +
      encArg(pk) +
      encArg(prevouts) +
      encArg(1n) +
      encPush(aUtxo.script) +
      encArg(4980n) +
      encArg(10n) +
      encPush(pkh(pk)) +
      encNum(0n) +
      encPush(pre) +
      encNum(BigInt(mIdx))
  )
  // anchor leg: witnessSettle is now a CONTINUATION method — it re-emits its
  // own state as output0 (anchor survives), then verdict+payout. Unlock gets
  // implicit _changePKH/_changeAmount like other continuation calls.
  const aPub = aArt.abi.methods.filter((m) => m.isPublic)
  const aIdx = aPub.findIndex((m) => m.name === 'witnessSettle')
  const { sigHex: aSig, preimageHex: aPre } = a.computeOpPushTxWithCodeSep(tx, 1, aUtxo.script, aUtxo.satoshis, aIdx)
  const aSub = a.getSubscriptForSigning(aUtxo.script, aIdx)
  const aUserSig = await prover.sign(tx.toHex(), 1, aSub, aUtxo.satoshis)
  tx.inputs[1].unlockingScript = UnlockingScript.fromHex(
    a.buildStatefulPrefix(aSig, true) +
      encArg(aUserSig) +
      encArg(pk) +
      encArg(RH) +
      encArg(QID) +
      encArg(pkh(pk)) +
      encArg(VCR) +
      encArg(10n) +
      encArg(4980n) +
      encArg(pkh(pk)) +
      encPush(pkh(pk)) +
      encNum(0n) +
      encPush(aPre) +
      encNum(BigInt(aIdx))
  )
  await prov.broadcast(tx)
  evalSpend(prov, 'anchored-settle', 0, 1)
  // anchor input executes too
  {
    const st = Transaction.fromHex(prov.getBroadcastedTxs().at(-1))
    const sp = new Spend({
      sourceTXID: aUtxo.txid,
      sourceOutputIndex: 0,
      sourceSatoshis: 1,
      lockingScript: LockingScript.fromHex(aUtxo.script),
      transactionVersion: st.version,
      otherInputs: st.inputs.filter((_, i) => i !== 1),
      outputs: st.outputs,
      inputIndex: 1,
      unlockingScript: st.inputs[1].unlockingScript,
      inputSequence: 0xffffffff,
      lockTime: 0,
      isRelaxed: true
    })
    try {
      sp.validate()
      console.log('anchor-witness VALID ops:', sp.executedOpCount)
      PASS++
    } catch (e) {
      console.log('anchor-witness FAIL:', e.message.slice(0, 80))
      FAILS.push('anchor-witness')
    }
  }
}
console.log(`\n=== ${PASS} passed, ${FAILS.length} failed: ${FAILS.join(',')}`)
