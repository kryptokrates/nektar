import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey, ProtoWallet, Transaction, P2PKH, Utils } from '@bsv/sdk'
import { MemoryTreeLedger, mintTree } from '../src/tree.js'
import { MemoryCarrierJournal } from '../src/journal.js'
import { MemoryFingerState, publishFinger, resolveFinger, fingerFundingLock, checkFingerBody, FINGER_KIND, type FingerBody } from '../src/finger.js'
import { mintCarrier } from '@lightwebinc/bcommon'
import { encodeRecord, MagicV1, type CommittedRecord } from '@lightwebinc/bfinger-host/dist/record.js'

import { fund, prove, fakeParent } from './helpers.js'
import { minihost, FakeTracker } from '../src/testing.js'
import { CborMap } from '@lightwebinc/bcommon'
import type { FundingOut } from '@nektar/chain'

const DER_R = { protocol: [1, 'bfinger'] as [1, 'bfinger'], keyID: 'record' }

const body1 = (hosts: string[] = ['https://one.example.com', 'https://two.example.com']): FingerBody => ({
  v: 1,
  hosts,
  office: 'nektar_abcdefghij',
  stream: 'nektar_abcdefghij',
  nets: ['regtest']
})

interface Rig {
  tracker: FakeTracker
  h1: Awaited<ReturnType<typeof minihost>>
  h2: Awaited<ReturnType<typeof minihost>>
  key: PrivateKey
  ledger: MemoryTreeLedger
  journal: MemoryCarrierJournal
  state: MemoryFingerState
  hosts: string[]
  mined: Map<string, Transaction>
  broadcasts: Transaction[]
  mempool: Set<string>
  tree: Transaction
  publish: (body: FingerBody) => Promise<{ changed: boolean; seq: number; carrierTxid?: string; tokenTxid?: string }>
}

async function rig(o: { failH1?: () => boolean; noproof?: boolean; provenFunding?: boolean; gapFunding?: boolean; liveMempool?: boolean; refuseBroadcast?: (tx: Transaction) => string | undefined } = {}): Promise<Rig> {
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
  const takeFunding = async (_sats: number): Promise<FundingOut> => {
    if (o.gapFunding === true) {
      // an unmined fee parent in raw-hex shape: its own input keeps the
      // sourceTXID but carries no sourceTransaction — the live funding gap
      const f = new Transaction()
      f.addInput({ sourceTransaction: fakeParent(key), sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(key) })
      f.addOutput({ satoshis: _sats, lockingScript: new P2PKH().lock(key.toPublicKey().toAddress()) })
      await f.sign()
      return { tx: Transaction.fromHex(f.toHex()), vout: 0, satoshis: _sats }
    }
    const f = fund(key)
    if (o.provenFunding === true) prove(f.tx) // the token's fee parent carries a proof
    return f
  }
  const mempool = new Set<string>()
  const broadcast = async (tx: Transaction): Promise<{ status: string }> => {
    const refused = o.refuseBroadcast?.(tx)
    if (refused !== undefined) return { status: 'error', description: refused } as { status: string }
    if (o.liveMempool === true) {
      // network model: an unmined input source must sit in mempool (or carry
      // a proof), else the broadcast refuses 'missing inputs' like the real
      // broadcaster — a mempool eviction can then be replayed per test
      for (const inp of tx.inputs) {
        const src = inp.sourceTransaction
        if (src !== undefined && src.merklePath === undefined && !mempool.has(src.id('hex'))) {
          return { status: 'error', description: 'WoC HTTP 400: Missing inputs' } as { status: string }
        }
      }
      mempool.add(tx.id('hex'))
    }
    if (o.noproof !== true) {
      prove(tx)
      mined.set(tx.id('hex'), tx)
    }
    broadcasts.push(tx)
    return { status: 'success' }
  }
  const fetchTx = async (txid: string): Promise<Transaction | undefined> => mined.get(txid)
  const publish = async (body: FingerBody) => await publishFinger({ key, state, ledger, journal, hosts, body, takeFunding, broadcast, fetchTx })
  return { tracker, h1, h2, key, ledger, journal, state, hosts, mined, broadcasts, publish, tree, mempool }
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
  } finally {
    await s.h1.close()
    await s.h2.close()
  }
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
  } finally {
    await s.h1.close()
    await s.h2.close()
  }
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
  } finally {
    await s.h1.close()
    await s.h2.close()
  }
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
      magic: MagicV1,
      identityKey: new Uint8Array(s.key.toPublicKey().encode(true) as number[]),
      seq: 2n,
      kind: 2,
      prev: new Uint8Array(32).fill(1),
      salt: new Uint8Array(32).fill(2),
      wc: new Uint8Array(32).fill(3),
      prevWitness: new Uint8Array(32).fill(4),
      notBefore: 0n,
      notAfter: 0n,
      body: new CborMap([
        { key: 'v', val: 1n },
        { key: 'hosts', val: ['https://evil.example.com'] },
        { key: 'office', val: 'nektar_abcdefghij' },
        { key: 'stream', val: 'nektar_abcdefghij' },
        { key: 'nets', val: ['regtest'] }
      ]),
      refs: [],
      unknown: []
    }
    const carrier = await mintCarrier(new ProtoWallet(other), 'nektar', DER_R, encodeRecord(forged), oTree, 0)
    // the real tm_finger refuses it: the carrier lock derives from `other`,
    // not from the identity the record names
    const { submitAll } = await import('../src/hosts.js')
    await submitAll(s.hosts, carrier.toAtomicBEEF(), ['tm_finger'], { retryWaits: [] })
    const got = await resolveFinger({ hosts: s.hosts, pub, tracker: s.tracker })
    assert.equal(got?.seq, 1, 'the forged seq-2 was refused; the real chain stands')
  } finally {
    await s.h1.close()
    await s.h2.close()
  }
})

// 4b. an unmined token whose ancestry is NOT proven resolves to nothing —
// the zero-conf path still requires proven parents (§10.3); the rig's
// takeFunding parent carries no merkle path, so tx.verify fails
test('finger: unmined token without proven ancestry resolves to nothing', async () => {
  const s = await rig({ noproof: true })
  try {
    await s.publish(body1())
    const got = await resolveFinger({ hosts: s.hosts, pub: s.key.toPublicKey().toString(), tracker: s.tracker })
    assert.equal(got, undefined, 'unmined token whose ancestry is unproven is not resolvable')
  } finally {
    await s.h1.close()
    await s.h2.close()
  }
})

// 4b2. upstream reader rule "mined or through its ancestry": a fresh token
// broadcast but not yet mined resolves while every input ancestor is proven
test('finger: unmined token resolves through proven ancestry', async () => {
  const s = await rig({ noproof: true, provenFunding: true })
  try {
    const r = await s.publish(body1())
    assert.equal(r.changed, true)
    const got = await resolveFinger({ hosts: s.hosts, pub: s.key.toPublicKey().toString(), tracker: s.tracker })
    assert.ok(got !== undefined, 'unmined token with proven ancestry resolves')
    assert.equal(got.seq, 1)
    assert.deepEqual(got.body, body1())
    assert.equal(got.tokenTxid, r.tokenTxid)
  } finally {
    await s.h1.close()
    await s.h2.close()
  }
})

// 4b3. a mined token whose BUMP root the tracker does not know still does
// not resolve — the carrier's funding-parent root alone is trusted here, so
// the refusal is attributable to the token check, not §10.4
test('finger: mined token whose proof the tracker rejects resolves to nothing', async () => {
  const s = await rig()
  try {
    await s.publish(body1())
    await s.publish(body1()) // propagate the proven token to the hosts
    s.tracker.strict = true
    s.tracker.validRoots.add(s.tree.merklePath!.computeRoot(s.tree.id('hex')))
    const got = await resolveFinger({ hosts: s.hosts, pub: s.key.toPublicKey().toString(), tracker: s.tracker })
    assert.equal(got, undefined, 'a BUMP root the tracker rejects fails closed')
  } finally {
    await s.h1.close()
    await s.h2.close()
  }
})

// 4b4. ancestry heal: the token was minted from an unmined fee parent whose
// own source is absent from the BEEF (the live testnet defect). Once fetchTx
// answers that parent proven, the next publish re-journals a healed BEEF and
// the record resolves while the token is still unmined. The rig's stub hosts
// admit the incomplete BEEF (they do not run the reference host's missing-
// source check), so the defect is asserted via verify() on the stored beef.
test('finger: stale ancestry heals once the fee parent is proven', async () => {
  const s = await rig({ noproof: true, gapFunding: true })
  try {
    const r = await s.publish(body1())
    assert.equal(r.changed, true)
    const pub = s.key.toPublicKey().toString()
    const beef0 = [...(await s.state.get())!.tokenBeef]
    const stx0 = Transaction.fromBEEF(beef0)
    assert.equal(await stx0.verify(s.tracker).catch(() => false), false, 'ancestry gap: SPV fails')
    assert.equal(await resolveFinger({ hosts: s.hosts, pub, tracker: s.tracker }), undefined)
    // the fee parent has since been mined — fetchTx now answers it proven
    const parent = stx0.inputs.find((i) => i.sourceTransaction !== undefined)!.sourceTransaction!
    prove(parent)
    s.mined.set(parent.id('hex'), parent)
    const r2 = await s.publish(body1())
    assert.equal(r2.changed, false)
    const st = (await s.state.get())!
    assert.notDeepEqual(st.tokenBeef, beef0, 'stored token BEEF was replaced by the healed one')
    const stx1 = Transaction.fromBEEF(st.tokenBeef)
    assert.equal(stx1.merklePath, undefined, 'the token itself is still unmined')
    assert.equal(await stx1.verify(s.tracker), true, 'ancestry proven — SPV passes')
    const got = await resolveFinger({ hosts: s.hosts, pub, tracker: s.tracker })
    assert.ok(got !== undefined, 'record resolves while the token is unmined')
    assert.equal(got.seq, 1)
    assert.deepEqual(got.body, body1())
  } finally {
    await s.h1.close()
    await s.h2.close()
  }
})

// 4b5. with proven ancestry there is nothing to heal — the stored BEEF is
// byte-identical across a re-publish
test('finger: ancestry heal is a no-op when ancestry is already proven', async () => {
  const s = await rig({ noproof: true, provenFunding: true })
  try {
    await s.publish(body1())
    const before = (await s.state.get())!.tokenBeef
    const r2 = await s.publish(body1())
    assert.equal(r2.changed, false)
    assert.deepEqual((await s.state.get())!.tokenBeef, before, 'stored BEEF untouched')
  } finally {
    await s.h1.close()
    await s.h2.close()
  }
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
    // http is refused unless the caller is on a regtest/local rig
    assert.throws(() => checkFingerBody(body1(['http://h.example.com'])))
    checkFingerBody(body1(['http://127.0.0.1:18091']), { allowHttp: true })
  } finally {
    await s.h1.close()
    await s.h2.close()
  }
})

// 5. golden: encodeRecord output is byte-identical to the upstream Go vector
// internal/protocol/record/testdata/record-v1-create.hex
test('finger: golden — record encoding matches the upstream vector byte for byte', () => {
  const golden =
    'ab00446266720101582102111111111111111111111111111111111111111111111111111111111111111102010301045820000000000000000000000000000000000000000000000000000000000000000005582022222222222222222222222222222222222222222222222222222222222222220658203333333333333333333333333333333333333333333333333333333333333333081a6553f10009000aa264706c616e6370726f6673746174757369617661696c61626c650b80'
  const rec: CommittedRecord = {
    magic: MagicV1,
    identityKey: new Uint8Array(Utils.toArray('02' + '11'.repeat(32), 'hex')),
    seq: 1n,
    kind: 1,
    prev: new Uint8Array(32),
    salt: new Uint8Array(32).fill(0x22),
    wc: new Uint8Array(32).fill(0x33),
    notBefore: 1700000000n,
    notAfter: 0n,
    body: new CborMap([
      { key: 'plan', val: 'pro' },
      { key: 'status', val: 'available' }
    ]),
    refs: [],
    unknown: []
  }
  assert.equal(Utils.toHex(Array.from(encodeRecord(rec))), golden)
})

// a token whose broadcast was refused is journaled but must never commit
// locally — the retry must re-attempt broadcast, not skip it (thit path).
test('finger: refused broadcast stalls the seq instead of committing a phantom', async () => {
  // refuse every broadcast — no ancestor heal can clear it; publish must
  // throw and the local chain must not commit the phantom
  let refusing = true
  const s = await rig({ noproof: true, liveMempool: true, refuseBroadcast: () => (refusing ? 'rejected by policy' : undefined) })
  try {
    await assert.rejects(() => s.publish(body1()), /broadcast refused/i)
    assert.equal(await s.state.get(), undefined, 'state must stay empty behind a refused token')
    const je = await s.journal.get('finger:token:1')
    assert.ok(je !== undefined, 'the journaled token is retained for retry')
    // lift the refusal and retry: the same journaled token must be broadcast
    // again (not skipped) — the retry the thit-hit path used to skip
    refusing = false
    const r = await s.publish(body1())
    assert.equal(r.changed, true)
    assert.equal(r.seq, 1)
    assert.equal(r.tokenTxid, je!.txid, 'the retried publish reuses the journaled token')
    assert.equal((await s.state.get())?.tokenTxid, r.tokenTxid)
  } finally {
    await s.h1.close()
    await s.h2.close()
  }
})

// a mempool-evicted prev token makes the next broadcast fail 'missing
// inputs'; the heal re-broadcasts the stored ancestry (oldest-first) and the
// publish then lands — the wedge that stranded ben/cara on testnet
test('finger: mempool-evicted parent heals via ancestor rebroadcast', async () => {
  const s = await rig({ noproof: true, provenFunding: true, liveMempool: true })
  try {
    const r1 = await s.publish(body1())
    assert.equal(r1.seq, 1)
    // evict the seq-1 token (unmined, dropped post-restart) — seq-2's token
    // spends it, so the next broadcast sees 'missing inputs'
    s.mempool.delete(r1.tokenTxid!)
    const r2 = await s.publish(body1(['https://three.example.com']))
    assert.equal(r2.changed, true)
    assert.equal(r2.seq, 2)
    // the evicted parent was re-broadcast before the seq-2 token landed
    const ids = s.broadcasts.map((t) => t.id('hex'))
    assert.ok(ids.filter((x) => x === r1.tokenTxid).length >= 2, 'prev token re-broadcast by the heal')
    assert.equal(ids[ids.length - 1], r2.tokenTxid, 'seq-2 token landed after the heal')
    const got = await resolveFinger({ hosts: [s.h1.url], pub: s.key.toPublicKey().toString(), tracker: s.tracker })
    assert.equal(got?.seq, 2)
  } finally {
    await s.h1.close()
    await s.h2.close()
  }
})
