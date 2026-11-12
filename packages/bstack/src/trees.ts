import { PrivateKey, Transaction } from '@bsv/sdk'
import { mintTree, type TreeLedger } from './tree.js'
import type { SigningWallet } from '@lightwebinc/bcommon'
import type { FundingOut, BroadcastResult } from '@nektar/chain'

/** outputs per funding tree unless the caller sizes it */
export const TREE_SIZE = 64

export async function ensureTrees(p: {
  key: PrivateKey
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
  onSpent?: (tx: Transaction) => void | Promise<void>
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
    }
  }
  const remaining = await p.ledger.remaining(kind)
  let minted: string | undefined
  if (remaining < lowWater && (await p.ledger.unproven()).filter((u) => u.kind === kind).length === 0) {
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
