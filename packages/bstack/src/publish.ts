import { PrivateKey, ProtoWallet } from '@bsv/sdk'
import { sha256, toHex } from '@nektar/core'
import { fundingLock } from '@lightwebinc/bcommon'
import { buildCarrier } from './carrier.js'
import { encodePayload, lanesFor, bodyDomains, type MarketPayload } from './market.js'
import { submitAll, type Quorum } from './hosts.js'
import { DERIVATION, FUNDING_TAG, ORIGINATOR, type TreeLedger } from './tree.js'
import type { CarrierJournal } from './journal.js'

export function artifactHash(payload: MarketPayload): string {
  return toHex(Uint8Array.from(sha256(encodePayload(payload))))
}

export async function publishArtifact(p: {
  key: PrivateKey
  ledger: TreeLedger
  journal: CarrierJournal
  hosts: string[]
  net: string
  payload: MarketPayload
  quorum?: Quorum
}): Promise<{ txid: string; took: string[]; admitted: string[]; missed: Array<{ host: string; error: string }> }> {
  const hash = artifactHash(p.payload)
  const topics = lanesFor(p.net, bodyDomains(p.payload))
  let beef: number[] | undefined
  let txid: string | undefined
  const held = await p.journal.get(hash)
  if (held !== undefined) {
    beef = held.beef
    txid = held.txid
  } else {
    const slot = await p.ledger.take(
      'market',
      (await fundingLock(new ProtoWallet(p.key), ORIGINATOR, DERIVATION, FUNDING_TAG)).toHex()
    )
    if (slot === undefined) throw new Error('no funding slot')
    const c = await buildCarrier({ key: p.key, payload: encodePayload(p.payload), slot })
    beef = c.beef
    txid = c.carrier.id('hex')
    await p.journal.put(hash, { txid, beef, topics })
  }
  // the journal re-serves missed hosts — no sleeps inside the send path
  const r = await submitAll(p.hosts, beef, topics, { quorum: p.quorum, retryWaits: [] })
  if (!r.ok) throw new Error(`publishArtifact: quorum failed ${JSON.stringify(r.missed)}`)
  return { txid: txid, took: r.took, admitted: r.admitted, missed: r.missed }
}
