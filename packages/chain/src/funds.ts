// funds.ts — the single owner of coin state (Stage 2): selection,
// reservation, spend marks, abandonment, adoption. Callers never mark
// spent or owned directly — they hand whole transactions to `spent`,
// `died` and `adopt` and the implementation decides which outpoints live
// or die. FundsBase holds the shared semantics (purpose floor, reserved
// set, serialized takes, change detection); a FundsStore supplies the
// backend — wallet basket, in-memory pool, or test treasury.
import { Transaction, P2PKH } from '@bsv/sdk'
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
  /** enumerate owned tombstones — the starve-time sweep proves an orphaned
   *  mark (its consuming draft left no candidate to heal through) unspent
   *  and revives it directly. Entries may carry the retained FundingOut;
   *  txOf resolves the rest. */
  tombstonedList?: () =>
    | Array<{ txid: string; vout: number; satoshis?: number; tx?: Transaction }>
    | Promise<Array<{ txid: string; vout: number; satoshis?: number; tx?: Transaction }>>
  /** already in the owned set? Default: a list() scan. */
  has?: (txid: string, vout: number) => Promise<boolean> | boolean
}

export interface FundsDeps {
  /** our pubkey-hash hex — identifies owned outputs and change
   *  (every adopted/change output is P2PKH-to-us). Optional when `lock`
   *  is supplied. */
  pkhHex?: string
  /** explicit locking-script hex override (adapters that only hold an
   *  address) — wins over the pkhHex-derived P2PKH script */
  lock?: string
  /** pre-encoded address override; derived from pkhHex (0x6f) otherwise */
  address?: string
  /** sats kept affordable for one deal — non-deal takes never dip below.
   *  Default 25_000. */
  dealFloor?: number
  /** durable cross-process lease — the automation ledger's outpoint lease.
   *  Seeds the same-named field on the impl — late-bound seams assign
   *  `funds.leased = …` after construction. */
  leased?: (txid: string, vout: number) => Promise<boolean> | boolean
  /** resolve a txid to its tx — `died` needs the dead tx's inputs/outputs */
  txOf?: (txid: string) => Promise<Transaction | undefined> | Transaction | undefined
  /** candidate's parent is dead — never selected (a hint, not a verdict).
   *  Seeds the `hintSkip` field. */
  hintSkip?: (tx: Transaction, vout: number) => Promise<boolean>
  /** attach the proof we hold for a candidate's parent tx */
  proofOf?: (tx: Transaction) => Promise<void>
  /** first-party spend probe — a 'spent' answer burns the candidate during
   *  selection. Seeds the `spendCheck` field. */
  spendCheck?: (txid: string, vout: number) => Promise<boolean>
  /** starve-sweep revive oracle, listing form: positively-listed-live
   *  outpoint ids ('txid:vout') — an index's address-utxos page or a
   *  first-party utxo view. A tombstone the listing positively names is
   *  lifted. This is a claim, not SPV — safe for self-bookkeeping: a
   *  falsely-revived mark self-corrects at pick-time spendCheck or the
   *  broadcaster's reject; a stuck one starves the pool for good. One call
   *  per starved take, never per-outpoint. Seeds the `liveSet` field. */
  liveSet?: () => Promise<Set<string> | undefined>
  /** starve-sweep revive oracle, per-outpoint form — 'unspent' is only
   *  honestly answerable where the utxo view is first-party (a node's own
   *  utxo set, the wallet's spendable listing). Used when no `liveSet` is
   *  wired; probes are capped. Seeds the `liveCheck` field. */
  liveCheck?: (txid: string, vout: number) => Promise<boolean>
  /** the candidate's parent tx is refusal-class dead — a first-party
   *  verdict or the broadcaster's own REJECTED word on a tx that left our
   *  hands. `take` answers it with died(): the phantom's inputs revive
   *  (each re-heals on its own evaluation), the dead tx's owned outputs
   *  drop. Bookkeeping only — no chain verdict is written. Seeds the
   *  `deadParent` field. */
  deadParent?: (tx: Transaction) => Promise<boolean>
  /** a purpose's take served by a dedicated pool instead of the shared set —
   *  the fee-vault covenant mints capped outs for `publish` takes; it returns
   *  undefined when it has nothing to serve (fall through to the pool).
   *  Seeds the `takeOverride` field. */
  takeOverride?: (purpose: FundPurpose, sats: number) => Promise<FundingOut | undefined>
  log?: (s: string) => void
}

const op = (txid: string, vout: number): string => `${txid}:${vout}`

/** starvation is a typed signal — callers bounce the deal on it across a
 *  package boundary, so it cannot ride message text that a refactor breaks */
export class FundsStarvedError extends Error {
  readonly code = 'STARVED' as const
}

export abstract class FundsBase implements Funds {
  /** in-process reservations: take() holds the outpoint until spent/died/
   *  release — two concurrent spends can never receive the same input */
  protected readonly reserved = new Set<string>()
  /** serialized take() — the async list/lease/proof gap must not interleave */
  private queue: Promise<unknown> = Promise.resolve()
  protected abstract readonly store: FundsStore

  /** late-bound behavior hooks — interface-level fields so callers that
   *  only know `Funds` (never the impl's deps) can still wire the lease,
   *  hint and spend-probe seams after construction */
  public leased?: (txid: string, vout: number) => Promise<boolean> | boolean
  public hintSkip?: (tx: Transaction, vout: number) => Promise<boolean>
  public deadParent?: (tx: Transaction) => Promise<boolean>
  public spendCheck?: (txid: string, vout: number) => Promise<boolean>
  public liveSet?: () => Promise<Set<string> | undefined>
  public liveCheck?: (txid: string, vout: number) => Promise<boolean>
  public txOf?: (txid: string) => Promise<Transaction | undefined> | Transaction | undefined
  public takeOverride?: (purpose: FundPurpose, sats: number) => Promise<FundingOut | undefined>
  /** sats kept affordable for one deal — non-deal takes never dip below;
   *  seeded from deps.dealFloor, re-tunable post-construction */
  public dealFloor?: number

  /** deps holds construction-time config (identity, floor, tx view); the
   *  hook fields above are seeded from it but remain mutable — the
   *  ledger's lease predicate and first-party spend record only exist once
   *  the records db opens, after the impl is already assembled */
  constructor(public deps: FundsDeps) {
    this.leased = deps.leased
    this.hintSkip = deps.hintSkip
    this.deadParent = deps.deadParent
    this.spendCheck = deps.spendCheck
    this.liveSet = deps.liveSet
    this.liveCheck = deps.liveCheck
    this.txOf = deps.txOf
    this.takeOverride = deps.takeOverride
    this.dealFloor = deps.dealFloor
  }

  /** the locking script that marks an output as ours (change, adoptions).
   *  Deps-supplied lock or pkhHex-derived P2PKH; wallet impls override to
   *  derive it from the wallet itself. */
  protected async ourLock(): Promise<string> {
    return this.deps.lock ?? `76a914${this.deps.pkhHex}88ac`
  }

  address(): Promise<string> | string {
    return this.deps.address ?? `1${this.deps.pkhHex?.slice(0, 24)}`
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

  async outs(): Promise<FundingOut[]> {
    return (await this.store.list()).filter((u) => {
      try {
        return !this.reserved.has(op(u.tx.id('hex'), u.vout))
      } catch {
        return false // a row that cannot serialize is not spendable stock
      }
    })
  }

  /** backends with a remote store re-query here — default no-op covers
   *  in-memory stores */
  async refresh(): Promise<void> {}

  /** an outpoint is spent for good — observed on-chain spend or a
   *  first-party probe answer; tombstoned, never offered again */
  async burn(txid: string, vout: number): Promise<void> {
    await this.store.drop(txid, vout)
    this.reserved.delete(op(txid, vout))
  }

  /** a resolved candidate's outpoint id — the hint side of selection
   *  (reservation, tombstone, lease, dead-parent refusal): pass-over only,
   *  never writes bookkeeping. 'dead' marks a refusal-class parent so take
   *  can heal exactly once — the probe is asked a single time */
  private async unusable(u: FundingOut, deadMemo: Map<string, boolean>): Promise<'dead' | true | false> {
    let id: string
    try {
      id = u.tx.id('hex')
    } catch {
      return true // an unserializable parent can never fund a spend — pass over
    }
    if (this.reserved.has(op(id, u.vout))) return true
    if ((await this.store.tombstoned?.(id, u.vout)) === true) return true
    if ((await this.leased?.(id, u.vout)) === true) return true
    // a proven parent cannot be a dead parent — the hint is never consulted
    if (u.tx.merklePath === undefined) {
      // the oracles are independent, checked in order of evidence: deadParent
      // (a sealed verdict) answers with died() below; hintSkip passes a hinted
      // parent over — a hint is not a verdict and writes nothing. The memo
      // dedupes across a draft's many outputs — a 250-entry file costs a probe
      // per distinct parent, not per outpoint
      if (this.deadParent !== undefined) {
        let dead = deadMemo.get(id)
        if (dead === undefined) {
          dead = await this.deadParent(u.tx)
          deadMemo.set(id, dead)
        }
        if (dead) return 'dead'
      }
      if ((await this.hintSkip?.(u.tx, u.vout)) === true) return true
    }
    return false
  }

  take(purpose: FundPurpose, sats: number): Promise<FundingOut> {
    const run = this.queue.then(async () => {
      const override = await this.takeOverride?.(purpose, sats)
      if (override !== undefined) {
        this.reserved.add(op(override.tx.id('hex'), override.vout))
        if (override.tx.merklePath === undefined) await this.deps.proofOf?.(override.tx)
        return override
      }
      const all = await this.store.list()
      const floor = this.dealFloor ?? 25_000
      const candidates: FundingOut[] = []
      const deadMemo = new Map<string, boolean>()
      let healed = false
      // the heal pass runs BEFORE the floor and ignores the take's size: a
      // phantom-drained file looks starved until the dead drafts' inputs
      // revive — 'owned' below is only honest on the healed set. A refusal-
      // class parent heals for good: died() frees the phantom's inputs and
      // drops the owned outputs it adopted — an in-flight outpoint is left
      // to its own take
      for (const u of all) {
        const veto = await this.unusable(u, deadMemo)
        if (veto === 'dead') {
          healed = true
          await this.died(u.tx)
          continue
        }
        if (veto === false && u.satoshis >= sats) candidates.push(u)
      }
      // tombstone sweep — the starve last-resort, run only when the live
      // set cannot cover the take. A spent-mark whose consuming draft left
      // no candidate behind (exported carrier, killed process, dropped
      // record) can never reach the heal pass above: list() hides it and
      // no dead child calls died() for it. Here the index's own 'unspent'
      // lifts the mark — probe largest-first, early-exit once covered,
      // capped so a rotten pool can't turn a take into a scan. A falsely-
      // revived mark self-corrects at pick-time spendCheck or the
      // broadcaster's reject; a stuck one starves the book for good.
      if (
        !candidates.some((u) => u.satoshis >= sats) &&
        (this.liveSet !== undefined || this.liveCheck !== undefined) &&
        this.store.tombstonedList !== undefined
      ) {
        const tombs = (await this.store.tombstonedList()).sort((a, b) => (b.satoshis ?? 0) - (a.satoshis ?? 0))
        // the listing oracle answers for the whole book in one call; the
        // per-outpoint probe is the capped fallback where no listing exists
        const set = await this.liveSet?.().catch(() => undefined)
        // non-deal takes owe the floor too — keep reviving until the take's
        // own need plus the reserved deal-floor is covered, not just `sats`
        const want = sats + (purpose === 'deal' ? 0 : (this.dealFloor ?? 25_000))
        let revived = 0,
          probed = 0
        // count bounds the scan; wall-time bounds the scan's *cost* — 48
        // retried probes against a degraded index can exceed the caller's
        // collect window even while the count stays legal
        const sweepUntil = Date.now() + 5_000
        for (const t of tombs) {
          if (probed >= 48 || revived >= want || Date.now() >= sweepUntil) break
          if (t.satoshis !== undefined && t.satoshis < sats) continue
          if (this.reserved.has(op(t.txid, t.vout))) continue
          if ((await this.leased?.(t.txid, t.vout)) === true) continue
          if (set !== undefined) {
            if (!set.has(op(t.txid, t.vout))) continue
            // a listing is the index's bookkeeping claim — where a first-
            // party utxo view exists it corroborates once before a durable
            // tombstone lifts; disagreement is drift, not life
            if (this.liveCheck !== undefined) {
              probed++
              if ((await this.liveCheck(t.txid, t.vout)) !== true) continue
            }
          } else {
            probed++
            if ((await this.liveCheck?.(t.txid, t.vout)) !== true) continue
          }
          const tx = t.tx ?? (await this.txOf?.(t.txid))
          if (tx === undefined) continue
          const fo: FundingOut = { tx, vout: t.vout, satoshis: t.satoshis ?? tx.outputs[t.vout]?.satoshis ?? 0 }
          if (fo.satoshis < sats) continue
          if (this.store.revive !== undefined) await this.store.revive(tx, t.vout)
          else await this.store.add(tx, t.vout)
          healed = true
          revived += fo.satoshis
          candidates.push(fo)
        }
      }
      if (purpose !== 'deal') {
        let owned = 0
        const stock = healed ? await this.store.list() : all
        for (const u of stock) if (!this.reserved.has(op(u.tx.id('hex'), u.vout))) owned += u.satoshis
        if (owned - sats < floor) throw new FundsStarvedError(`funds: ${purpose} take of ${sats} sats would leave ${owned - sats} < deal floor ${floor}`)
      }
      candidates.sort((a, b) => a.satoshis - b.satoshis)
      // first-party truth side: a 'spent' answer burns the candidate for
      // good and selection moves on — a hint only passes over, this drops
      for (const pick of candidates) {
        let id: string
        try {
          id = pick.tx.id('hex')
        } catch {
          continue // a stored tx that cannot serialize can never be spent
        }
        if (pick.tx.outputs[pick.vout]?.satoshis === undefined || pick.tx.outputs[pick.vout]?.lockingScript === undefined) {
          // the carried parent is a stand-in at the spent vout — an input
          // built on it cannot fee or sign. Repair from the full-tx source
          // when one exists; otherwise the row is unspendable stock and the
          // take moves to the next candidate rather than exploding in the
          // consumer (mintTree/fee reads the same fields)
          const full = await Promise.resolve(this.txOf?.(id)).catch(() => undefined)
          if (full?.outputs[pick.vout]?.satoshis !== undefined && full?.outputs[pick.vout]?.lockingScript !== undefined) {
            pick.tx = full
          } else {
            continue
          }
        }
        if ((await this.spendCheck?.(id, pick.vout)) === true) {
          await this.burn(id, pick.vout)
          continue
        }
        this.reserved.add(op(id, pick.vout))
        if (pick.tx.merklePath === undefined) await this.deps.proofOf?.(pick.tx)
        return pick
      }
      throw new FundsStarvedError(`funds: no spendable output ≥ ${sats} sats`)
    })
    this.queue = run.catch(() => {})
    return run
  }

  /** input outpoint id — sourceTXID or the held sourceTransaction's id */
  private static inputOp(inp: Transaction['inputs'][number]): string | undefined {
    let txid = inp.sourceTXID
    if (txid === undefined && inp.sourceTransaction !== undefined) {
      try {
        txid = inp.sourceTransaction.id('hex')
      } catch {
        txid = undefined // an EF/lazy stand-in cannot serialize — sourceTXID is its honest name
      }
    }
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
    const lock = await this.ourLock()
    for (const [vout, o] of tx.outputs.entries()) {
      if (o.lockingScript.toHex() !== lock) continue
      if ((await this.store.has?.(tx.id('hex'), vout)) === true) continue
      await this.store.add(tx, vout)
    }
  }

  /** a signed tx left our hands for someone else to air — the inputs are
   *  consumed (signed away either way) but its outputs are NOT adopted:
   *  discovery adopts the change when the spend lands on-chain. */
  async exported(tx: Transaction): Promise<void> {
    for (const inp of tx.inputs) {
      const io = FundsBase.inputOp(inp)
      if (io === undefined) continue
      const [txid, v] = io.split(':')
      await this.store.drop(txid, Number(v))
      this.reserved.delete(io)
    }
  }

  async died(x: Transaction | string): Promise<void> {
    const txid = typeof x === 'string' ? x : x.id('hex')
    const tx = typeof x === 'string' ? await this.txOf?.(x) : x
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
      const parent = inp.sourceTransaction ?? (await this.txOf?.(pid))
      if (parent === undefined || (await this.hintSkip?.(parent, vout)) === true) continue
      if (this.store.revive !== undefined) await this.store.revive(parent, vout)
      else await this.store.add(parent, vout)
    }
    const lock = await this.ourLock()
    for (const [vout, o] of tx.outputs.entries()) {
      if (o.lockingScript.toHex() === lock) await this.store.drop(txid, vout)
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

  async adopt(tx: Transaction, vout: number): Promise<boolean> {
    const txid = tx.id('hex')
    if ((await this.store.tombstoned?.(txid, vout)) === true) return false
    if ((await this.spendCheck?.(txid, vout)) === true) return false
    if ((await this.store.has?.(txid, vout)) === true) return false
    await this.store.add(tx, vout)
    return true
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
  // dropped outs retained for the starve sweep — the tombstone alone loses
  // the out's tx and amount, and a dead draft's input mark is only
  // revivable if the sweep can still name both
  const tombOuts = new Map<string, FundingOut>()
  const lift = (key: string): FundingOut | undefined => {
    const u = tombOuts.get(key)
    tombOuts.delete(key)
    return u
  }
  return {
    outs,
    tombstones,
    list: () => outs.filter((u) => !tombstones.has(op(u.tx.id('hex'), u.vout))),
    has: (txid, vout) => outs.some((u) => u.tx.id('hex') === txid && u.vout === vout),
    add: (tx, vout) => {
      const o = tx.outputs[vout]
      if (o === undefined) throw new Error(`funds: adopt out-of-range vout ${vout}`)
      lift(op(tx.id('hex'), vout))
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
        const [u] = outs.splice(i, 1)
        tombstones.add(op(txid, vout))
        tombOuts.set(op(txid, vout), u)
      }
    },
    revive: (tx, vout) => {
      tombstones.delete(op(tx.id('hex'), vout))
      const o = tx.outputs[vout]
      const held = lift(op(tx.id('hex'), vout))
      if (o !== undefined && !outs.some((u) => u.tx.id('hex') === tx.id('hex') && u.vout === vout)) {
        outs.push({ tx, vout, satoshis: held?.satoshis ?? o.satoshis ?? 0 })
      }
    },
    tombstoned: (txid, vout) => tombstones.has(op(txid, vout)),
    tombstonedList: () =>
      [...tombOuts.entries()].map(([key, u]) => {
        const [txid, v] = key.split(':')
        return { txid, vout: Number(v), satoshis: u.satoshis, tx: u.tx }
      })
  }
}

/** the generic FundsBase over an arbitrary FundsStore — what adapters hand
 *  out when the backend is a foreign funder shape */
class AdaptedFunds extends FundsBase {
  constructor(
    protected readonly store: FundsStore,
    deps: FundsDeps
  ) {
    super(deps)
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

/** the funder shapes older call sites hand us: a real Funds carrier
 *  (`.funds`) or a utxos-shaped treasury with mark/add verbs. New code
 *  should construct a Funds impl directly — this adapts, never widens. */
export interface FunderLike {
  funds?: Funds
  utxos?: Array<{ tx: Transaction; vout: number; satoshis?: number }>
  markSpent?: (txid: string, vout: number) => void
  addOwned?: (tx: Transaction, vout: number) => void
  unmark?: (txid: string, vout: number) => void
  /** markSpent's read side — without it the adapter cannot tell a tombstoned
   *  own outpoint from a foreign input, and died()'s revive guard skips
   *  freeing the dead spend's inputs for good */
  isSpent?: (txid: string, vout: number) => boolean
  /** enumerate spent-marked entries — the starve sweep proves orphaned
   *  marks unspent and revives them. Without it a tombstoned outpoint whose
   *  consuming draft left no candidate starves the pool for good */
  spentList?: () => Array<{ tx: Transaction; vout: number; satoshis?: number }>
  address?: () => Promise<string> | string
}

/** Adapt a funder-shaped object into Funds. A carrier's own `.funds` wins;
 *  a utxos-shaped funder becomes a FundsStore view (live `utxos` getter,
 *  markSpent=drop, addOwned=add, unmark=revive) under one MemoryFunds — the
 *  shared state machine still owns selection and reservations. The owned
 *  lock comes from a synchronous `address()` or `deps.lock`/`pkhHex` —
 *  without it change could never be recognized as ours. */
export function fundsOf(f: FunderLike, depsOrFloor?: Partial<FundsDeps> | number): Funds {
  if (f.funds !== undefined) return f.funds
  // already the owner — a bare Funds passes through untouched (the
  // lifecycle verbs distinguish it from a funder-shaped object) */
  const maybeFunds = f as Partial<Funds>
  if (typeof maybeFunds.spent === 'function' && typeof maybeFunds.died === 'function') return maybeFunds as Funds
  const deps: Partial<FundsDeps> = typeof depsOrFloor === 'number' ? { dealFloor: depsOrFloor } : (depsOrFloor ?? {})
  if (f.utxos === undefined) throw new Error('fundsOf: funder carries no Funds owner and no utxos view')
  const addr = f.address?.()
  const lock =
    deps.lock ??
    (deps.pkhHex !== undefined ? `76a914${deps.pkhHex}88ac` : undefined) ??
    (addr !== undefined && !(addr instanceof Promise) ? new P2PKH().lock(addr).toHex() : undefined)
  if (lock === undefined) throw new Error('fundsOf: no owned lock — pass deps.lock/deps.pkhHex or a synchronous address()')
  const store: FundsStore = {
    list: () => (f.utxos ?? []).map((u) => ({ tx: u.tx, vout: u.vout, satoshis: u.satoshis ?? 0 })),
    add: (tx, vout) => f.addOwned?.(tx, vout),
    drop: (txid, vout) => f.markSpent?.(txid, vout),
    revive: (tx, vout) => {
      const txid = tx.id('hex')
      if (f.unmark !== undefined) f.unmark(txid, vout)
      else f.addOwned?.(tx, vout)
    },
    tombstoned: f.isSpent !== undefined ? (txid, vout) => f.isSpent!(txid, vout) : undefined,
    tombstonedList:
      f.spentList !== undefined ? () => f.spentList!().map((u) => ({ txid: u.tx.id('hex'), vout: u.vout, satoshis: u.satoshis, tx: u.tx })) : undefined
  }
  return new AdaptedFunds(store, { ...deps, lock })
}
