// Optimistic-claim operations — the shared business logic for
// MamemZkClaim/MamemAnchor/ClaimVerdict/DisclosureSale covenant txs.
// Scripts and the MCP server call these; nothing here knows about e2e
// tests or MCP — deps are a runar-sdk-compatible provider + signer.
import { Transaction, LockingScript, UnlockingScript } from '@bsv/sdk'
import { RunarContract, serializeState, deserializeState } from 'runar-sdk'

export interface Utxo { txid: string, outputIndex: number, satoshis: number, script: string }

const encPush = (h: string): string => {
  const l = h.length / 2
  if (l === 0) return '00'; if (l <= 75) return l.toString(16).padStart(2, '0') + h
  if (l <= 0xff) return '4c' + l.toString(16).padStart(2, '0') + h
  return '4d' + (l & 0xff).toString(16).padStart(2, '0') + ((l >> 8) & 0xff).toString(16).padStart(2, '0') + h
}
const encNum = (n: bigint): string => {
  if (n === 0n) return '00'
  if (n >= 1n && n <= 16n) return (0x50 + Number(n)).toString(16)
  const b: number[] = []; let v = n
  while (v > 0n) { b.push(Number(v & 0xffn)); v >>= 8n }
  if (b[b.length - 1] & 0x80) b.push(0)
  return encPush(b.map(x => x.toString(16).padStart(2, '0')).join(''))
}
const encArg = (v: bigint | string): string => typeof v === 'bigint' ? encNum(v) : encPush(v)
export const p2pkh = (pkh: string): string => '76a914' + pkh + '88ac'

export const outpointOf = (u: { txid: string, outputIndex: number }): string =>
  Buffer.concat([Buffer.from(u.txid, 'hex').reverse(), Buffer.from([u.outputIndex, 0, 0, 0])]).toString('hex')

/** deserialize a deployed claim's state tail (codePart ‖ '6a' ‖ fields) */
export function readClaimState (art: any, scriptHex: string): any {
  return deserializeState(art.stateFields, scriptHex.slice(art.script.length + 2))
}

/** deploy a MamemZkClaim with the given ctor args; returns the contract
 *  (currentUtxo set) — caller registers claimOp() in the registry. */
export async function deployClaim (art: any, ctorArgs: any[], provider: any, signer: any, satoshis = 5000): Promise<{ contract: any, utxo: Utxo }> {
  const c = new RunarContract(art, ctorArgs)
  c.connect(provider, signer)
  await c.deploy({ satoshis })
  return { contract: c, utxo: (c as any).currentUtxo }
}

/** build the verdict locking script a settle emits */
export function verdictScript (verdictCode: string, vArt: any, fields: { receiptHash: string, journalRoot: string, queryId: string, payeePkh: string }): string {
  return verdictCode + '6a' + serializeState(vArt.stateFields, fields)
}

/** 2-input anchored settle: [claim, anchor] → [anchorCont, verdict, payout,
 *  change(0)]. claim.settleAnchored + anchor.witnessSettle both enumerate
 *  the identical output list → journalRoot == anchor.R consensus-enforced,
 *  and the anchor's continuation keeps it alive for future appends. */
export async function buildAnchoredSettleTx (opts: {
  claimArt: any, claim: any, claimUtxo: Utxo,
  anchorArt: any, anchor: any, anchorUtxo: Utxo,
  vArt: any, verdictCode: string,
  receiptHash: string, queryId: string, journalRoot: string,
  payoutSats: number, verdictSats: number, anchorSats: number,
  payeePkh: string, signer: any, pubKey: string
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
  tx.inputs[0].unlockingScript = UnlockingScript.fromHex(
    c.buildStatefulPrefix(opSig, true) + encArg(userSig) + encArg(pubKey) + encArg(prevouts) + encArg(BigInt(opts.anchorSats)) + encPush(aUtxo.script) + encArg(BigInt(opts.payoutSats)) + encArg(BigInt(opts.verdictSats)) +
    encPush(payeePkh) + encNum(0n) + encPush(pre) + encNum(BigInt(mIdx)))
  const aIdx = aArt.abi.methods.filter((m: any) => m.isPublic).findIndex((m: any) => m.name === 'witnessSettle')
  const { sigHex: aSig, preimageHex: aPre } = a.computeOpPushTxWithCodeSep(tx, 1, aUtxo.script, aUtxo.satoshis, aIdx)
  const aSub = a.getSubscriptForSigning(aUtxo.script, aIdx)
  const aUserSig = await signer.sign(tx.toHex(), 1, aSub, aUtxo.satoshis)
  tx.inputs[1].unlockingScript = UnlockingScript.fromHex(
    a.buildStatefulPrefix(aSig, true) + encArg(aUserSig) + encArg(pubKey) + encArg(opts.receiptHash) + encArg(opts.queryId) + encArg(payeePkh) + encArg(opts.verdictCode) + encArg(BigInt(opts.verdictSats)) + encArg(BigInt(opts.payoutSats)) + encArg(payeePkh) +
    encPush(payeePkh) + encNum(0n) + encPush(aPre) + encNum(BigInt(aIdx)))
  return tx
}

/** Gated disclosure purchase: [sale, verdict] → [sellerPayout, buyerChange].
 *  sale.buyGated pins the exact pair via hash256(prevouts); the verdict's
 *  authorize enforces payment to its payeePkh — atomic pay-iff-settled.
 *  Both legs are pure covenants (opSig + preimage — no user signature). */
export async function buildBuyGatedTx (opts: {
  saleArt: any, sale: any, saleUtxo: Utxo,
  verdictArt: any, verdict: any, verdictUtxo: Utxo,
  wrapped: string, saleSats: number, changeSats: number, changePkh: string
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
  const saleUnlock = encPush(sSig) + sale.buildUnlockingScript('buyGated', [opts.wrapped, BigInt(opts.changeSats), prevouts, sPre])
  if (saleUnlock.length % 2) throw new Error(`sale unlock odd hex (${saleUnlock.length}): sSig=${sSig.length} sPre=${sPre.length} prevouts=${prevouts.length} wrapped=${opts.wrapped.length}`)
  tx.inputs[0].unlockingScript = UnlockingScript.fromHex(saleUnlock)
  const vIdx = vArt.abi.methods.filter((m: any) => m.isPublic).findIndex((m: any) => m.name === 'authorize')
  const { sigHex: vSig, preimageHex: vPre } = vc.computeOpPushTxWithCodeSep(tx, 1, vUtxo.script, vUtxo.satoshis, vIdx)
  // single-method contract → no selector push
  const vUnlock = vc.buildStatefulPrefix(vSig, true) + encArg(BigInt(opts.saleSats)) + encPush(opts.changePkh) + encNum(BigInt(opts.changeSats)) + encPush(vPre)
  if (vUnlock.length % 2) throw new Error(`verdict unlock odd hex (${vUnlock.length}): vSig=${vSig.length} vPre=${vPre.length} changePkh=${opts.changePkh.length}`)
  tx.inputs[1].unlockingScript = UnlockingScript.fromHex(vUnlock)
  return tx
}

/** terminal-method call recipe: the SDK only pushes codePart + change args
 *  for continuation methods; terminal covenant methods still need them.
 *  Patches the prepared call, signs each sigIndex, finalizes. */
export async function callTerminal (c: any, method: string, args: any[], outputs: Array<{ scriptHex: string, satoshis: number }>, changePkh: string, signer: any, changeAmt = 1n): Promise<any> {
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
import { existsSync, readFileSync, writeFileSync, mkdirSync } from 'node:fs'
import { dirname, join } from 'node:path'

export interface ClaimEntry { txid: string, vout: number }
export const loadClaims = (file: string): ClaimEntry[] => { try { return JSON.parse(readFileSync(file, 'utf8')) } catch { return [] } }
export function registerClaim (file: string, e: ClaimEntry): void {
  const list = loadClaims(file)
  if (!list.some(x => x.txid === e.txid && x.vout === e.vout)) { list.push(e); mkdirSync(dirname(file), { recursive: true }); writeFileSync(file, JSON.stringify(list, null, 1)) }
}
export const proofFile = (dir: string, receiptHash: string): string => join(dir, `${receiptHash}.bin`)
export function storeProof (dir: string, receiptHash: string, bytes: Uint8Array): string {
  mkdirSync(dir, { recursive: true }); const f = proofFile(dir, receiptHash); writeFileSync(f, bytes); return f
}
