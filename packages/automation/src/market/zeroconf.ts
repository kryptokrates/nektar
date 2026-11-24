// zeroconf.ts — the seller's delivery policy for an unproven payment.
//
// Verified ancestry proves where a payment CAME from, not that it will
// confirm: a buyer can hand over a perfectly-valid signed tx and then
// double-spend its parents. Shipping the disclosure while the payment is
// unproven is the seller's no-recourse exposure — the content is gone
// whether the payment lands or not. The policy: a small cap and an
// ancestor bound cover the honest common case; anything larger or deeper
// parks in payment_pending until the payment's own proof arrives.
import type { Transaction } from '@bsv/sdk'
import { fullSourceTx, inputPrevout } from '@nektar/core'

export type ZeroconfVerdict = 'proven' | 'zeroconf' | 'await-proof'

/** the delivery verdict for an accepted payment. `proven` means the payment
 *  tx ITSELF carries a merkle path — not the wire's spv flag, which is also
 *  true for an unmined tx whose parents are proven. */
export function zeroconfVerdict(a: {
  proven: boolean
  priceSats: number
  unconfirmedAncestors: number
  opts: { maxZeroconfSats: number; maxUnconfirmedAncestors: number }
}): ZeroconfVerdict {
  if (a.proven) return 'proven'
  if (a.priceSats <= a.opts.maxZeroconfSats && a.unconfirmedAncestors <= a.opts.maxUnconfirmedAncestors) return 'zeroconf'
  return 'await-proof'
}

/** distinct UNPROVEN ancestor txs behind `tx` (tx itself excluded):
 *  breadth-first over inputs; a carried or resolved parent with a merkle
 *  path ends its branch; a parent that cannot be resolved counts as one
 *  unproven ancestor and ends its branch. Stops as soon as the count
 *  exceeds `cap` so the walk is bounded — a level with many unproven
 *  inputs can overshoot cap + 1, but the verdict only asks `<= cap` and
 *  deeper ancestry only changes a log line, never the verdict. */
export async function unconfirmedAncestors(tx: Transaction, resolve: (txid: string) => Promise<Transaction | undefined>, cap: number): Promise<number> {
  const seen = new Set<string>()
  let count = 0
  const queue: Transaction[] = [tx]
  while (queue.length > 0 && count <= cap) {
    const cur = queue.shift()!
    for (const i of cur.inputs) {
      const id = inputPrevout(i)?.txid
      // a null prevout (coinbase-shaped input) is a structural terminal, not
      // an unproven ancestor — there is no parent to count
      if (id === undefined || id === '00'.repeat(32) || seen.has(id)) continue
      seen.add(id)
      // the carried parent wins — a resolved copy can never be LESS proven
      // than the wire that already survived TxStore.read. A stand-in
      // carries no ancestry, so resolve the real parent
      const p = fullSourceTx(i) ?? (await resolve(id))
      if (p === undefined) {
        count++
        continue
      }
      if (p.merklePath !== undefined) continue
      count++
      queue.push(p)
    }
  }
  return count
}
