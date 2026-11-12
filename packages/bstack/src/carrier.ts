import { PrivateKey, ProtoWallet, Transaction, type ChainTracker } from '@bsv/sdk'
import {
  mintCarrier,
  sweep as bSweep,
  mineableRefusal,
  unlockingRefusal,
  decodeFunding,
  decodeStrictPushDrop,
  readerLockingKey,
  type Fees
} from '@lightwebinc/bcommon'
import { anchoredTree, DERIVATION, FUNDING_TAG, ORIGINATOR } from './tree.js'

// a carrier's funding parent must be proven — or one unconfirmed level deep
// on anchored ancestry, the same §10.3 rule finger applies to tokens: every
// one of the tree's own inputs carries a verifiable merkle path, so the BEEF
// steps back to a mined anchor and the host's exposure is a single mempool
// hop of a 1-sat slot
const provenParent = async (p: Transaction, tracker: ChainTracker): Promise<boolean> => {
  if (p.merklePath !== undefined) return await p.merklePath.verify(p.id('hex'), tracker)
  for (const i of p.inputs) {
    const gp = i.sourceTransaction
    if (!gp?.merklePath || !(await gp.merklePath.verify(gp.id('hex'), tracker))) return false
  }
  return true
}

export async function buildCarrier(p: {
  key: PrivateKey
  payload: Uint8Array
  slot: { tree: Transaction; vout: number }
}): Promise<{ carrier: Transaction; beef: number[] }> {
  const { key, payload, slot } = p
  if (!anchoredTree(slot.tree)) throw new Error('buildCarrier: funding tree is not proven (no merklePath or anchored ancestry)')
  const carrier = await mintCarrier(new ProtoWallet(key), ORIGINATOR, DERIVATION, payload, slot.tree, slot.vout)
  return { carrier, beef: carrier.toAtomicBEEF() }
}

export async function buildSweep(p: { key: PrivateKey; tree: Transaction; vouts: number[]; fees: Fees }): Promise<Transaction> {
  return await bSweep(new ProtoWallet(p.key), ORIGINATOR, DERIVATION, FUNDING_TAG, p.tree, p.vouts, p.fees)
}

export type CarrierCheck = { ok: true; carrier: Transaction; payload: Uint8Array; lockingKey: string; fundingOutpoint: string } | { ok: false; reason: string }

export async function checkCarrier(beef: number[], o: { tracker: ChainTracker }): Promise<CarrierCheck> {
  try {
    const tx = Transaction.fromAtomicBEEF(beef)
    for (const i of tx.inputs) {
      const p = i.sourceTransaction
      if (p === undefined || !(await provenParent(p, o.tracker))) return { ok: false, reason: 'spv' }
    }
    const r1 = mineableRefusal(tx)
    if (r1 !== undefined) return { ok: false, reason: `mineable:${r1}` }
    const r2 = unlockingRefusal(tx)
    if (r2 !== undefined) return { ok: false, reason: `unlocking:${r2}` }
    if (tx.inputs.length !== 1 || tx.outputs.length !== 1) return { ok: false, reason: 'shape' }
    const parent = tx.inputs[0].sourceTransaction
    if (!parent) return { ok: false, reason: 'no-parent' }
    const out = parent.outputs[tx.inputs[0].sourceOutputIndex]
    const key = decodeFunding(out.lockingScript, Array.from(FUNDING_TAG))
    if (key === undefined) return { ok: false, reason: 'not-funding' }
    return {
      ok: true,
      carrier: tx,
      payload: extractPayload(tx),
      lockingKey: key.toString(),
      fundingOutpoint: `${parent.id('hex')}:${tx.inputs[0].sourceOutputIndex}`
    }
  } catch (e) {
    return { ok: false, reason: `parse:${(e as Error).message.slice(0, 80)}` }
  }
}

export type SweepCheck = { ok: true; spends: string[] } | { ok: false; reason: string }

// a retraction is a MINED sweep: the subject must carry a verifiable merkle path,
// and at least one input must spend a funding output carrying our tag
export async function checkSweep(beef: number[], o: { tracker: ChainTracker; heldOutpoints?: (op: string) => boolean }): Promise<SweepCheck> {
  try {
    const tx = Transaction.fromAtomicBEEF(beef)
    if (!tx.merklePath || !(await tx.merklePath.verify(tx.id('hex'), o.tracker))) return { ok: false, reason: 'unproven' }
    const spends: string[] = []
    for (const i of tx.inputs) {
      const p = i.sourceTransaction
      // when the subject is proven the beef may carry no ancestors: the outpoint alone is the key
      const op = `${i.sourceTXID ?? p?.id('hex')}:${i.sourceOutputIndex}`
      if (p !== undefined) {
        const out = p.outputs[i.sourceOutputIndex]
        if (out === undefined || decodeFunding(out.lockingScript, Array.from(FUNDING_TAG)) === undefined) continue
      }
      if (o.heldOutpoints === undefined || o.heldOutpoints(op)) spends.push(op)
    }
    if (spends.length === 0) return { ok: false, reason: 'no-held-outpoint' }
    return { ok: true, spends }
  } catch (e) {
    return { ok: false, reason: `parse:${(e as Error).message.slice(0, 80)}` }
  }
}

function extractPayload(tx: Transaction): Uint8Array {
  return Uint8Array.from(decodeStrictPushDrop(tx.outputs[0].lockingScript)?.fields?.[0] ?? [])
}

export function carrierLockingKey(pubHex: string): string {
  return readerLockingKey(DERIVATION.protocol, DERIVATION.keyID, pubHex).toString()
}
