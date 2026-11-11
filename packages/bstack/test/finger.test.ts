import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey, ProtoWallet, Transaction, Utils } from '@bsv/sdk'
import { MemoryTreeLedger, mintTree } from '../src/tree.js'
import { MemoryCarrierJournal } from '../src/journal.js'
import {
  MemoryFingerState, publishFinger, resolveFinger, fingerFundingLock,
  checkFingerBody, FINGER_KIND, type FingerBody
} from '../src/finger.js'
import { mintCarrier } from '@lightwebinc/bcommon'
import { encodeRecord, MagicV1, type CommittedRecord } from '@lightwebinc/bfinger-host/dist/record.js'
import { FundingTag } from '@lightwebinc/bfinger-host/dist/funding.js'
import { fund, prove } from './helpers.js'
import { minihost, FakeTracker } from '../src/testing.js'
import { CborMap } from '@lightwebinc/bcommon'
import type { FundingOut } from '@nektar/chain'

const DER_R = { protocol: [1, 'bfinger'] as [1, 'bfinger'], keyID: 'record' }

const body1 = (hosts: string[] = ['https://one.example.com', 'https://two.example.com']): FingerBody =>
  ({ v: 1, hosts, office: 'nektar_abcdefghij', stream: 'nektar_abcdefghij', nets: ['mock'] })

interface Rig {
  tracker: FakeTracker, h1: Awaited<ReturnType<typeof minihost>>, h2: Awaited<ReturnType<typeof minihost>>
  key: PrivateKey, ledger: MemoryTreeLedger, journal: MemoryCarrierJournal, state: MemoryFingerState
  hosts: string[], mined: Map<string, Transaction>, broadcasts: Transaction[]
  publish: (body: FingerBody) => Promise<{ changed: boolean, seq: number, carrierTxid?: string, tokenTxid?: string }>
}

async function rig (o: { failH1?: () => boolean, noproof?: boolean } = {}): Promise<Rig> {
  const tracker = new FakeTracker()
  const h1 = await minihost([], { tracker, finger: true, ...(o.failH1 !== undefined ? { failNext: () => o.failH1!() } : {}) })
  const h2 = await minihost([], { tracker, finger: true })
  const key = PrivateKey.fromRandom()
  const ledger = new MemoryTreeLedger()
  const journal = new MemoryCarrierJournal()
  const state = new MemoryFingerState()
  const tree = await mintTree({ key, funding: fund(key), n: 8, satsPerOutput: 100, lock: await fingerFundingLock(key) })
  prove(tree)
  await ledger.add(tree, 8, FINGER_KIND)
  await ledger.markProven(tree.id('hex'))
  const mined = new Map<string, Transaction>()
  const broadcasts: Transaction[] = []
  const hosts = [h1.url, h2.url]
  const takeFunding = async (_sats: number): Promise<FundingOut> => fund(key)
  const broadcast = async (tx: Transaction): Promise<{ status: string }> => {
    if (o.noproof !== true) { prove(tx); mined.set(tx.id('hex'), tx) }
    broadcasts.push(tx)
    return { status: 'success' }
  }
  const fetchTx = async (txid: string): Promise<Transaction | undefined> => mined.get(txid)
  const publish = async (body: FingerBody) =>
    await publishFinger({ key, state, ledger, journal, hosts, body, takeFunding, broadcast, fetchTx })
  return { tracker, h1, h2, key, ledger, journal, state, hosts, mined, broadcasts, publish }
}

// 1. create → token mined → resolve on either host returns the body, seq 1
test('finger: create resolves on either host once the token is mined', async () => {
  const s = await rig()
  try {
    const r = await s.publish(body1())
    assert.equal(r.changed, true)
    assert.equal(r.seq, 1)
    assert.equal(r.carrierTxid !== undefined && r.carrierTxid.length === 64, true)
    // the record resolves only after the token's proof is on the hosts: the
    // next call (changed:false) propagates the proven BEEF
    const p2 = await s.publish(body1())
    assert.equal(p2.changed, false)
    for (const h of [s.h1.url, s.h2.url]) {
      const got = await resolveFinger({ hosts: [h], pub: s.key.toPublicKey().toString(), tracker: s.tracker })
      assert.ok(got !== undefined, `resolve on ${h}`)
      assert.equal(got.seq, 1, 'upstream first sequence is 1')
      assert.deepEqual(got.body, body1())
      assert.equal(got.tokenTxid, r.tokenTxid)
      assert.deepEqual(got.hostsAnswered, [h])
      assert.deepEqual(got.unreachable, [])
    }
  } finally { await s.h1.close(); await s.h2.close() }
})

// 2. same body again → changed:false, no slot consumed, no second broadcast
test('finger: unchanged body is a no-op — no slot, no broadcast', async () => {
  const s = await rig()
  try {
    await s.publish(body1())
    const slots = await s.ledger.remaining(FINGER_KIND)
    const calls = s.broadcasts.length
    const r = await s.publish(body1())
    assert.equal(r.changed, false)
    assert.equal(r.seq, 1)
    assert.equal(await s.ledger.remaining(FINGER_KIND), slots, 'no funding slot consumed')
    assert.equal(s.broadcasts.length, calls, 'nothing broadcast')
  } finally { await s.h1.close(); await s.h2.close() }
})

// 3. update → next seq, new body; a stale host loses; quorum 2 with one
// updated host → undefined
test('finger: update wins on the newest seq; quorum needs agreement', async () => {
  let armed = false
  const s = await rig({ failH1: () => armed })
  try {
    await s.publish(body1())
    await s.publish(body1()) // propagate the proven token to both hosts
    for (const h of [s.h1.url, s.h2.url]) {
      const got = await resolveFinger({ hosts: [h], pub: s.key.toPublicKey().toString(), tracker: s.tracker })
      assert.equal(got?.seq, 1)
    }
    const v2 = body1(['https://three.example.com', 'https://four.example.com'])
    armed = true
    const r = await s.publish(v2)
    assert.equal(r.seq, 2)
    await s.publish(v2) // proof propagation still fails on h1
    armed = false
    // h1 still serves seq 1, h2 serves seq 2 — the newer wins
    const got = await resolveFinger({ hosts: s.hosts, pub: s.key.toPublicKey().toString(), tracker: s.tracker })
    assert.equal(got?.seq, 2)
    assert.deepEqual(got?.body, v2)
    assert.deepEqual(got?.hostsAnswered, [s.h2.url])
    const h1only = await resolveFinger({ hosts: [s.h1.url], pub: s.key.toPublicKey().toString(), tracker: s.tracker })
    assert.equal(h1only?.seq, 1, 'stale host still answers its old record')
    // quorum 2 with only one host updated → nothing resolves
    const q2 = await resolveFinger({ hosts: s.hosts, pub: s.key.toPublicKey().toString(), tracker: s.tracker, quorum: 2 })
    assert.equal(q2, undefined, 'quorum not reached')
  } finally { await s.h1.close(); await s.h2.close() }
})

// 4a. a record for this identity signed under another key never displaces
// the real chain (refused at admission: the lock names the wrong record key)
test('finger: forged record under another key loses to the real chain', async () => {
  const s = await rig()
  try {
    await s.publish(body1())
    await s.publish(body1())
    const pub = s.key.toPublicKey().toString()
    const other = PrivateKey.fromRandom()
    const oTree = await mintTree({ key: other, funding: fund(other), n: 2, satsPerOutput: 100, lock: await fingerFundingLock(other) })
    prove(oTree)
    const forged: CommittedRecord = {
      magic: MagicV1, identityKey: new Uint8Array(s.key.toPublicKey().encode(true) as number[]),
      seq: 2n, kind: 2, prev: new Uint8Array(32).fill(1), salt: new Uint8Array(32).fill(2),
      wc: new Uint8Array(32).fill(3), prevWitness: new Uint8Array(32).fill(4),
      notBefore: 0n, notAfter: 0n,
      body: new CborMap([{ key: 'v', val: 1n }, { key: 'hosts', val: ['https://evil.example.com'] }, { key: 'office', val: 'nektar_abcdefghij' }, { key: 'stream', val: 'nektar_abcdefghij' }, { key: 'nets', val: ['mock'] }]),
      refs: [], unknown: []
    }
    const carrier = await mintCarrier(new ProtoWallet(other), 'nektar', DER_R, encodeRecord(forged), oTree, 0)
    // the real tm_finger refuses it: the carrier lock derives from `other`,
    // not from the identity the record names
    const { submitAll } = await import('../src/hosts.js')
    await submitAll(s.hosts, carrier.toAtomicBEEF(), ['tm_finger'], { retryWaits: [] })
    const got = await resolveFinger({ hosts: s.hosts, pub, tracker: s.tracker })
    assert.equal(got?.seq, 1, 'the forged seq-2 was refused; the real chain stands')
  } finally { await s.h1.close(); await s.h2.close() }
})

// 4b. an unproven token never resolves — the reader requires SPV (§10.3)
test('finger: unmined token resolves to nothing', async () => {
  const s = await rig({ noproof: true })
  try {
    await s.publish(body1())
    const got = await resolveFinger({ hosts: s.hosts, pub: s.key.toPublicKey().toString(), tracker: s.tracker })
    assert.equal(got, undefined, 'token without a merkle path is not resolvable')
  } finally { await s.h1.close(); await s.h2.close() }
})

// 4c. body validation: 9 hosts, a javascript: URL and a non-URL all refuse
test('finger: body validation refuses too many or bad hosts', async () => {
  const s = await rig()
  try {
    await assert.rejects(s.publish(body1(Array.from({ length: 9 }, (_, i) => `https://h${i}.example.com`))), /1\.\.8/)
    await assert.rejects(s.publish(body1(['javascript:alert(1)'])), /https/)
    await assert.rejects(s.publish(body1(['not a url'])), /bad host URL/)
    assert.throws(() => checkFingerBody(body1(['ftp://x.example.com'])))
    assert.throws(() => checkFingerBody({ ...body1(), office: 'bad office' }))
    assert.throws(() => checkFingerBody({ ...body1(), stream: 'bad stream' }))
    assert.throws(() => checkFingerBody({ ...body1(), nets: ['UPPER'] }))
    // http is refused unless the caller is on a mock/regtest network
    assert.throws(() => checkFingerBody(body1(['http://h.example.com'])))
    checkFingerBody(body1(['http://127.0.0.1:18091']), { allowHttp: true })
  } finally { await s.h1.close(); await s.h2.close() }
})

// 5. golden: encodeRecord output is byte-identical to the upstream Go vector
// internal/protocol/record/testdata/record-v1-create.hex
test('finger: golden — record encoding matches the upstream vector byte for byte', () => {
  const golden = 'ab00446266720101582102111111111111111111111111111111111111111111111111111111111111111102010301045820000000000000000000000000000000000000000000000000000000000000000005582022222222222222222222222222222222222222222222222222222222222222220658203333333333333333333333333333333333333333333333333333333333333333081a6553f10009000aa264706c616e6370726f6673746174757369617661696c61626c650b80'
  const rec: CommittedRecord = {
    magic: MagicV1,
    identityKey: new Uint8Array(Utils.toArray('02' + '11'.repeat(32), 'hex')),
    seq: 1n, kind: 1, prev: new Uint8Array(32),
    salt: new Uint8Array(32).fill(0x22), wc: new Uint8Array(32).fill(0x33),
    notBefore: 1700000000n, notAfter: 0n,
    body: new CborMap([{ key: 'plan', val: 'pro' }, { key: 'status', val: 'available' }]),
    refs: [], unknown: []
  }
  assert.equal(Utils.toHex(Array.from(encodeRecord(rec))), golden)
})
