import { Transaction, PublicKey } from '@bsv/sdk'
import { verifyWant, verifyOffer, type SignedWant, type SignedOffer } from '@nektar/bstack'
import { verifyOfferSig, parseBuyWrapped } from '@nektar/identity'
import { DisclosureSaleHost, buildP2PKHScript } from '@nektar/anchor'
import { sha256, toHex } from '@nektar/core'
import type { AutomationStore, Operation } from './store.js'

export interface TradeEvidence {
  v: 1
  network: string
  want: SignedWant
  offers: SignedOffer[]
  selectedOfferKey: string
  ranking: unknown
  escrow: { txid: string, txHex: string, vout: number, depositorPkh: string }
  disclosure: { txid: string, txHex: string }
  buyerEvents: unknown[]
  claims: { confirmation: 'broadcast-accepted-not-proven' | 'proven', semantic: 'not_asserted' }
}

export async function collectTradeEvidence (store: AutomationStore, op: Operation, fetchTx: (txid: string) => Promise<Transaction | undefined>): Promise<TradeEvidence> {
  const offers = Object.values(op.data.offers as Record<string, { so: SignedOffer }>).map(o => o.so)
  const utxo = op.data.utxo as { txid: string, vout: number }
  const result = op.data.result as { disclosureTxid: string }
  const escrowTx = await fetchTx(utxo.txid)
  const discTx = await fetchTx(result.disclosureTxid)
  if (!escrowTx || !discTx) throw new Error('evidence: escrow or disclosure transaction is not retrievable')
  return {
    v: 1, network: op.network, want: op.data.want as SignedWant, offers,
    selectedOfferKey: (op.data.selected as { key: string }).key, ranking: op.data.ranking,
    escrow: { txid: utxo.txid, txHex: escrowTx.toHex(), vout: utxo.vout, depositorPkh: (op.data.escrow as { depositorPkh: string }).depositorPkh },
    disclosure: { txid: result.disclosureTxid, txHex: discTx.toHex() },
    buyerEvents: await store.events(op.operationId),
    claims: { confirmation: discTx.merklePath ? 'proven' : 'broadcast-accepted-not-proven', semantic: 'not_asserted' }
  }
}

export function verifyTradeEvidence (e: TradeEvidence): { ok: boolean, checks: Array<{ name: string, ok: boolean }> } {
  const checks: Array<{ name: string, ok: boolean }> = []
  const check = (name: string, fn: () => boolean) => { let ok = false; try { ok = fn() } catch { ok = false } checks.push({ name, ok }) }
  const w = e.want.want
  const sel = e.offers.find(o => `${o.offer.pub}:${o.offer.offerId}` === e.selectedOfferKey)
  const q = sel?.offer.offer as any
  const pkh = (pub: string) => Buffer.from(PublicKey.fromString(pub).toHash() as number[]).toString('hex')
  check('want signature', () => verifyWant(w, e.want.sig, { allowExpired: true }) && w.net === e.network)
  check('every recorded offer is signed and bound to this want', () => e.offers.length > 0 && e.offers.every(o => verifyOffer(o.offer, o.sig, { allowExpired: true }) && o.offer.wantId === w.wantId && o.offer.taskHash === w.taskHash && o.offer.buyerPub === w.pub))
  check('selected offer exists and its quote is signed by the seller', () => !!sel && q.sellerPub === sel.offer.pub && (verifyOfferSig(q, w.pub, sel.offer.leaf ?? '') || verifyOfferSig(q, '', sel.offer.leaf ?? '')))
  check('price is within the signed want ceiling', () => sel!.offer.priceSats === Number(q.cfg.price) && (w.maxSats === undefined || sel!.offer.priceSats <= w.maxSats))
  const escrow = Transaction.fromHex(e.escrow.txHex)
  const disc = Transaction.fromHex(e.disclosure.txHex)
  check('escrow transaction id', () => escrow.id('hex') === e.escrow.txid)
  check('escrow output carries the quoted covenant for the buyer', () => {
    const cfg = { ...q.cfg, price: BigInt(q.cfg.price), refundAfter: BigInt(q.cfg.refundAfter), maxFee: BigInt(q.cfg.maxFee) }
    return e.escrow.depositorPkh === pkh(w.pub) && escrow.outputs[e.escrow.vout]!.lockingScript.toHex() === DisclosureSaleHost.escrowScript(cfg, e.escrow.depositorPkh)
  })
  check('disclosure spends the escrow output', () => disc.id('hex') === e.disclosure.txid && disc.inputs[0]!.sourceTXID === e.escrow.txid && (disc.inputs[0]!.sourceOutputIndex ?? 0) === e.escrow.vout)
  check('disclosure pays the seller exactly the quoted price', () => disc.outputs[0]!.satoshis === Number(q.cfg.price) && disc.outputs[0]!.lockingScript.toHex() === buildP2PKHScript(pkh(sel!.offer.pub)))
  check('revealed value matches the committed hash', () => toHex(sha256(parseBuyWrapped(disc))) === q.cfg.hWrapped)
  return { ok: checks.every(c => c.ok), checks }
}
