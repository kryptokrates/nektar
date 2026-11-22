import { Transaction } from '@bsv/sdk'
import { mintTree, PROVISION_KIND, type TreeLedger } from './tree.js'
import type { SigningWallet } from '@lightwebinc/bcommon'
import type { SignerLike } from '@nektar/core'
import { hintRejected, hintSilent, FundsStarvedError, type FundingOut, type BroadcastResult } from '@nektar/chain'

/** outputs per funding tree unless the caller sizes it */
export const TREE_SIZE = 64
/** minimum change reserve each mint take must leave behind — not spent, the
 *  take's utxo-selection floor so the mint's change stays a viable next take.
 *  Lower for small treasuries; below ~fee+dust the change strands. */
const TREE_RESERVE_SATS = 2000

export async function ensureTrees(p: {
  key: SignerLike
  wallet?: SigningWallet
  ledger: TreeLedger
  lowWater?: number
  size?: number
  satsPerOutput?: number
  kind?: string
  lock?: import('@bsv/sdk').LockingScript
  takeFunding: (sats: number) => Promise<FundingOut>
  broadcast: (tx: Transaction) => Promise<BroadcastResult>
  fetchTx: (txid: string) => Promise<Transaction | undefined>
  /** the one transaction-facts answer — a dead verdict OR a rejected-family
   *  hint drops an unproven mint so the kind's `unproven === 0` mint gate
   *  re-opens; a silent hint routes to onStuck for heal/reap */
  facts?: (txid: string) => Promise<import('@nektar/chain').TxFacts>
  /** fired when an unproven tree is dropped as chain-rejected — lets the
   *  caller mark its ghost inputs/outputs spent so funding never re-selects
   *  them (the change output would otherwise cascade parent-rejected mints) */
  onReject?: (tx: Transaction) => void | Promise<void>
  /** a mint refused at broadcast — the tx never books a ledger row, so its
   *  consumed funding needs the caller's heal before the pass fails: a
   *  definitive refusal releases the reservation and quarantines the
   *  poisoned input; a maybe-aired (transport) failure must not. Without
   *  this the refused mint leaks its take reservation forever. */
  onRefused?: (tx: Transaction, result: BroadcastResult) => void | Promise<void>
  /** fired for an unproven mint the chain observer cannot see ('unknown' /
   *  undecidable status — the silent-eviction case a rejection verdict never
   *  covers): 'keep' leaves the row, 'drop' reaps it like a rejection. The
   *  caller heals inside (resubmit) and answers 'drop' only on a definitive
   *  failure — a row left here gates every later mint of its kind. */
  onStuck?: (tx: Transaction) => Promise<'keep' | 'drop'>
  onSpent?: (tx: Transaction) => void | Promise<void>
  /** max unproven mints in flight per kind (default 1) — deeper than one
   *  keeps a refill minting while an earlier mint still waits on its block,
   *  so the proof wait never gates the next refill */
  maxUnproven?: number
  /** change outputs each mint fans its remainder across (default 4) — later
   *  spends draw distinct vouts off the SAME broadcast mint, keeping their
   *  unconfirmed ancestry one hop deep instead of chaining mint-to-mint */
  changeFanout?: number
  /** extra sats the take's utxo must cover beyond the tree outputs — the
   *  change the mint leaves for the NEXT take, not a cost. Small treasuries
   *  lower it (NEKTAR_TREE_RESERVE_SATS); below ~fee+dust the change strands */
  reserveSats?: number
}): Promise<{ minted: string[]; proven: string[]; remaining: number }> {
  // Hosts admit a carrier only from a funding tree that is proven in a block,
  // so the stock is what keeps publishing free of block times: the next tree
  // is minted when half of the current one is still unused and has a whole
  // tree's worth of traffic to get mined in.
  const size = p.size ?? TREE_SIZE
  const lowWater = p.lowWater ?? Math.ceil(size / 2)
  const satsPerOutput = p.satsPerOutput ?? 1
  const kind = p.kind ?? 'market'
  const proven: string[] = []
  for (const u of await p.ledger.unproven()) {
    // a provision row matches every kind's proving pass — it carries slots
    // for several lanes, so whichever upkeep runs first may attach its path
    if (u.kind !== kind && u.kind !== PROVISION_KIND) continue
    const tx = await p.fetchTx(u.txid)
    if (tx !== undefined && tx.merklePath !== undefined) {
      await p.ledger.markProven(u.txid, tx.merklePath)
      proven.push(u.txid)
    } else if (p.facts !== undefined) {
      const f = await p.facts(u.txid)
      if (f.verdict === 'dead') {
        // a sealed dead verdict — the mint provably lost a competing spend;
        // an unproven row would gate out every later mint of this kind
        // forever. A provision row is reaped the same way, but never
        // tombstones: its inputs are the payer's, not ours. A REJECTED hint
        // is NOT this — it routes to the heal arm below.
        await p.ledger.drop(u.txid)
        if (u.kind !== PROVISION_KIND && p.onReject !== undefined && tx !== undefined) await p.onReject(tx)
      } else if (u.kind !== PROVISION_KIND && (hintSilent(f.hint) || hintRejected(f.hint)) && p.onStuck !== undefined && tx !== undefined) {
        // silently vanished or refused on a status word — resubmit or reap.
        // The heal path decides on first-party evidence; a pushed hint alone
        // never drops a row or writes a tombstone
        if ((await p.onStuck(tx)) === 'drop') {
          await p.ledger.drop(u.txid)
          if (p.onReject !== undefined) await p.onReject(tx)
        }
      }
    }
  }
  // fill to target, not one per pass: a burst that just drained stock needs
  // refill depth now (proven-only lanes have no anchored stock to dip into),
  // and `maxUnproven` is the in-flight bound that keeps a stuck block from
  // minting unboundedly. `remaining` counts anchored stock on the market
  // lane, so a fresh anchored mint may already satisfy the target.
  const minted: string[] = []
  for (;;) {
    const unprovenKind = (await p.ledger.unproven()).filter((u) => u.kind === kind).length
    if ((await p.ledger.remaining(kind)) >= lowWater || unprovenKind >= (p.maxUnproven ?? 1)) break
    const reserve = p.reserveSats ?? TREE_RESERVE_SATS
    let funding: FundingOut
    try {
      funding = await p.takeFunding(size * satsPerOutput + reserve)
    } catch (e) {
      if (e instanceof FundsStarvedError) throw new Error(`${e.message} — the mint reserve is ${reserve} sats; lower NEKTAR_TREE_RESERVE_SATS or fund more`)
      throw e
    }
    const tree = await mintTree({ key: p.key, wallet: p.wallet, funding, n: size, satsPerOutput, lock: p.lock, changeOutputs: p.changeFanout ?? 4 })
    // a funder re-issuing the same UTXO mints the identical tree — the
    // ledger's idempotent add would leave unprovenKind flat and the loop
    // would never reach its bound; treat a repeat as the bound itself
    if (minted.includes(tree.id('hex'))) break
    const r = await p.broadcast(tree)
    if (r.status !== 'success') {
      if (p.onRefused !== undefined) await p.onRefused(tree, r)
      throw new Error(`ensureTrees: broadcast failed ${JSON.stringify(r).slice(0, 120)}`)
    }
    await p.ledger.add(tree, size, kind)
    minted.push(tree.id('hex'))
    if (p.onSpent !== undefined) await p.onSpent(tree)
  }
  return { minted, proven, remaining: await p.ledger.remaining(kind) }
}
