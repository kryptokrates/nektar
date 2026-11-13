import { sha256, toHex, type SignerLike } from '@nektar/core'
import { fundingLock, type SigningWallet } from '@lightwebinc/bcommon'
import { Transaction } from '@bsv/sdk'
import { buildCarrier, carrierFundingLockHex, walletOrProto } from './carrier.js'
import { encodePayload, lanesFor, bodyDomains, type MarketPayload } from './market-wire.js'
import { submitAll, type Quorum } from './hosts.js'
import { DERIVATION, FUNDING_TAG, ORIGINATOR, type TreeLedger } from './tree.js'
import type { CarrierJournal } from './journal.js'

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
}): Promise<{ txid: string; took: string[]; admitted: string[]; missed: Array<{ host: string; error: string }> }> {
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
    if (slot === undefined) throw new Error('no funding slot')
    const c = await buildCarrier({ key: p.key, wallet: p.wallet, payload: encodePayload(p.payload), slot })
    beef = c.beef
    txid = c.carrier.id('hex')
    await p.journal.put(hash, { txid, beef, topics, fundingTxid: slot.tree.id('hex') })
  }
  // the journal re-serves missed hosts — no sleeps inside the send path
  const r = await submitAll(p.hosts, beef, topics, { quorum: p.quorum, retryWaits: [] })
  if (!r.ok) throw new Error(`publishArtifact: quorum failed ${JSON.stringify(r.missed)}`)
  return { txid: txid, took: r.took, admitted: r.admitted, missed: r.missed }
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
    try {
      const tx = Transaction.fromAtomicBEEF(row.beef)
      const parent = tx.inputs[0]?.sourceTransaction
      if (parent === undefined || parent.id('hex') !== p.treeTxid || parent.merklePath !== undefined) continue
      parent.merklePath = p.merklePath
      const beef = tx.toAtomicBEEF()
      const r = await submitAll(p.hosts, beef, row.topics, { retryWaits: [] })
      // persist the enriched beef only once quorum is reached — a row left
      // unenriched retries on the next pass instead of stranding a missed
      // host on its stale deadline-bound copy
      if (r.ok) {
        await p.journal.put(row.hash, { txid: row.txid, beef, topics: row.topics, fundingTxid: p.treeTxid })
        republished++
      }
      missed.push(...r.missed)
    } catch {
      /* a row that can't be enriched or re-sent retries on the next pass */
    }
  }
  return { republished, missed }
}
