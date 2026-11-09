// TPNIL-v0 conformance cases — versioned interop checks over a real
// createNektarOverlay with a stub tracker: intake verification, net binding,
// tip SPV, expiry, fees, federation idempotency, beacon lifecycle.
import { describe, it, before } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey } from '@bsv/sdk'
import { createNektarOverlay } from '../src/index.js'
import { signWant, signOffer, signBeacon } from '../src/presence.js'


const key = PrivateKey.fromRandom()
let ls: Awaited<ReturnType<typeof createNektarOverlay>>['lsPresence']
const hdr = (h: number) => Promise.resolve(h === 1 || h === 40066 ? { hash: 'a'.repeat(64), height: h, prevHash: '0'.repeat(64), merkleRoot: '0'.repeat(64), time: 1, bits: 1, nonce: 1 } : undefined)
const tracker: any = { currentHeight: async () => 40066, tipHeight: async () => 40066, headerAtHeight: hdr, isValidRootForHeight: async () => true }

before(async () => {
  const ov = await createNektarOverlay(tracker, { broadcast: async () => ({ status: 'success' }) } as any, undefined, { net: 'ttn', strictNet: true })
  ls = ov.lsPresence
})

const want = (o = {}) => signWant(key, { domains: ['soccer'], ts: Date.now(), net: 'ttn', tipHeight: 40066, tip: 'a'.repeat(64), ...o })
const offer = (o = {}) => signOffer(key, { shard: 's', seq: 0, priceSats: 100, domains: ['soccer'], ts: Date.now(), net: 'ttn', tipHeight: 40066, tip: 'a'.repeat(64), ...o })

describe('TPNIL-v0 presence conformance', () => {
  it('accepts a valid SPV-bound want', async () => assert.equal(await ls.announceWant(want()), true))
  it('rejects a tampered signature', async () => { const w = want(); w.sig = '00' + w.sig.slice(2); assert.equal(await ls.announceWant(w), false) })
  it('rejects a wrong-net want', async () => assert.equal(await ls.announceWant(want({ net: 'main' })), false))
  it('rejects an unbound want in strict mode', async () => assert.equal(await ls.announceWant(want({ tipHeight: undefined, tip: undefined })), false))
  it('rejects a forged tip hash', async () => assert.equal(await ls.announceWant(want({ tip: 'b'.repeat(64) })), false))
  it('rejects an already-expired want', async () => assert.equal(await ls.announceWant(want({ exp: Date.now() - 1000 })), false))
  it('dedups re-announced events (federation idempotent)', async () => { const w = want(); await ls.announceWant(w); assert.equal(await ls.announceWant(w), true) })
  it('accepts a valid offer; rejects a forged tip on it', async () => {
    assert.equal(await ls.announceOffer(offer()), true)
    assert.equal(await ls.announceOffer(offer({ tip: 'b'.repeat(64) })), false)
  })
  it('beacon lifecycle: intake, peersFor, wrong-net rejected', async () => {
    const sb = signBeacon(key, { endpoint: 'https://rv.example', net: 'ttn', topics: ['tm_presence'], ts: Date.now() })
    assert.equal(await ls.announceBeacon(sb), true)
    assert.equal(ls.peersFor().some(x => x.beacon.endpoint === 'https://rv.example'), true)
    const bad = signBeacon(key, { endpoint: 'https://wrong.net', net: 'main', topics: ['tm_presence'], ts: Date.now() })
    assert.equal(await ls.announceBeacon(bad), false)
  })
})
