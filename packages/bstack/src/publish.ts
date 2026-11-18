import { sha256, toHex, type SignerLike } from '@nektar/core'
import { fundingLock, type SigningWallet } from '@lightwebinc/bcommon'
import { Transaction } from '@bsv/sdk'
import { buildCarrier, carrierFundingLockHex, walletOrProto } from './carrier.js'
import { encodePayload, lanesFor, bodyDomains, type MarketPayload } from './market-wire.js'
import { submitAll, type Quorum } from './hosts.js'
import { DERIVATION, FUNDING_TAG, ORIGINATOR, fundingSlotReason, type TreeLedger } from './tree.js'
import { parseBeef, type CarrierJournal } from './journal.js'

export function artifactHash(payload: MarketPayload): string {
  return toHex(Uint8Array.from(sha256(encodePayload(payload))))
}

export async function publishArtifact(p: {
  key: SignerLike
  wallet?: SigningWallet
  ledger: TreeLedger
  journal: CarrierJournal
  hosts: string[]
  net: string
  payload: MarketPayload
  quorum?: Quorum
  /** fired the moment the signed carrier exists (journaled, before the host
   *  fanout) — direct-push to a present peer overlaps the submitAll RTT */
  push?: (beef: number[]) => void
}): Promise<{
  txid: string
  hash: string
  took: string[]
  admitted: string[]
  missed: Array<{ host: string; error: string }>
  pending: string[]
  settled: Promise<{ took: string[]; missed: Array<{ host: string; error: string }> }>
}> {
  const hash = artifactHash(p.payload)
  const topics = lanesFor(p.net, bodyDomains(p.payload))
  const lockHex = (await fundingLock(walletOrProto(p.key, p.wallet), ORIGINATOR, DERIVATION, FUNDING_TAG)).toHex()
  let beef: number[] | undefined
  let txid: string | undefined
  let held = await p.journal.get(hash)
  // a carrier journaled under a rotated identity can never evaluate — drop it
  if (held !== undefined && carrierFundingLockHex(held.beef) !== lockHex && p.journal.del !== undefined) {
    await p.journal.del(hash)
    held = undefined
  }
  if (held !== undefined) {
    beef = held.beef
    txid = held.txid
  } else {
    const slot = await p.ledger.take('market', lockHex)
    if (slot === undefined) throw new Error(await fundingSlotReason(p.ledger, 'market'))
    const c = await buildCarrier({ key: p.key, wallet: p.wallet, payload: encodePayload(p.payload), slot })
    beef = c.beef
    txid = c.carrier.id('hex')
    await p.journal.put(hash, { txid, beef, topics, fundingTxid: slot.tree.id('hex') })
  }
  // the journal re-serves missed hosts — no sleeps inside the send path, and
  // resolve at quorum: a straggler host leg completes in the background and
  // the journal still carries it, so one slow host can't price every send at
  // its own 15s timeout (observed: offers pending until the want expired)
  p.push?.(beef)
  const r = await submitAll(p.hosts, beef, topics, { quorum: p.quorum, retryWaits: [], early: true })
  if (!r.ok) throw new Error(`publishArtifact: quorum failed ${JSON.stringify(r.missed)}`)
  return { txid, hash, took: r.took, admitted: r.admitted, missed: r.missed, pending: r.pending, settled: r.settled }
}

/** re-serve a journaled carrier to ONE host — the repair leg for a fan-out
 *  that resolved at quorum while this host missed (or was still pending).
 *  A missing journal row resolves: nothing left to repair. */
export async function resubmitCarrier(p: { journal: CarrierJournal; host: string; key: string }): Promise<void> {
  const row = await p.journal.get(p.key)
  if (row === undefined) return
  const r = await submitAll([p.host], row.beef, row.topics, { quorum: 'all', retryWaits: [] })
  if (!r.ok) throw new Error(`resubmit ${p.host}: ${r.missed[0].error}`)
}

/** one journaled carrier whose funding tree just proved: attach the parent's
 *  BUMP (same txid), re-submit, and persist the enriched beef once quorum is
 *  reached — the per-row body of republishProved so a single hash can be
 *  queued and retried on its own. `done` is also true when the row is gone
 *  or its parent already carries a path (nothing to repair). `submitted`
 *  distinguishes a real re-announce from those no-op cases. */
export async function republishOne(p: {
  journal: CarrierJournal
  hosts: string[]
  hash: string
  treeTxid: string
  merklePath: import('@bsv/sdk').MerklePath
}): Promise<{ done: boolean; submitted?: boolean; missed: Array<{ host: string; error: string }> }> {
  const row = await p.journal.get(p.hash)
  if (row === undefined) return { done: true, missed: [] }
  try {
    // log-anchor rows journal plain beef — fromAtomicBEEF dead-letters them
    const tx = parseBeef(row.beef)
    if (tx === undefined) return { done: false, missed: [] }
    const parent = tx.inputs.find((i) => i.sourceTransaction?.id('hex') === p.treeTxid)?.sourceTransaction
    if (parent === undefined || parent.merklePath !== undefined) return { done: true, missed: [] }
    parent.merklePath = p.merklePath
    const beef = tx.toAtomicBEEF()
    const r = await submitAll(p.hosts, beef, row.topics, { retryWaits: [] })
    // persist the enriched beef only once quorum is reached — a row left
    // unenriched retries on the next pass instead of stranding a missed host
    // on its stale deadline-bound copy
    if (r.ok) await p.journal.put(p.hash, { txid: row.txid, beef, topics: row.topics, fundingTxid: row.fundingTxid ?? p.treeTxid })
    return { done: r.ok, submitted: r.ok, missed: r.missed }
  } catch {
    return { done: false, missed: [] }
  }
}

/** after a funding tree's own BUMP lands, republish its journaled carriers
 *  with the parent's proof attached — the same txid, so hosts re-record the
 *  entry free of the unproven-funding eviction deadline (the FUNDING_PROOF_MS
 *  row module.ts stamps at admission and never clears). Rows whose stored
 *  beef already carries the parent's path are skipped; a submit miss retries
 *  on the next call since the journal write keeps the row visible. */
export async function republishProved(p: {
  journal: CarrierJournal
  hosts: string[]
  treeTxid: string
  merklePath: import('@bsv/sdk').MerklePath
}): Promise<{ republished: number; missed: Array<{ host: string; error: string }> }> {
  if (p.journal.fundedBy === undefined) return { republished: 0, missed: [] }
  let republished = 0
  const missed: Array<{ host: string; error: string }> = []
  for (const row of await p.journal.fundedBy(p.treeTxid)) {
    const r = await republishOne({ journal: p.journal, hosts: p.hosts, hash: row.hash, treeTxid: p.treeTxid, merklePath: p.merklePath })
    if (r.submitted === true) republished++
    missed.push(...r.missed)
  }
  return { republished, missed }
}
