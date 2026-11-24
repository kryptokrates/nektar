// Transaction-graph walks — pure traversals over sourceTransaction links.
import type { Transaction, TransactionInput } from '@bsv/sdk'
import { sparse } from './sparse.js'

export function isEfStub(tx: Transaction): boolean {
  if (tx.inputs.length === 0 && tx.outputs.some((o: unknown) => o === null)) return true
  try {
    tx.id('hex')
    return false
  } catch {
    return true
  }
}

export function fullSourceTx(inp: TransactionInput): Transaction | undefined {
  const src = inp.sourceTransaction
  if (src === undefined || isEfStub(src)) return undefined
  if (inp.sourceTXID !== undefined && src.id('hex') !== inp.sourceTXID) return undefined
  return src
}

export function inputPrevout(inp: TransactionInput): { txid: string; vout: number } | undefined {
  const txid = inp.sourceTXID ?? fullSourceTx(inp)?.id('hex')
  const vout = sparse(inp.sourceOutputIndex)
  return txid !== undefined && vout !== undefined ? { txid, vout } : undefined
}

/** every ancestor of tx in dependency order (parents before children) via
 *  sourceTransaction links — the order a missing-ancestors broadcast heal
 *  must use so each parent lands before its child. */
export function ancestorsOf(tx: Transaction): Transaction[] {
  const seen = new Set<string>()
  const out: Transaction[] = []
  const walk = (t: Transaction): void => {
    for (const inp of t.inputs) {
      const src = fullSourceTx(inp)
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
