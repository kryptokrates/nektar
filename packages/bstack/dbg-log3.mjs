import { FakeTracker } from './dist/src/testing.js'
import { MemoryTreeLedger, mintTree } from './dist/src/tree.js'
import { MemoryCarrierJournal } from './dist/src/journal.js'
import { MemoryLogState } from './dist/src/log.js'
import { appendLog, anchorLog, logFundingLock, LOG_KIND, logTopic } from './dist/src/log.js'
import { fund, prove } from './dist/test/helpers.js'
import { PrivateKey, Transaction } from '@bsv/sdk'
import { blogsModule } from '@lightwebinc/blogs-host/dist/module.js'

const STREAM = 'nektar_abcdefghij'
const tracker = new FakeTracker()
const mhost = { metrics: { preset: () => {}, inc: () => {}, gauge: () => {} }, log: (m, x) => console.log('HOST:', m, JSON.stringify(x)) }
const m = blogsModule(mhost, [{ stream: STREAM, policy: { open: true } }], [`tm_log_${STREAM}`])
const tm = m.topics[`tm_log_${STREAM}`]
const ls = m.lookups['ls_log']

const key = PrivateKey.fromRandom()
const ledger = new MemoryTreeLedger()
const journal = new MemoryCarrierJournal()
const state = new MemoryLogState()
const tree = await mintTree({ key, funding: fund(key), n: 8, satsPerOutput: 100, lock: await logFundingLock(key) })
prove(tree)
await ledger.add(tree, 8, LOG_KIND)
await ledger.markProven(tree.id('hex'))

const treeBeef = tree.toBEEF()
console.log('tree txid', tree.id('hex'))
try {
  const r = await tm.identifyAdmissibleOutputs(treeBeef, [])
  console.log('tree admit:', JSON.stringify(r))
  for (const o of r.outputsToAdmit) await ls.outputAdmittedByTopic({ mode: 'whole-tx', atomicBEEF: treeBeef, outputIndex: o, topic: `tm_log_${STREAM}` })
} catch (e) { console.log('tree admit THREW:', e.message) }

// coin check on tree outputs
for (let v = 0; v < 8; v++) {
  const c = await ls.coin(`tm_log_${STREAM}`, tree.id('hex'), v)
  if (c) console.log('coin', v, '->', JSON.stringify(c).slice(0, 90))
}
