// funds.ts — the one owner of "which coins do I have". Every piece of
// spent/owned bookkeeping in the system lives behind this interface: take
// reserves an outpoint for a spend, spent() consumes a broadcast tx's inputs
// and adopts its change, died() frees a spend that never landed, adopt()
// imports outputs someone else paid us. Selection (smallest-sufficient,
// lease/hint filters) is inside the implementations — callers never pick
// outpoints themselves. check-arch fails on spent/owned bookkeeping outside
// the Funds implementations.
import { P2PKH, type Transaction } from '@bsv/sdk'
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
  /** the tx was signed and exported for someone ELSE to air (e.g. a BEEF
   *  payment handed to a seller): the inputs are consumed — they are signed
   *  away either way — but outputs paying us are NOT adopted; if the spend
   *  lands, discovery adopts the change then */
  exported(tx: Transaction): void | Promise<void>
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

/** the funder shapes existing code hands us — take(), takeReserve(), or a
 *  bare utxos list; probed, not widened */
export interface FunderLike {
  take?: (satoshis: number) => Promise<FundingOut>
  takeReserve?: (
    spendCheck?: (txid: string, vout: number) => Promise<boolean>,
    minSats?: number,
    skip?: (tx: Transaction, vout: number) => Promise<boolean>
  ) => Promise<FundingOut | null>
  utxos?: Array<{ tx: Transaction; vout: number; satoshis?: number }>
  markSpent?: (txid: string, vout: number) => void
  addOwned?: (tx: Transaction, vout: number) => void
  address?: () => Promise<string> | string
}

/** Adapt any legacy funder shape into Funds. Tombstones (consumed inputs,
 *  died outputs) live here — bases like MemoryFunder only know their live
 *  pool, so adopt() consults the tombstones first. When the base offers
 *  takeReserve, reservations ride its `leased` filter; a base with only a
 *  destructive take is already the reservation. */
export function fundsOf(f: FunderLike, dealFloor = 0): Funds {
  const spentLog = new Set<string>()
  const dead = new Set<string>()
  const reserved = new Map<string, number>()
  const lockOf = async (): Promise<string> => {
    const a = await f.address?.()
    if (a === undefined) throw new Error('funds: funder has no address()')
    return new P2PKH().lock(a).toHex()
  }
  const consume = (tx: Transaction): void => {
    for (const inp of tx.inputs) {
      const id = inp.sourceTXID ?? inp.sourceTransaction?.id('hex')
      if (id === undefined) continue
      const vout = inp.sourceOutputIndex ?? 0
      spentLog.add(op(id, vout))
      reserved.delete(op(id, vout))
      f.markSpent?.(id, vout)
    }
  }
  const revive = (tx: Transaction, lock: string): void => {
    for (const inp of tx.inputs) {
      const src = inp.sourceTransaction
      const id = inp.sourceTXID ?? src?.id('hex')
      const vout = inp.sourceOutputIndex ?? 0
      if (id === undefined || src === undefined) continue
      reserved.delete(op(id, vout))
      spentLog.delete(op(id, vout))
      if ((src.outputs[vout]?.lockingScript.toHex() ?? '') !== lock) continue
      if ((f.utxos ?? []).some((u) => op(u.tx.id('hex'), u.vout) === op(id, vout))) continue
      f.addOwned?.(src, vout)
    }
  }
  const funds: Funds = {
    address: () => f.address?.() ?? Promise.reject(new Error('funds: funder has no address()')),
    balance: async () => (f.utxos ?? []).filter((u) => !dead.has(op(u.tx.id('hex'), u.vout)) && !spentLog.has(op(u.tx.id('hex'), u.vout)) && !reserved.has(op(u.tx.id('hex'), u.vout))).reduce((s, u) => s + (u.satoshis ?? 0), 0),
    outs: async () => (f.utxos ?? []).filter((u) => !dead.has(op(u.tx.id('hex'), u.vout)) && !spentLog.has(op(u.tx.id('hex'), u.vout)) && !reserved.has(op(u.tx.id('hex'), u.vout))).map((u) => ({ tx: u.tx, vout: u.vout, satoshis: u.satoshis ?? 0 })),
    async take(purpose: FundsPurpose, sats: number): Promise<FundingOut> {
      const committed = [...reserved.values()].reduce((s, v) => s + v, 0)
      if (purpose !== 'deal' && dealFloor > 0 && (await funds.balance()) - committed - sats < dealFloor)
        throw new Error(`funds: ${purpose} take of ${sats} sats would drop the balance below the deal floor (${dealFloor})`)
      const leased = funds.leased
      const merged = (t: string, v: number): Promise<boolean> => Promise.resolve(reserved.has(op(t, v)) || (leased?.(t, v) ?? false))
      if (f.takeReserve !== undefined) {
        const u = await f.takeReserve(funds.spendCheck, sats, async (tx, v) => (await merged(tx.id('hex'), v)) || (funds.hintSkip?.(tx, v) ?? false))
        if (u === null) throw new Error(`funds: no outpoint of ${sats} sats or more`)
        reserved.set(op(u.tx.id('hex'), u.vout), u.satoshis)
        return u
      }
      // destructive-take base (MemoryFunder/StackFunder): the take IS the
      // reservation — lease/hint filters retry a bounded few picks; a refused
      // pick goes straight back
      for (let i = 0; i < 8; i++) {
        const u = f.take !== undefined ? await f.take(sats) : (f.utxos ?? []).find((x) => (x.satoshis ?? 0) >= sats)
        if (u === undefined) throw new Error(`funds: no outpoint of ${sats} sats or more`)
        const id = u.tx.id('hex')
        if (dead.has(op(id, u.vout)) || spentLog.has(op(id, u.vout)) || (await merged(id, u.vout)) || (funds.hintSkip !== undefined && (await funds.hintSkip(u.tx, u.vout)))) {
          f.addOwned?.(u.tx, u.vout)
          continue
        }
        if (funds.spendCheck !== undefined && (await funds.spendCheck(id, u.vout))) {
          spentLog.add(op(id, u.vout))
          f.markSpent?.(id, u.vout)
          continue
        }
        reserved.set(op(id, u.vout), u.satoshis)
        return u
      }
      throw new Error(`funds: no eligible outpoint of ${sats} sats or more`)
    },
    async spent(tx: Transaction): Promise<void> {
      consume(tx)
      const lock = await lockOf()
      const tid = tx.id('hex')
      tx.outputs.forEach((o, vout) => {
        if (o.lockingScript.toHex() !== lock) return
        if (dead.has(op(tid, vout)) || spentLog.has(op(tid, vout))) return
        if ((f.utxos ?? []).some((u) => op(u.tx.id('hex'), u.vout) === op(tid, vout))) return
        f.addOwned?.(tx, vout)
      })
    },
    exported(tx: Transaction): void {
      consume(tx)
    },
    async died(tx: Transaction): Promise<void> {
      const tid = tx.id('hex')
      tx.outputs.forEach((_o, vout) => {
        dead.add(op(tid, vout))
        f.markSpent?.(tid, vout)
      })
      revive(tx, await lockOf())
    },
    adopt(tx: Transaction, vout: number): boolean {
      const k = op(tx.id('hex'), vout)
      if (dead.has(k) || spentLog.has(k)) return false
      if ((f.utxos ?? []).some((u) => op(u.tx.id('hex'), u.vout) === k)) return false
      f.addOwned?.(tx, vout)
      return true
    }
  }
  return funds
}

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
