import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey, ProtoWallet, Transaction, P2PKH } from '@bsv/sdk'
import { MemoryTreeLedger, mintTree } from '../src/tree.js'
import { MemoryCarrierJournal } from '../src/journal.js'
import { MemoryLogState, appendLog, readLog, anchorLog, publishMinedLog, logFundingLock, logTopic, LOG_KIND } from '../src/log.js'
import { submitAll } from '../src/hosts.js'
import { mintCarrier, lockFields, unlocker } from '@lightwebinc/bcommon'
import { EncodingLines, ProfileFull, TagAnchor, encodeAnchor, encodeBatch, fromHex, toHex } from '@lightwebinc/blogs-host/dist/index.js'
import { fund, prove } from './helpers.js'
import { minihost, FakeTracker } from '../src/testing.js'

const STREAM = 'nektar_abcdefghij'
const DER_B = { protocol: [1, 'blogs'] as [1, 'blogs'], keyID: 'batch' }
const DER_A = { protocol: [1, 'blogs'] as [1, 'blogs'], keyID: 'anchor' }

interface Rig {
  tracker: FakeTracker, h1: Awaited<ReturnType<typeof minihost>>, h2: Awaited<ReturnType<typeof minihost>>
  key: PrivateKey, ledger: MemoryTreeLedger, journal: MemoryCarrierJournal, state: MemoryLogState, hosts: string[]
}

async function rig (o: { drop?: (txid: string) => boolean, h2fail?: () => boolean, extraSlots?: number } = {}): Promise<Rig> {
  const tracker = new FakeTracker()
  const h1 = await minihost([], { tracker, logStream: STREAM, ...(o.drop !== undefined ? { drop: o.drop } : {}) })
  const h2 = await minihost([], { tracker, logStream: STREAM, ...(o.drop !== undefined ? { drop: o.drop } : {}), ...(o.h2fail !== undefined ? { failNext: o.h2fail } : {}) })
  const key = PrivateKey.fromRandom()
  const ledger = new MemoryTreeLedger()
  const journal = new MemoryCarrierJournal()
  const state = new MemoryLogState()
  const n = 8 + (o.extraSlots ?? 0)
  const tree = await mintTree({ key, funding: fund(key), n, satsPerOutput: 100, lock: await logFundingLock(key) })
  prove(tree)
  await ledger.add(tree, n, LOG_KIND)
  await ledger.markProven(tree.id('hex'))
  const hosts = [h1.url, h2.url]
  await publishMinedLog({ journal, hosts, stream: STREAM, tx: tree, quorum: 'all' })
  return { tracker, h1, h2, key, ledger, journal, state, hosts }
}

const lines = (n: number): string[] => [`{"v":1,"n":${n}}`]
const batch = (key: PrivateKey, seq: number, prev: Uint8Array, ts: number, text: string): Uint8Array => {
  const content = new TextEncoder().encode(text.endsWith('\n') ? text : `${text}\n`)
  return encodeBatch({ producer: new Uint8Array(key.toPublicKey().encode(true) as number[]), stream: STREAM, seq, prev, timestamp: ts, count: 1, encoding: EncodingLines, profile: ProfileFull, content, size: content.length, extra: [] })
}

// 1. three appends -> seq 1,2,3; readLog returns the lines in order, no gaps
test('log: three appends read back in order, no gaps, real admission', async () => {
  const s = await rig()
  try {
    const r1 = await appendLog({ key: s.key, ledger: s.ledger, journal: s.journal, state: s.state, hosts: s.hosts, stream: STREAM, lines: lines(1), now: () => 1000, quorum: 'all' })
    const r2 = await appendLog({ key: s.key, ledger: s.ledger, journal: s.journal, state: s.state, hosts: s.hosts, stream: STREAM, lines: lines(2), now: () => 2000, quorum: 'all' })
    const r3 = await appendLog({ key: s.key, ledger: s.ledger, journal: s.journal, state: s.state, hosts: s.hosts, stream: STREAM, lines: lines(3), now: () => 3000, quorum: 'all' })
    assert.deepEqual([r1.seq, r2.seq, r3.seq], [1, 2, 3])
    assert.notEqual(r1.txid, r2.txid)
    const read = await readLog({ hosts: s.hosts, stream: STREAM, producer: s.key.toPublicKey().toString(), tracker: s.tracker })
    assert.equal(read.head, 3)
    assert.deepEqual(read.batches.map(b => b.seq), [1, 2, 3])
    assert.deepEqual(read.gaps, [])
    assert.deepEqual(read.batches.flatMap(b => b.lines), [lines(1)[0], lines(2)[0], lines(3)[0]])
    assert.deepEqual(read.withheld, [])
    assert.deepEqual(read.unreachable, [])
    assert.equal(await s.ledger.remaining(LOG_KIND), 8 - 3, 'one slot per batch')
  } finally { await s.h1.close(); await s.h2.close() }
})

// 2. a failed submission retries the SAME journaled carrier: same txid/seq,
// no second slot
test('log: failed submission retries the journaled carrier — same txid, no second slot', async () => {
  let armed = false
  const s = await rig({ h2fail: () => armed })
  try {
    await appendLog({ key: s.key, ledger: s.ledger, journal: s.journal, state: s.state, hosts: s.hosts, stream: STREAM, lines: lines(1), now: () => 1000, quorum: 'all' })
    armed = true
    await assert.rejects(appendLog({ key: s.key, ledger: s.ledger, journal: s.journal, state: s.state, hosts: s.hosts, stream: STREAM, lines: lines(2), now: () => 2000, quorum: 'all' }), /log submit failed/)
    armed = false
    const slots = await s.ledger.remaining(LOG_KIND)
    const journaled = await s.journal.get(`log:${STREAM}:2`)
    assert.ok(journaled !== undefined, 'the failed batch was journaled before submission')
    const again = await appendLog({ key: s.key, ledger: s.ledger, journal: s.journal, state: s.state, hosts: s.hosts, stream: STREAM, lines: lines(2), now: () => 9999, quorum: 'all' })
    assert.equal(again.txid, journaled.txid, 'resubmits the same carrier')
    assert.equal(again.seq, 2)
    assert.equal(await s.ledger.remaining(LOG_KIND), slots, 'no second slot consumed')
    const read = await readLog({ hosts: s.hosts, stream: STREAM, producer: s.key.toPublicKey().toString(), tracker: s.tracker })
    assert.deepEqual(read.batches.map(b => b.seq), [1, 2])
    assert.deepEqual(read.gaps, [])
  } finally { await s.h1.close(); await s.h2.close() }
})

// 3. both hosts drop a batch -> the seq reports as a gap
test('log: batch dropped everywhere shows a gap; both hosts named withheld', async () => {
  const dropped = new Set<string>()
  const s = await rig({ drop: txid => dropped.has(txid) })
  try {
    for (const n of [1, 2, 3]) await appendLog({ key: s.key, ledger: s.ledger, journal: s.journal, state: s.state, hosts: s.hosts, stream: STREAM, lines: lines(n), now: () => n * 1000, quorum: 'all' })
    dropped.add((await s.journal.get(`log:${STREAM}:2`))!.txid)
    const both = await readLog({ hosts: s.hosts, stream: STREAM, producer: s.key.toPublicKey().toString(), tracker: s.tracker })
    assert.equal(both.batches.length, 2)
    assert.deepEqual(both.gaps, [{ from: 2, to: 2 }])
  } finally { await s.h1.close(); await s.h2.close() }
})

// 3b. sibling repair: only one of two hosts serves a batch
test('log: one host withholds a batch — the sibling fills it, withheld names it', async () => {
  const dropped = new Set<string>()
  const h2drop = (txid: string): boolean => dropped.has(txid)
  const tracker = new FakeTracker()
  const h1 = await minihost([], { tracker, logStream: STREAM })
  const h2 = await minihost([], { tracker, logStream: STREAM, drop: h2drop })
  const key = PrivateKey.fromRandom()
  const ledger = new MemoryTreeLedger()
  const journal = new MemoryCarrierJournal()
  const state = new MemoryLogState()
  try {
    const tree = await mintTree({ key, funding: fund(key), n: 8, satsPerOutput: 100, lock: await logFundingLock(key) })
    prove(tree)
    await ledger.add(tree, 8, LOG_KIND)
    await ledger.markProven(tree.id('hex'))
    const hosts = [h1.url, h2.url]
    await publishMinedLog({ journal, hosts, stream: STREAM, tx: tree, quorum: 'all' })
    for (const n of [1, 2, 3]) await appendLog({ key, ledger, journal, state, hosts, stream: STREAM, lines: lines(n), now: () => n * 1000, quorum: 'all' })
    dropped.add((await journal.get(`log:${STREAM}:2`))!.txid)
    const read = await readLog({ hosts, stream: STREAM, producer: key.toPublicKey().toString(), tracker })
    assert.equal(read.batches.length, 3, 'sibling repair fills the withheld batch')
    assert.deepEqual(read.gaps, [])
    assert.deepEqual(read.withheld, [{ host: h2.url, seq: 2 }])
  } finally { await h1.close(); await h2.close() }
})

// 4. a foreign producer's batch never lists for our producer; a wrong-prev
// batch reports broken positions
test('log: foreign batches filtered by producer; wrong-prev reports a break', async () => {
  const dropped = new Set<string>()
  const s = await rig({ drop: txid => dropped.has(txid), extraSlots: 2 })
  try {
    for (const n of [1, 2, 3]) await appendLog({ key: s.key, ledger: s.ledger, journal: s.journal, state: s.state, hosts: s.hosts, stream: STREAM, lines: lines(n), now: () => n * 1000, quorum: 'all' })
    const ours = s.key.toPublicKey().toString()
    const honest2 = (await s.journal.get(`log:${STREAM}:2`))!.txid

    // another producer on the same stream, admitted under its own key —
    // readLog for ours must not return it
    const other = PrivateKey.fromRandom()
    const oTree = await mintTree({ key: other, funding: fund(other), n: 2, satsPerOutput: 100, lock: await logFundingLock(other) })
    prove(oTree)
    await submitAll(s.hosts, oTree.toBEEF(), [logTopic(STREAM)])
    const oCarrier = await mintCarrier(new ProtoWallet(other), 'nektar', DER_B, batch(other, 1, new Uint8Array(32), 1, 'forged'), oTree, 0)
    const sub = await submitAll(s.hosts, oCarrier.toAtomicBEEF(), [logTopic(STREAM)])
    assert.equal(sub.took.length, 2, 'foreign batch admitted under its own producer')
    let read = await readLog({ hosts: s.hosts, stream: STREAM, producer: ours, tracker: s.tracker })
    assert.equal(read.batches.length, 3, 'foreign batch not returned for our producer')
    assert.deepEqual(read.gaps, [])

    // our own key with a wrong prev: a fork candidate at seq 2 — honest wins
    const slot = await s.ledger.take(LOG_KIND)
    const wCarrier = await mintCarrier(new ProtoWallet(s.key), 'nektar', DER_B, batch(s.key, 2, fromHex('11'.repeat(32)), 1, 'wrong prev'), slot!.tree, slot!.vout)
    assert.equal((await submitAll(s.hosts, wCarrier.toAtomicBEEF(), [logTopic(STREAM)])).took.length, 2)
    read = await readLog({ hosts: s.hosts, stream: STREAM, producer: ours, tracker: s.tracker })
    assert.equal(read.batches.length, 3, 'honest candidate preferred over the fork')
    assert.deepEqual(read.gaps, [])

    // when the honest seq-2 is withheld everywhere, the wrong-prev fork is
    // the only candidate: seq 2 AND seq 3 (whose prev names the withheld
    // honest batch) both report broken links
    dropped.add(honest2)
    read = await readLog({ hosts: s.hosts, stream: STREAM, producer: ours, tracker: s.tracker })
    assert.deepEqual(read.gaps, [{ from: 2, to: 2 }, { from: 3, to: 3 }], 'wrong-prev and its dependent report as breaks')
  } finally { await s.h1.close(); await s.h2.close() }
})

// 5. anchor: one interval token over 3 batches -> anchoredThrough 3;
// tampered root -> 0
test('log: anchor publishes a mined token; tampered root verifies to nothing', async () => {
  const s = await rig({ extraSlots: 4 })
  const mined = new Map<string, Transaction>()
  const broadcast = async (tx: Transaction) => { prove(tx); mined.set(tx.id('hex'), tx); return { status: 'success' } }
  const fetchTx = async (txid: string) => mined.get(txid)
  try {
    for (const n of [1, 2, 3]) await appendLog({ key: s.key, ledger: s.ledger, journal: s.journal, state: s.state, hosts: s.hosts, stream: STREAM, lines: lines(n), now: () => n * 1000, quorum: 'all' })
    const r = await anchorLog({ key: s.key, ledger: s.ledger, journal: s.journal, state: s.state, hosts: s.hosts, stream: STREAM, broadcast, fetchTx, minBatches: 1, maxAgeMs: 0 })
    assert.ok(r !== undefined)
    assert.deepEqual([r.interval, r.first, r.count], [1, 1, 3])
    // the flight completes once the token is mined: publish to the topic
    const done = await anchorLog({ key: s.key, ledger: s.ledger, journal: s.journal, state: s.state, hosts: s.hosts, stream: STREAM, broadcast, fetchTx, minBatches: 1, maxAgeMs: 0 })
    assert.ok(done !== undefined, 'flight resolved after the block')
    const read = await readLog({ hosts: s.hosts, stream: STREAM, producer: s.key.toPublicKey().toString(), tracker: s.tracker })
    assert.equal(read.anchoredThrough, 3, 'the interval covers seqs 1-3')

    // a second anchor with a tampered root is a valid token on the wire but
    // fails the recomputation -> stays unanchored
    const s2 = await rig({ extraSlots: 4 })
    try {
      for (const n of [1, 2, 3]) await appendLog({ key: s2.key, ledger: s2.ledger, journal: s2.journal, state: s2.state, hosts: s2.hosts, stream: STREAM, lines: lines(n), now: () => n * 1000, quorum: 'all' })
      const w = new ProtoWallet(s2.key)
      const badRecord = encodeAnchor({ producer: new Uint8Array(s2.key.toPublicKey().encode(true) as number[]), stream: STREAM, interval: 1, prev: new Uint8Array(32), first: 1, count: 3, root: fromHex('ff'.repeat(32)), timestamp: 1, extra: [] })
      const tx = new Transaction()
      const slot = await s2.ledger.take(LOG_KIND)
      tx.addInput({ sourceTransaction: slot!.tree, sourceOutputIndex: slot!.vout, unlockingScriptTemplate: unlocker(w, 'nektar', DER_B) })
      tx.addOutput({ satoshis: 1, lockingScript: await lockFields(w, 'nektar', DER_A, [TagAnchor, badRecord], true) })
      tx.addOutput({ lockingScript: new P2PKH().lock(s2.key.toPublicKey().toAddress()), change: true })
      await tx.fee(); await tx.sign()
      prove(tx)
      await publishMinedLog({ journal: s2.journal, hosts: s2.hosts, stream: STREAM, tx, quorum: 'all' })
      const bad = await readLog({ hosts: s2.hosts, stream: STREAM, producer: s2.key.toPublicKey().toString(), tracker: s2.tracker })
      assert.equal(bad.anchoredThrough, 0, 'tampered root never advances anchoredThrough')
    } finally { await s2.h1.close(); await s2.h2.close() }
  } finally { await s.h1.close(); await s.h2.close() }
})

// 6. golden: our encodeBatch output is byte-identical to the upstream Go
// vector testdata/vectors/batch-v1.json entry "batch-lines"
test('log: golden — batch record matches the upstream vector byte for byte', () => {
  const v = {
    producer: '0324653eac434488002cc06bbfb7f10fe18991e35f9fe4302dbea6d2353dc0ab1c',
    stream: 'example_app_qzxkvbmwtr', seq: 2,
    prev: '656ee0a653c6ea0667df0d52952454cae88766acc154143b52f7c69caaf0c383',
    timestamp: 1767225601000, count: 3,
    content: '7b226c6576656c223a22696e666f222c226d7367223a2273746172746564222c22686f7374223a226c6f67732e6578616d706c652e636f6d227d0a7b226c6576656c223a22696e666f222c226d7367223a226163636570746564222c2270656572223a223139322e302e322e3130227d0a7b226c6576656c223a227761726e222c226d7367223a221b5b324a636c65617265643f222c2270656572223a22323030313a6462383a3a3130227d0a',
    record: 'ab0044626c62010158210324653eac434488002cc06bbfb7f10fe18991e35f9fe4302dbea6d2353dc0ab1c02766578616d706c655f6170705f717a786b76626d7774720302045820656ee0a653c6ea0667df0d52952454cae88766acc154143b52f7c69caaf0c383051b0000019b76daabe80603070108010958ad7b226c6576656c223a22696e666f222c226d7367223a2273746172746564222c22686f7374223a226c6f67732e6578616d706c652e636f6d227d0a7b226c6576656c223a22696e666f222c226d7367223a226163636570746564222c2270656572223a223139322e302e322e3130227d0a7b226c6576656c223a227761726e222c226d7367223a221b5b324a636c65617265643f222c2270656572223a22323030313a6462383a3a3130227d0a0b18ad'
  }
  const content = fromHex(v.content)
  const rec = encodeBatch({ producer: fromHex(v.producer), stream: v.stream, seq: v.seq, prev: fromHex(v.prev), timestamp: v.timestamp, count: v.count, encoding: EncodingLines, profile: ProfileFull, content, size: content.length, extra: [] })
  assert.equal(toHex(rec), v.record)
})
