import { PrivateKey, Transaction, type MerklePath } from '@bsv/sdk'
import { mintTree, type TreeLedger } from './tree.js'
import type { FundingOut, BroadcastResult } from '@nektar/chain'

export async function ensureTrees (p: {
  key: PrivateKey, ledger: TreeLedger, lowWater?: number, size?: number, satsPerOutput?: number,
  takeFunding: (sats: number) => Promise<FundingOut>,
  broadcast: (tx: Transaction) => Promise<BroadcastResult>,
  fetchTx: (txid: string) => Promise<Transaction | undefined>,
  onSpent?: (tx: Transaction) => void | Promise<void>
}): Promise<{ minted?: string, proven: string[], remaining: number }> {
  const lowWater = p.lowWater ?? 4
  const size = p.size ?? 16
  const satsPerOutput = p.satsPerOutput ?? 1
  const proven: string[] = []
  for (const txid of await p.ledger.unproven()) {
    const tx = await p.fetchTx(txid)
    if (tx !== undefined && tx.merklePath !== undefined) { await p.ledger.markProven(txid, tx.merklePath); proven.push(txid) }
  }
  const remaining = await p.ledger.remaining()
  let minted: string | undefined
  if (remaining < lowWater && (await p.ledger.unproven()).length === 0) {
    const funding = await p.takeFunding(size * satsPerOutput + 2000)
    const tree = await mintTree({ key: p.key, funding, n: size, satsPerOutput })
    const r = await p.broadcast(tree)
    if (r.status !== 'success') throw new Error(`ensureTrees: broadcast failed ${JSON.stringify(r).slice(0, 120)}`)
    await p.ledger.add(tree, size)
    minted = tree.id('hex')
    if (p.onSpent !== undefined) await p.onSpent(tree)
  }
  return { minted, proven, remaining: await p.ledger.remaining() }
}
