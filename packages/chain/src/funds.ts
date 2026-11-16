// funds.ts — the single owner of coin state (Stage 2): selection,
// reservation, spend marks, abandonment, adoption. Callers never mark
// spent or owned directly — they hand whole transactions to `spent`,
// `died` and `adopt` and the implementation decides which outpoints live
// or die. FundsBase holds the shared semantics (purpose floor, reserved
// set, serialized takes, change detection); a FundsStore supplies the
// backend — wallet basket, in-memory pool, or test treasury.
import { Transaction } from '@bsv/sdk'
import type { Funder, FundingOut, Funds, FundPurpose } from './interfaces.js'

/** a narrow Funder view over a Funds owner — every take is purpose-labeled.
 *  Anchor/AutomatonChain and other `Funder`-shaped consumers sit behind
 *  this; the label travels through to the allocation floor. */
export function fundsAsFunder(funds: Funds, purpose: FundPurpose): Funder & { funds: Funds } {
  return {
    take: async (sats) => await funds.take(purpose, sats),
    address: () => funds.address(),
    funds
  }
}

/** what the backend owes the shared semantics — four primitive verbs over
 *  the owned-outpoint set. Implementations: wallet basket (automation's
 *  WalletFunds), in-memory pool (MemoryFunds), test treasury (StackFunder). */
export interface FundsStore {
  /** owned unspent candidates — the impl decides what 'unspent' means */
  list: () => Promise<FundingOut[]> | FundingOut[]
  /** record an owned output (internalize / persist / pool-push) */
  add: (tx: Transaction, vout: number) => Promise<void> | void
  /** remove an owned output — spent on-chain or dead record */
  drop: (txid: string, vout: number) => Promise<void> | void
  /** bring a previously-owned input back — the spend consuming it died
   *  while its parent still lives. Default: re-add via tx+vout. */
  revive?: (tx: Transaction, vout: number) => Promise<void> | void
  /** persistent tombstone — an outpoint we deliberately marked spent */
  tombstoned?: (txid: string, vout: number) => Promise<boolean> | boolean
  /** already in the owned set? Default: a list() scan. */
  has?: (txid: string, vout: number) => Promise<boolean> | boolean
}

export interface FundsDeps {
  /** our pubkey-hash hex — identifies owned outputs and change
   *  (every adopted/change output is P2PKH-to-us) */
  pkhHex: string
  /** pre-encoded address override; derived from pkhHex (0x6f) otherwise */
  address?: string
  /** sats kept affordable for one deal — non-deal takes never dip below.
   *  Default 25_000. */
  dealFloor?: number
  /** durable cross-process lease — the automation ledger's outpoint lease */
  leased?: (txid: string, vout: number) => Promise<boolean> | boolean
  /** resolve a txid to its tx — `died` needs the dead tx's inputs/outputs */
  txOf?: (txid: string) => Promise<Transaction | undefined> | Transaction | undefined
  /** candidate's parent is dead — never selected (a hint, not a verdict) */
  deadParent?: (tx: Transaction) => Promise<boolean>
  /** attach the proof we hold for a candidate's parent tx */
  proofOf?: (tx: Transaction) => Promise<void>
  /** first-party spend record — we already spent this outpoint ourselves */
  spentByUs?: (txid: string, vout: number) => Promise<boolean> | boolean
  /** a purpose's take served by a dedicated pool instead of the shared set —
   *  the fee-vault covenant mints capped outs for `publish` takes; it returns
   *  undefined when it has nothing to serve (fall through to the pool) */
  takeOverride?: (purpose: FundPurpose, sats: number) => Promise<FundingOut | undefined>
  log?: (s: string) => void
}

const op = (txid: string, vout: number): string => `${txid}:${vout}`

export abstract class FundsBase implements Funds {
  /** in-process reservations: take() holds the outpoint until spent/died/
   *  release — two concurrent spends can never receive the same input */
  protected readonly reserved = new Set<string>()
  /** serialized take() — the async list/lease/proof gap must not interleave */
  private queue: Promise<unknown> = Promise.resolve()
  protected abstract readonly store: FundsStore

  /** mutable so late-bound seams can be wired after construction — the
   *  ledger's lease predicate and first-party spend record only exist once
   *  the records db opens, after the impl is already assembled */
  constructor(public deps: FundsDeps) {}

  private get ourLock(): string {
    return `76a914${this.deps.pkhHex}88ac`
  }

  address(): Promise<string> | string {
    return this.deps.address ?? `1${this.deps.pkhHex.slice(0, 24)}`
  }

  async balance(): Promise<number> {
    let sum = 0
    for (const u of await this.store.list()) {
      if (!this.reserved.has(op(u.tx.id('hex'), u.vout))) sum += u.satoshis
    }
    return sum
  }

  /** un-reserve an outpoint — the spend built on it was never broadcast */
  release(txid: string, vout: number): void {
    this.reserved.delete(op(txid, vout))
  }

  /** reserve a specific outpoint — the fee-vault pool and funding-import
   *  paths hold a named output rather than letting take() choose. Atomic:
   *  returns false when the outpoint is already held by another spend. */
  hold(txid: string, vout: number): boolean {
    const o = op(txid, vout)
    if (this.reserved.has(o)) return false
    this.reserved.add(o)
    return true
  }

  async list(): Promise<FundingOut[]> {
    return await this.store.list()
  }

  async available(): Promise<FundingOut[]> {
    return (await this.store.list()).filter((u) => !this.reserved.has(op(u.tx.id('hex'), u.vout)))
  }

  /** backends with a remote store re-query here — default no-op covers
   *  in-memory stores */
  async refresh(): Promise<void> {}

  /** an observed spend consumed this outpoint on-chain — drop it from the
   *  owned set (a spend we did not build gets the same mark ours would) */
  async drop(txid: string, vout: number): Promise<void> {
    await this.store.drop(txid, vout)
    this.reserved.delete(op(txid, vout))
  }

  /** a resolved candidate's outpoint id */
  private async unusable(u: FundingOut): Promise<boolean> {
    const id = u.tx.id('hex')
    if (this.reserved.has(op(id, u.vout))) return true
    if ((await this.store.tombstoned?.(id, u.vout)) === true) return true
    if ((await this.deps.leased?.(id, u.vout)) === true) return true
    // a proven parent cannot be a dead parent — the hint is never consulted
    if (u.tx.merklePath === undefined && (await this.deps.deadParent?.(u.tx)) === true) return true
    return false
  }

  take(purpose: FundPurpose, sats: number): Promise<FundingOut> {
    const run = this.queue.then(async () => {
      const override = await this.deps.takeOverride?.(purpose, sats)
      if (override !== undefined) {
        this.reserved.add(op(override.tx.id('hex'), override.vout))
        if (override.tx.merklePath === undefined) await this.deps.proofOf?.(override.tx)
        return override
      }
      const all = await this.store.list()
      const floor = this.deps.dealFloor ?? 25_000
      if (purpose !== 'deal') {
        let owned = 0
        for (const u of all) if (!this.reserved.has(op(u.tx.id('hex'), u.vout))) owned += u.satoshis
        if (owned - sats < floor) throw new Error(`funds: ${purpose} take of ${sats} sats would leave ${owned - sats} < deal floor ${floor}`)
      }
      const candidates: FundingOut[] = []
      for (const u of all) {
        if (u.satoshis < sats || (await this.unusable(u))) continue
        candidates.push(u)
      }
      candidates.sort((a, b) => a.satoshis - b.satoshis)
      const pick = candidates[0]
      if (pick === undefined) throw new Error(`funds: no spendable output ≥ ${sats} sats`)
      this.reserved.add(op(pick.tx.id('hex'), pick.vout))
      if (pick.tx.merklePath === undefined) await this.deps.proofOf?.(pick.tx)
      return pick
    })
    this.queue = run.catch(() => {})
    return run
  }

  /** input outpoint id — sourceTXID or the held sourceTransaction's id */
  private static inputOp(inp: Transaction['inputs'][number]): string | undefined {
    const txid = inp.sourceTXID ?? inp.sourceTransaction?.id('hex')
    return txid !== undefined ? op(txid, inp.sourceOutputIndex) : undefined
  }

  async spent(tx: Transaction): Promise<void> {
    for (const inp of tx.inputs) {
      const io = FundsBase.inputOp(inp)
      if (io === undefined) continue
      const [txid, v] = io.split(':')
      await this.store.drop(txid, Number(v))
      this.reserved.delete(io)
    }
    for (const [vout, o] of tx.outputs.entries()) {
      if (o.lockingScript.toHex() !== this.ourLock) continue
      if ((await this.store.has?.(tx.id('hex'), vout)) === true) continue
      await this.store.add(tx, vout)
    }
  }

  async died(txid: string): Promise<void> {
    const tx = await this.deps.txOf?.(txid)
    if (tx === undefined) {
      this.deps.log?.(`funds: died(${txid.slice(0, 16)}…) — tx not held, nothing to free`)
      return
    }
    for (const inp of tx.inputs) {
      const io = FundsBase.inputOp(inp)
      if (io === undefined) continue
      this.reserved.delete(io)
      const [pid, v] = io.split(':')
      const vout = Number(v)
      if ((await this.store.has?.(pid, vout)) === true) continue
      if ((await this.store.tombstoned?.(pid, vout)) !== true) continue
      const parent = inp.sourceTransaction ?? (await this.deps.txOf?.(pid))
      if (parent === undefined || (await this.deps.deadParent?.(parent)) === true) continue
      if (this.store.revive !== undefined) await this.store.revive(parent, vout)
      else await this.store.add(parent, vout)
    }
    for (const [vout, o] of tx.outputs.entries()) {
      if (o.lockingScript.toHex() === this.ourLock) await this.store.drop(txid, vout)
    }
  }

  /** verified-live re-ownership of a tombstoned out — reconcile's verb.
   *  `adopt` respects tombstones; `revive` lifts one the caller proved is
   *  still live on-chain. */
  async revive(tx: Transaction, vout: number): Promise<void> {
    const txid = tx.id('hex')
    if (this.store.revive !== undefined) await this.store.revive(tx, vout)
    else await this.store.add(tx, vout)
    this.reserved.delete(op(txid, vout))
  }

  async adopt(tx: Transaction, vout: number): Promise<void> {
    const txid = tx.id('hex')
    if ((await this.store.tombstoned?.(txid, vout)) === true) return
    if ((await this.deps.spentByUs?.(txid, vout)) === true) return
    if ((await this.store.has?.(txid, vout)) === true) return
    await this.store.add(tx, vout)
  }
}

/** array-backed store — in-memory pools and test treasuries share it. The
 *  returned object is the durable record: two MemoryFunds over one store
 *  is a "restart" — the new instance sees the same outpoints with no
 *  re-import. */
export interface ListFundsStore extends FundsStore {
  readonly outs: FundingOut[]
  readonly tombstones: Set<string>
}

export function listFundsStore(): ListFundsStore {
  const outs: FundingOut[] = []
  const tombstones = new Set<string>()
  return {
    outs,
    tombstones,
    list: () => outs.filter((u) => !tombstones.has(op(u.tx.id('hex'), u.vout))),
    has: (txid, vout) => outs.some((u) => u.tx.id('hex') === txid && u.vout === vout),
    add: (tx, vout) => {
      const o = tx.outputs[vout]
      if (o === undefined) throw new Error(`funds: adopt out-of-range vout ${vout}`)
      tombstones.delete(op(tx.id('hex'), vout))
      if (!outs.some((u) => u.tx.id('hex') === tx.id('hex') && u.vout === vout)) {
        outs.push({ tx, vout, satoshis: o.satoshis ?? 0 })
      }
    },
    drop: (txid, vout) => {
      const i = outs.findIndex((u) => u.tx.id('hex') === txid && u.vout === vout)
      // tombstone only what was ours — a foreign input consumed by our tx
      // must not become revivable (it was never ours to bring back)
      if (i >= 0) {
        outs.splice(i, 1)
        tombstones.add(op(txid, vout))
      }
    },
    revive: (tx, vout) => {
      tombstones.delete(op(tx.id('hex'), vout))
      const o = tx.outputs[vout]
      if (o !== undefined && !outs.some((u) => u.tx.id('hex') === tx.id('hex') && u.vout === vout)) {
        outs.push({ tx, vout, satoshis: o.satoshis ?? 0 })
      }
    },
    tombstoned: (txid, vout) => tombstones.has(op(txid, vout))
  }
}

/** in-memory Funds — the default test tier's coin owner. Pass a shared
 *  `listFundsStore()` to two instances to simulate restart persistence. */
export class MemoryFunds extends FundsBase {
  protected readonly store: ListFundsStore

  constructor(store: ListFundsStore | undefined, deps: FundsDeps) {
    super(deps)
    this.store = store ?? listFundsStore()
  }

  /** the mutable record — for restart simulations and direct inspection */
  get records(): ListFundsStore {
    return this.store
  }
}
