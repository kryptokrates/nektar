// txfacts.ts — the one answer about a transaction. Every "what happened to
// this tx" question resolves to a TxFacts: bytes held, proof held, depth, a
// verdict (mined | pending | dead) with its source, and separately the
// outside status word as a HINT. The verdict is computed before the hint is
// ever asked — a broadcaster status string can wake a reconcile but can
// never become the verdict's basis (spv-strict: hints are not proofs).
//
// Verdict order is deliberately monotonic:
//   1. proof in hand           → mined / 'proof'   (outranks every record)
//   2. first-party death       → dead  / 'first-party' (our reject flag, a
//      backend verdict, or the durable ledger's own terminal row)
//   3. abandon horizon         → dead  / 'abandon' (the chain observer's
//      computed terminal for a watched tx that never proved)
//   4. otherwise               → pending (the hint field still says what the
//      outside word was — callers may treat it as 'seen', never as truth)
import type { MerklePath, Transaction } from '@bsv/sdk'

/** the outside word for a txid — a hint, never a verdict */
export interface StatusHint {
  /** the broadcaster's current status word (uppercase; '' = undecidable) */
  status: string
  /** some source gave a definitive reply — a verdict, or a clean 'not
   *  carried'. 'Could not ask' is not 'not on the network'. */
  answered: boolean
}

export type TxVerdict = 'mined' | 'pending' | 'dead'
export type TxVerdictSource = 'proof' | 'first-party' | 'abandon'

export interface TxFacts {
  /** bytes held by the chain view (a local prepared-but-unbroadcast copy in
   *  the tx store is NOT 'held' — callers wanting bytes pull fetchTx) */
  tx?: Transaction
  /** the verified merkle path, when one exists anywhere we hold */
  proof?: MerklePath
  /** confirmations deep — only when proven AND a height source is wired */
  depth?: number
  verdict: TxVerdict
  source?: TxVerdictSource
  /** the outside status word, fetched only while the verdict is pending —
   *  absent when a terminal verdict made the question moot or no hint
   *  source exists (memory tier) */
  hint?: StatusHint
}

export interface TxFactsDeps {
  /** bytes the chain view holds — anchor set and backend answers; never the
   *  bare tx store (a prepared-but-unbroadcast tx is not 'seen') */
  held?: (txid: string) => Transaction | undefined | Promise<Transaction | undefined>
  /** a proof held anywhere outside held()'s tx (e.g. the tx store's copy) */
  proof?: (txid: string) => MerklePath | undefined | Promise<MerklePath | undefined>
  /** first-party death: our own reject delivery, a backend verdict, or the
   *  durable ledger's terminal row — never fed by an outside status word */
  rejected?: (txid: string) => boolean | Promise<boolean>
  /** the chain observer's abandon horizon fired for this txid */
  abandoned?: (txid: string) => boolean | Promise<boolean>
  /** the outside status word — the broadcaster/WoC answer, absent on tiers
   *  with no outside to ask */
  hint?: (txid: string) => Promise<StatusHint>
  /** chain height for depth — absent leaves depth unset */
  height?: () => number | Promise<number>
}

export function txFactsOf(deps: TxFactsDeps): (txid: string) => Promise<TxFacts> {
  return async (txid) => {
    const tx = await deps.held?.(txid)
    const proof = tx?.merklePath ?? (await deps.proof?.(txid))
    let verdict: TxVerdict = 'pending'
    let source: TxVerdictSource | undefined
    if (proof !== undefined) {
      verdict = 'mined'
      source = 'proof'
    } else if ((await deps.rejected?.(txid)) === true) {
      verdict = 'dead'
      source = 'first-party'
    } else if ((await deps.abandoned?.(txid)) === true) {
      verdict = 'dead'
      source = 'abandon'
    }
    const depth = proof !== undefined && deps.height !== undefined ? Math.max(0, (await deps.height()) - proof.blockHeight + 1) : undefined
    // sealed before the hint is asked: the outside word can inform a caller
    // but can never travel back into the verdict above
    const hint = verdict === 'pending' && deps.hint !== undefined ? await deps.hint(txid).catch(() => ({ status: '', answered: false })) : undefined
    return { tx, proof, depth, verdict, source, hint }
  }
}

/** the mined-word pair — a proof claim, still hint-tier until a BUMP verifies */
export const MINED_WORDS = ['MINED', 'IMMUTABLE']

/** the rejection-word pair — a refusal claim; a double-spend is provable only
 *  via the competing tx's own BUMP */
export const REJECTED_WORDS = ['REJECTED', 'DOUBLE_SPEND_ATTEMPTED']

/** status words meaning "the network carries it" — a MINED/IMMUTABLE word is
 *  a strong hint but earns no 'mined' verdict without a proof object */
export const SEEN_HINTS = [
  'RECEIVED',
  'SENT_TO_NETWORK',
  'ACCEPTED_BY_NETWORK',
  'SEEN_ON_NETWORK',
  'SEEN_MULTIPLE_NODES',
  'STUMP_PROCESSING',
  'MINED',
  'IMMUTABLE'
]

/** every broadcaster status word we know — push-callback normalization set;
 *  unknown words collapse to OTHER rather than being trusted verbatim */
export const KNOWN_STATUSES = [
  ...SEEN_HINTS,
  'QUEUED',
  'STORED',
  'ANNOUNCED_TO_NETWORK',
  'SEEN_IN_ORPHAN_MEMPOOL',
  'CONFIRMED',
  ...REJECTED_WORDS
]

/** an answered seen-family status — the tx is observable somewhere */
export const hintSeen = (h?: StatusHint): boolean => h !== undefined && h.answered && SEEN_HINTS.includes(h.status)

/** an answered rejection-family status — a wake to reap, still not a verdict */
export const hintRejected = (h?: StatusHint): boolean => h !== undefined && h.answered && (h.status === 'REJECTED' || h.status.startsWith('DOUBLE_SPEND'))

/** nobody outside could tell us anything — undecidable, unanswered, or no
 *  hint source at all (the silent-eviction case a rejection never covers) */
export const hintSilent = (h?: StatusHint): boolean => h === undefined || !h.answered || h.status === '' || h.status === 'UNKNOWN'
