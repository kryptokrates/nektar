// funds.ts — the one owner of "which coins do I have". Every piece of
// spent/owned bookkeeping in the system lives behind this interface: take
// reserves an outpoint for a spend, spent() consumes a broadcast tx's inputs
// and adopts its change, died() frees a spend that never landed, adopt()
// imports outputs someone else paid us. Selection (smallest-sufficient,
// lease/hint filters) is inside the implementations — callers never pick
// outpoints themselves. check-arch fails on spent/owned bookkeeping outside
// the Funds implementations.
import type { Transaction } from '@bsv/sdk'
import type { FundingOut } from './interfaces.js'

/** why an output is being taken — allocation policy, not bookkeeping:
 *  a `deal` spend takes freely; `publish`/`upkeep` must leave the deal floor
 *  untouched so an order can always afford to settle */
export type FundsPurpose = 'deal' | 'publish' | 'upkeep'

export interface Funds {
  /** our receive address — funding and change outputs lock to it */
  address(): Promise<string> | string
  /** spendable sats right now */
  balance(): Promise<number>
  /** the spendable view — fundingStatus and tests read it */
  outs(): Promise<FundingOut[]>
  /** reserve an outpoint of at least `sats` for `purpose`. Non-destructive:
   *  the outpoint is consumed only when the built tx broadcasts and spent()
   *  lands. Throws when nothing eligible exists; `publish`/`upkeep` takes
   *  also refuse when the take would drop the balance below dealFloorSats. */
  take(purpose: FundsPurpose, sats: number): Promise<FundingOut>
  /** one call after a successful broadcast: the tx's inputs are consumed and
   *  its outputs paying our address are adopted */
  spent(tx: Transaction): void | Promise<void>
  /** the spend was rejected or abandoned — it never landed: its inputs are
   *  free again and its outputs are tombstoned (adopt refuses them later) */
  died(tx: Transaction): void | Promise<void>
  /** coins sent by someone else; refuses outpoints we already spent */
  adopt(tx: Transaction, vout: number): Promise<boolean> | boolean
  /** outpoint lease hook — the automation ledger's prepared-tx leases plug in
   *  here so concurrent takes never pick an input a prepared tx claims */
  leased?: (txid: string, vout: number) => Promise<boolean>
  /** hint-only pass-over (e.g. a broadcaster says this candidate's parent
   *  was rejected) — never writes bookkeeping, only skips this take */
  hintSkip?: (tx: Transaction, vout: number) => Promise<boolean>
  /** first-party spend probe (chain utxo status / wallet truth): a 'spent'
   *  answer drops the candidate permanently */
  spendCheck?: (txid: string, vout: number) => Promise<boolean>
}

export const op = (txid: string, vout: number): string => `${txid}:${vout}`

/** shared selection: smallest-sufficient candidate ≥ floor, skipping leased
 *  and hint-dead outpoints. The `skip` callback is hint-only — it never
 *  marks or drops (a rejected-parent hint is not first-party truth) */
export const pickSpendable = async (
  utxos: FundingOut[],
  floor: number,
  leased: ((txid: string, vout: number) => Promise<boolean>) | undefined,
  skip: ((tx: Transaction, vout: number) => Promise<boolean>) | undefined,
  spendCheck: ((txid: string, vout: number) => Promise<boolean>) | undefined,
  onSpent: (txid: string, vout: number) => void | Promise<void>
): Promise<FundingOut | undefined> => {
  const eligible: FundingOut[] = []
  for (const u of utxos.filter((x) => x.satoshis >= floor).sort((a, b) => a.satoshis - b.satoshis)) {
    const txid = u.tx.id('hex')
    if (leased !== undefined && (await leased(txid, u.vout))) continue
    if (skip !== undefined && (await skip(u.tx, u.vout))) continue
    eligible.push(u)
  }
  for (const u of eligible) {
    const txid = u.tx.id('hex')
    if (spendCheck !== undefined && (await spendCheck(txid, u.vout))) {
      await onSpent(txid, u.vout)
      continue
    }
    return u
  }
  return undefined
}
