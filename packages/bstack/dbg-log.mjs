import { minihost, FakeTracker } from './dist/src/testing.js'
import { MemoryTreeLedger, mintTree } from './dist/src/tree.js'
import { MemoryCarrierJournal } from './dist/src/journal.js'
import { MemoryLogState } from './dist/src/log.js'
import { publishMinedLog, appendLog, anchorLog, readLog, logFundingLock, LOG_KIND } from './dist/src/log.js'
import { fund, prove } from './dist/test/helpers.js'
import { PrivateKey } from '@bsv/sdk'

const STREAM = 'nektar_abcdefghij'
const tracker = new FakeTracker()
const h1 = await minihost([], { tracker, logStream: STREAM })
const key = PrivateKey.fromRandom()
const ledger = new MemoryTreeLedger()
const journal = new MemoryCarrierJournal()
const state = new MemoryLogState()
const tree = await mintTree({ key, funding: fund(key), n: 8, satsPerOutput: 100, lock: await logFundingLock(key) })
prove(tree)
await ledger.add(tree, 8, LOG_KIND)
await ledger.markProven(tree.id('hex'))
await publishMinedLog({ journal, hosts: [h1.url], stream: STREAM, tx: tree, quorum: 'all' })
console.log('tree published, host has:', h1.has(tree.id('hex')))

await appendLog({ key, ledger, journal, state, hosts: [h1.url], stream: STREAM, lines: ['{"v":1,"n":1}'], now: () => 1000, quorum: 'all' })
const read1 = await readLog({ hosts: [h1.url], stream: STREAM, producer: key.toPublicKey().toString(), tracker })
console.log('read1 batches:', read1.batches.length, 'anchoredThrough:', read1.anchoredThrough)

const mined = new Map()
const broadcast = async (tx) => { prove(tx); mined.set(tx.id('hex'), tx); return { status: 'success' } }
const fetchTx = async (txid) => mined.get(txid)
const r = await anchorLog({ key, ledger, journal, state, hosts: [h1.url], stream: STREAM, broadcast, fetchTx, minBatches: 1, maxAgeMs: 0 })
console.log('anchor flight:', JSON.stringify(r))
const done = await anchorLog({ key, ledger, journal, state, hosts: [h1.url], stream: STREAM, broadcast, fetchTx, minBatches: 1, maxAgeMs: 0 })
console.log('anchor done:', JSON.stringify(done))
const read2 = await readLog({ hosts: [h1.url], stream: STREAM, producer: key.toPublicKey().toString(), tracker })
console.log('read2 anchoredThrough:', read2.anchoredThrough)
await h1.close()
