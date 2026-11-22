import { test } from 'node:test'
import assert from 'node:assert/strict'
import { verifyAnswerReceipt, verifyReceiptZk, IngestError } from '../src/index.js'
import type { AnswerReceipt, MetricsPort } from '../src/index.js'

const receipt = (over: Partial<AnswerReceipt> = {}): AnswerReceipt => ({
  answer: 'a',
  fragmentRefs: [],
  checkResult: 'verified',
  truthNote: '',
  modelId: 'm',
  timestamp: 1,
  ...over
})

void test('receipt: unsigned and malformed signatures fail closed', () => {
  assert.deepEqual(verifyAnswerReceipt(receipt()), { ok: false, reason: 'unsigned receipt' })
  assert.deepEqual(verifyAnswerReceipt(receipt({ signerPub: 'zz' })), { ok: false, reason: 'unsigned receipt' })
  const bad = verifyAnswerReceipt(receipt({ signature: 'beef', signerPub: '02' + '11'.repeat(32) }))
  assert.equal(bad.ok, false)
  assert.match(bad.reason ?? '', /malformed signature/)
})

void test('receipt: zk artifact is re-verified, never trusted by presence', () => {
  assert.deepEqual(verifyReceiptZk({}), { ok: false, reason: 'no zk artifact on receipt' })
  const bad = verifyReceiptZk({ zk: { garbage: true } as unknown as Pick<AnswerReceipt, 'zk'>['zk'] })
  assert.equal(bad.ok, false)
  assert.match(bad.reason ?? '', /malformed zk artifact/)
})

void test('ingest: IngestError is a distinct refusal type', () => {
  const e = new IngestError('unsigned write')
  assert.ok(e instanceof Error)
  assert.equal(e.name, 'Error')
})

// the port seam: an impl stays assignable as long as it covers the declared
// surface — this object is the contract the mcp Metrics class satisfies
const metrics: MetricsPort = {
  inc: () => {},
  observe: () => {},
  gauge: () => {},
  value: () => 0,
  render: () => ''
}
void metrics
