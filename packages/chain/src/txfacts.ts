// txfacts.ts — THE answer about a transaction. Every "is it mined / alive /
// dead" question in the codebase resolves through txFacts: the bytes we
// hold, the proof we hold, confirmations depth, and a three-way verdict with
// its provenance ('proof' | 'first-party' | 'abandon'). The outside world's
// status word is returned separately as `hint` — it may wake a reconcile or
// shorten a poll, but it NEVER lands in the verdict: a hint carries no
// proof object and no first-party record.
import type { Transaction, MerklePath } from '@bsv/sdk'

export type TxVerdict = 'mined' | 'pending' | 'dead'
export type TxVerdictFrom = 'proof' | 'first-party' | 'abandon'

/** an outside status read: `status` is the adapter's uppercase status word
 *  ('' when undecidable); `answered` distinguishes "asked and told" from
 *  "couldn't ask" — only an answered read may age an abandon clock. */
export interface TxHint {
  status: string
  answered: boolean
}

export interface TxFacts {
  /** the tx bytes, when our graph/backend holds them */
  tx?: Transaction
  /** the merkle proof, when held — the only self-verifying evidence */
  proof?: MerklePath
  /** confirmations depth (tip - blockHeight + 1) when proof + tip are known */
  depth?: number
  verdict: TxVerdict
  /** which class of evidence produced a terminal verdict — undefined on
   *  'pending' (there is no evidence either way yet) */
  verdictFrom?: TxVerdictFrom
  /** the outside world's status word — advisory only, never the verdict */
  hint?: TxHint
}

export interface TxFactsDeps {
  /** our own view: graph bytes (which may already carry the proof) */
  txOf?: (txid: string) => Promise<Transaction | undefined> | Transaction | undefined
  /** a proof we hold elsewhere than on the tx object (the tx store) */
  proofOf?: (txid: string) => Promise<MerklePath | undefined> | MerklePath | undefined
  /** first-party dead verdict: a verified chain event, a definitive
   *  broadcast refusal, the backend's own isRejected, or our ledger's
   *  terminal (rejected/conflicted) row */
  rejected?: (txid: string) => Promise<boolean> | boolean
  /** the observer-computed abandon horizon fired for this txid */
  abandoned?: (txid: string) => Promise<boolean> | boolean
  /** the outside status read — the adapter's private probe fills this;
   *  never consulted for the verdict */
  hintOf?: (txid: string) => Promise<TxHint | string | undefined>
  /** confirmations depth for a held proof — needs the caller's tip */
  depthOf?: (proof: MerklePath) => Promise<number | undefined> | number | undefined
}

const normHint = (h: TxHint | string | undefined): TxHint | undefined =>
  h === undefined ? undefined : typeof h === 'string' ? { status: h, answered: true } : h

/** The single status answer. Verdict precedence is evidence-ranked:
 *  a held merkle proof is 'mined' (outranks every recorded verdict — a
 *  rejected mark that later proves still counts mined); a first-party
 *  dead record is 'dead'; the abandon horizon is 'dead'; otherwise
 *  'pending'. The hint is fetched independently and never feeds it. */
export async function txFacts(txid: string, deps: TxFactsDeps): Promise<TxFacts> {
  const [tx, rawHint] = await Promise.all([Promise.resolve(deps.txOf?.(txid)), (deps.hintOf?.(txid) ?? Promise.resolve(undefined)).catch(() => undefined)])
  const hint = normHint(rawHint)
  const proof = tx?.merklePath ?? (await deps.proofOf?.(txid))
  if (proof !== undefined) {
    const depth = await deps.depthOf?.(proof)
    return { tx, proof, depth: depth ?? undefined, verdict: 'mined', verdictFrom: 'proof', hint }
  }
  if ((await deps.rejected?.(txid)) === true) return { tx, verdict: 'dead', verdictFrom: 'first-party', hint }
  if ((await deps.abandoned?.(txid)) === true) return { tx, verdict: 'dead', verdictFrom: 'abandon', hint }
  return { tx, verdict: 'pending', hint }
}
