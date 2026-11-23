// Transaction-graph walks — pure traversals over sourceTransaction links.
import { Transaction } from '@bsv/sdk'

/** every ancestor of tx in dependency order (parents before children) via
 *  sourceTransaction links — the order a missing-ancestors broadcast heal
 *  must use so each parent lands before its child. */
export function ancestorsOf(tx: Transaction): Transaction[] {
  const seen = new Set<string>()
  const out: Transaction[] = []
  const walk = (t: Transaction): void => {
    for (const inp of t.inputs) {
      const src = inp.sourceTransaction
      if (src === undefined) continue
      const id = src.id('hex')
      if (seen.has(id)) continue
      seen.add(id)
      walk(src)
      out.push(src)
    }
  }
  walk(tx)
  return out
}

/** the 'missing inputs' heal, one copy: a parent ran ahead of the network
 *  (evicted or never landed) — re-broadcast the tx's unproven ancestors
 *  oldest-first so each parent lands before its child, then let the caller
 *  retry once. Best-effort: individual ancestor failures are swallowed, the
 *  retried broadcast is the verdict. */
export async function rebroadcastAncestors(p: { broadcast: (tx: Transaction) => Promise<unknown> }, tx: Transaction): Promise<void> {
  try {
    for (const anc of ancestorsOf(tx)) {
      if (anc.merklePath === undefined) await p.broadcast(anc).catch(() => undefined)
    }
  } catch {
    /* heal is best-effort — the retried broadcast decides */
  }
}
