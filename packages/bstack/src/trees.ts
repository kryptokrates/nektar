import { Transaction } from '@bsv/sdk'
import { mintTree, type TreeLedger } from './tree.js'
import type { SigningWallet } from '@lightwebinc/bcommon'
import type { SignerLike } from '@nektar/core'
import type { FundingOut, BroadcastResult } from '@nektar/chain'

/** outputs per funding tree unless the caller sizes it */
export const TREE_SIZE = 64

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
  /** chain-observer verdict for a txid (broadcaster/overlay status) — when
   *  given, an unproven mint whose status is a definitive rejection is
   *  dropped so the kind's `unproven === 0` mint gate re-opens */
  txStatus?: (txid: string) => Promise<string>
  /** fired when an unproven tree is dropped as chain-rejected — lets the
   *  caller mark its ghost inputs/outputs spent so funding never re-selects
   *  them (the change output would otherwise cascade parent-rejected mints) */
  onReject?: (tx: Transaction) => void | Promise<void>
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
}): Promise<{ minted?: string; proven: string[]; remaining: number }> {
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
    if (u.kind !== kind) continue
    const tx = await p.fetchTx(u.txid)
    if (tx !== undefined && tx.merklePath !== undefined) {
      await p.ledger.markProven(u.txid, tx.merklePath)
      proven.push(u.txid)
    } else if (p.txStatus !== undefined) {
      const st = (await p.txStatus(u.txid)).toUpperCase()
      if (st === 'REJECTED' || st.startsWith('DOUBLE_SPEND')) {
        // the mint was refused/lost a competing spend — an unproven row
        // would gate out every later mint of this kind forever
        await p.ledger.drop(u.txid)
        if (p.onReject !== undefined && tx !== undefined) await p.onReject(tx)
      } else if ((st === '' || st === 'UNKNOWN') && p.onStuck !== undefined && tx !== undefined) {
        // silently vanished (never propagated, evicted) — resubmit or reap;
        // without this the mint gate stays shut past any restart
        if ((await p.onStuck(tx)) === 'drop') {
          await p.ledger.drop(u.txid)
          if (p.onReject !== undefined) await p.onReject(tx)
        }
      }
    }
  }
  const remaining = await p.ledger.remaining(kind)
  let minted: string | undefined
  if (remaining < lowWater && (await p.ledger.unproven()).filter((u) => u.kind === kind).length < (p.maxUnproven ?? 1)) {
    const funding = await p.takeFunding(size * satsPerOutput + 2000)
    const tree = await mintTree({ key: p.key, wallet: p.wallet, funding, n: size, satsPerOutput, lock: p.lock })
    const r = await p.broadcast(tree)
    if (r.status !== 'success') throw new Error(`ensureTrees: broadcast failed ${JSON.stringify(r).slice(0, 120)}`)
    await p.ledger.add(tree, size, kind)
    minted = tree.id('hex')
    if (p.onSpent !== undefined) await p.onSpent(tree)
  }
  return { minted, proven, remaining: await p.ledger.remaining(kind) }
}
