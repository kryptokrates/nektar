// Optimistic-claim operations — the shared business logic for
// NektarZkClaim/NektarAnchor/ClaimVerdict/DisclosureSale covenant txs.
// Scripts and the MCP server call these; nothing here knows about e2e
// tests or MCP — deps are a runar-sdk-compatible provider + signer.
import { Transaction, LockingScript, UnlockingScript } from '@bsv/sdk'
import { RunarContract, serializeState, deserializeState, buildP2PKHScript } from 'runar-sdk'

export interface Utxo {
  txid: string
  outputIndex: number
  satoshis: number
  script: string
}

export const p2pkh = (pkh: string): string => buildP2PKHScript(pkh)

export const outpointOf = (u: { txid: string; outputIndex: number }): string =>
  Buffer.concat([Buffer.from(u.txid, 'hex').reverse(), Buffer.from([u.outputIndex, 0, 0, 0])]).toString('hex')

/** deserialize a deployed claim's state tail (codePart ‖ '6a' ‖ fields) */
export function readClaimState(art: any, scriptHex: string): any {
  return deserializeState(art.stateFields, scriptHex.slice(art.script.length + 2))
}

/** deploy a NektarZkClaim with the given ctor args; returns the contract
 *  (currentUtxo set) — caller registers claimOp() in the registry. */
export async function deployClaim(art: any, ctorArgs: any[], provider: any, signer: any, satoshis = 5000): Promise<{ contract: any; utxo: Utxo }> {
  const c = new RunarContract(art, ctorArgs)
  c.connect(provider, signer)
  await c.deploy({ satoshis })
  return { contract: c, utxo: (c as any).currentUtxo }
}

/** build the verdict locking script a settle emits */
export function verdictScript(verdictCode: string, vArt: any, fields: { receiptHash: string; journalRoot: string; queryId: string; payeePkh: string }): string {
  return verdictCode + '6a' + serializeState(vArt.stateFields, fields)
}

/** 2-input anchored settle: [claim, anchor] → [anchorCont, verdict, payout,
 *  change(0)]. claim.settleAnchored + anchor.witnessSettle both enumerate
 *  the identical output list → journalRoot == anchor.R consensus-enforced,
 *  and the anchor's continuation keeps it alive for future appends. */
export async function buildAnchoredSettleTx(opts: {
  claimArt: any
  claim: any
  claimUtxo: Utxo
  anchorArt: any
  anchor: any
  anchorUtxo: Utxo
  vArt: any
  verdictCode: string
  receiptHash: string
  queryId: string
  journalRoot: string
  payoutSats: number
  verdictSats: number
  anchorSats: number
  payeePkh: string
  signer: any
  pubKey: string
}): Promise<Transaction> {
  const { claimArt: cArt, claim: c, claimUtxo: cUtxo, anchorArt: aArt, anchor: a, anchorUtxo: aUtxo, signer, pubKey, payeePkh } = opts
  const prevouts = outpointOf(cUtxo) + outpointOf(aUtxo)
  const verdict = verdictScript(opts.verdictCode, opts.vArt, { receiptHash: opts.receiptHash, journalRoot: opts.journalRoot, queryId: opts.queryId, payeePkh })
  const tx = new Transaction()
  tx.addInput({ sourceTXID: cUtxo.txid, sourceOutputIndex: cUtxo.outputIndex, unlockingScript: UnlockingScript.fromHex('00'), sequence: 0xffffffff })
  tx.addInput({ sourceTXID: aUtxo.txid, sourceOutputIndex: aUtxo.outputIndex, unlockingScript: UnlockingScript.fromHex('00'), sequence: 0xffffffff })
  tx.addOutput({ satoshis: opts.anchorSats, lockingScript: LockingScript.fromHex(aUtxo.script) })
  tx.addOutput({ satoshis: opts.verdictSats, lockingScript: LockingScript.fromHex(verdict) })
  tx.addOutput({ satoshis: opts.payoutSats, lockingScript: LockingScript.fromHex(p2pkh(payeePkh)) })
  tx.addOutput({ satoshis: 0, lockingScript: LockingScript.fromHex(p2pkh(payeePkh)) })
  const mIdx = cArt.abi.methods.filter((m: any) => m.isPublic).findIndex((m: any) => m.name === 'settleAnchored')
  const { sigHex: opSig, preimageHex: pre } = c.computeOpPushTxWithCodeSep(tx, 0, cUtxo.script, cUtxo.satoshis, mIdx)
  const sigSub = c.getSubscriptForSigning(cUtxo.script, mIdx)
  const userSig = await signer.sign(tx.toHex(), 0, sigSub, cUtxo.satoshis)
  // runar arg encoding: explicit params, implicit change pair (_changePKH,
  // _changeAmount), txPreimage — buildUnlockingScript appends the selector
  tx.inputs[0].unlockingScript = UnlockingScript.fromHex(
    c.buildStatefulPrefix(opSig, true) +
      c.buildUnlockingScript('settleAnchored', [
        userSig,
        pubKey,
        prevouts,
        BigInt(opts.anchorSats),
        aUtxo.script,
        BigInt(opts.payoutSats),
        BigInt(opts.verdictSats),
        payeePkh,
        0n,
        pre
      ])
  )
  const aIdx = aArt.abi.methods.filter((m: any) => m.isPublic).findIndex((m: any) => m.name === 'witnessSettle')
  const { sigHex: aSig, preimageHex: aPre } = a.computeOpPushTxWithCodeSep(tx, 1, aUtxo.script, aUtxo.satoshis, aIdx)
  const aSub = a.getSubscriptForSigning(aUtxo.script, aIdx)
  const aUserSig = await signer.sign(tx.toHex(), 1, aSub, aUtxo.satoshis)
  tx.inputs[1].unlockingScript = UnlockingScript.fromHex(
    a.buildStatefulPrefix(aSig, true) +
      a.buildUnlockingScript('witnessSettle', [
        aUserSig,
        pubKey,
        opts.receiptHash,
        opts.queryId,
        payeePkh,
        opts.verdictCode,
        BigInt(opts.verdictSats),
        BigInt(opts.payoutSats),
        payeePkh,
        payeePkh,
        0n,
        aPre
      ])
  )
  return tx
}

/** Gated disclosure purchase: [sale, verdict] → [sellerPayout, buyerChange].
 *  sale.buyGated pins the exact pair via hash256(prevouts); the verdict's
 *  authorize enforces payment to its payeePkh — atomic pay-iff-settled.
 *  Both legs are pure covenants (opSig + preimage — no user signature). */
export async function buildBuyGatedTx(opts: {
  saleArt: any
  sale: any
  saleUtxo: Utxo
  verdictArt: any
  verdict: any
  verdictUtxo: Utxo
  wrapped: string
  saleSats: number
  changeSats: number
  changePkh: string
}): Promise<Transaction> {
  const { saleArt: sArt, sale, saleUtxo: sUtxo, verdictArt: vArt, verdict: vc, verdictUtxo: vUtxo } = opts
  const prevouts = outpointOf(sUtxo) + outpointOf(vUtxo)
  const tx = new Transaction()
  tx.addInput({ sourceTXID: sUtxo.txid, sourceOutputIndex: sUtxo.outputIndex, unlockingScript: UnlockingScript.fromHex('00'), sequence: 0xffffffff })
  tx.addInput({ sourceTXID: vUtxo.txid, sourceOutputIndex: vUtxo.outputIndex, unlockingScript: UnlockingScript.fromHex('00'), sequence: 0xffffffff })
  tx.addOutput({ satoshis: opts.saleSats, lockingScript: LockingScript.fromHex(p2pkh(opts.changePkh)) })
  tx.addOutput({ satoshis: opts.changeSats, lockingScript: LockingScript.fromHex(p2pkh(opts.changePkh)) })
  const sIdx = sArt.abi.methods.filter((m: any) => m.isPublic).findIndex((m: any) => m.name === 'buyGated')
  const { sigHex: sSig, preimageHex: sPre } = sale.computeOpPushTxWithCodeSep(tx, 0, sUtxo.script, sUtxo.satoshis, sIdx)
  // opSig is the implicit first push; the rest are ABI-encoded args
  const saleUnlock = sale.buildUnlockingScript('buyGated', [sSig, opts.wrapped, BigInt(opts.changeSats), prevouts, sPre])
  if (saleUnlock.length % 2)
    throw new Error(
      `sale unlock odd hex (${saleUnlock.length}): sSig=${sSig.length} sPre=${sPre.length} prevouts=${prevouts.length} wrapped=${opts.wrapped.length}`
    )
  tx.inputs[0].unlockingScript = UnlockingScript.fromHex(saleUnlock)
  const vIdx = vArt.abi.methods.filter((m: any) => m.isPublic).findIndex((m: any) => m.name === 'authorize')
  const { sigHex: vSig, preimageHex: vPre } = vc.computeOpPushTxWithCodeSep(tx, 1, vUtxo.script, vUtxo.satoshis, vIdx)
  // single-method contract → buildUnlockingScript adds no selector push
  const vUnlock =
    vc.buildStatefulPrefix(vSig, true) + vc.buildUnlockingScript('authorize', [BigInt(opts.saleSats), opts.changePkh, BigInt(opts.changeSats), vPre])
  if (vUnlock.length % 2)
    throw new Error(`verdict unlock odd hex (${vUnlock.length}): vSig=${vSig.length} vPre=${vPre.length} changePkh=${opts.changePkh.length}`)
  tx.inputs[1].unlockingScript = UnlockingScript.fromHex(vUnlock)
  return tx
}

/** terminal-method call recipe: the SDK only pushes codePart + change args
 *  for continuation methods; terminal covenant methods still need them.
 *  Patches the prepared call, signs each sigIndex, finalizes. */
export async function callTerminal(
  c: any,
  method: string,
  args: any[],
  outputs: Array<{ scriptHex: string; satoshis: number }>,
  changePkh: string,
  signer: any,
  changeAmt = 1n
): Promise<any> {
  const pubMethods = c.artifact.abi.methods.filter((m: any) => m.isPublic)
  const mIdx = pubMethods.findIndex((m: any) => m.name === method)
  const prepared = await c.prepareCall(method, args, { terminalOutputs: [...outputs, { scriptHex: p2pkh(changePkh), satoshis: Number(changeAmt) }] })
  prepared._methodNeedsChange = true
  prepared._changePKHHex = changePkh
  prepared._changeAmount = changeAmt
  const sigSub = c.getSubscriptForSigning(prepared._contractUtxo.script, mIdx)
  const sigs: any = {}
  const txHex = prepared.tx.toHex()
  for (const idx of prepared.sigIndices) sigs[idx] = await signer.sign(txHex, 0, sigSub, prepared._contractUtxo.satoshis)
  return c.finalizeCall(prepared, sigs)
}

// ── file-backed claim registry + proof store (shared by watcher + MCP) ──
import { readFileSync, writeFileSync, mkdirSync } from 'node:fs'
import { dirname, join } from 'node:path'

export interface ClaimEntry {
  txid: string
  vout: number
  spent?: boolean
}
export const loadClaims = (file: string): ClaimEntry[] => {
  try {
    return JSON.parse(readFileSync(file, 'utf8'))
  } catch {
    return []
  }
}
export function registerClaim(file: string, e: ClaimEntry): void {
  const list = loadClaims(file)
  if (!list.some((x) => x.txid === e.txid && x.vout === e.vout)) {
    list.push(e)
    mkdirSync(dirname(file), { recursive: true })
    writeFileSync(file, JSON.stringify(list, null, 1))
  }
}
// The outpoint is consumed (by our challenge or any spend). The claim's
// continuation UTXO gets a new txid the registry cannot follow without a
// tx-index — marking spent just stops retrying the dead outpoint.
export function markClaimSpent(file: string, txid: string, vout: number): void {
  const list = loadClaims(file)
  const e = list.find((x) => x.txid === txid && x.vout === vout)
  if (e && !e.spent) {
    e.spent = true
    writeFileSync(file, JSON.stringify(list, null, 1))
  }
}
export const proofFile = (dir: string, receiptHash: string): string => join(dir, `${receiptHash}.bin`)
export function storeProof(dir: string, receiptHash: string, bytes: Uint8Array): string {
  mkdirSync(dir, { recursive: true })
  const f = proofFile(dir, receiptHash)
  writeFileSync(f, bytes)
  return f
}
