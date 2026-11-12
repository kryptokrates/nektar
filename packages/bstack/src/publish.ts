import { sha256, toHex, type SignerLike } from '@nektar/core'
import { fundingLock, type SigningWallet } from '@lightwebinc/bcommon'
import { buildCarrier, carrierFundingLockHex, walletOrProto } from './carrier.js'
import { encodePayload, lanesFor, bodyDomains, type MarketPayload } from './market.js'
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
    await p.journal.put(hash, { txid, beef, topics })
  }
  // the journal re-serves missed hosts — no sleeps inside the send path
  const r = await submitAll(p.hosts, beef, topics, { quorum: p.quorum, retryWaits: [] })
  if (!r.ok) throw new Error(`publishArtifact: quorum failed ${JSON.stringify(r.missed)}`)
  return { txid: txid, took: r.took, admitted: r.admitted, missed: r.missed }
}
