import { minihost, FakeTracker } from './dist/src/testing.js'
import { MemoryTreeLedger, mintTree } from './dist/src/tree.js'
import { MemoryCarrierJournal } from './dist/src/journal.js'
import { MemoryLogState } from './dist/src/log.js'
import { publishMinedLog, appendLog, anchorLog, readLog, logFundingLock, LOG_KIND, logTopic } from './dist/src/log.js'
import { submitAll, lookupAll } from './dist/src/hosts.js'
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
await appendLog({ key, ledger, journal, state, hosts: [h1.url], stream: STREAM, lines: ['{"v":1,"n":1}'], now: () => 1000, quorum: 'all' })
const mined = new Map()
const broadcast = async (tx) => { prove(tx); mined.set(tx.id('hex'), tx); return { status: 'success' } }
const fetchTx = async (txid) => mined.get(txid)
await anchorLog({ key, ledger, journal, state, hosts: [h1.url], stream: STREAM, broadcast, fetchTx, minBatches: 1, maxAgeMs: 0 })
const done = await anchorLog({ key, ledger, journal, state, hosts: [h1.url], stream: STREAM, broadcast, fetchTx, minBatches: 1, maxAgeMs: 0 })
console.log('token txid:', done.txid, 'host has:', h1.has(done.txid))
const prod = key.toPublicKey().toString()
for (const q of [{ stream: STREAM, producer: prod, interval: 1 }, { stream: STREAM, producer: prod, seq: 1 }, { stream: STREAM, tree: done.txid }]) {
  const r = await lookupAll([h1.url], 'ls_log', q, {})
  console.log('query', JSON.stringify(q), '->', JSON.stringify(r.answers.map(a => ('error' in a ? a : { host: a.host, outputs: a.outputs.map(o => o.txid ?? '?') }))))
}
await h1.close()
