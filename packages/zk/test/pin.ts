// reproduce gate (ZK): the pinned program ids verify the committed real
// receipts — the membership fixture's proven root is core/merkle.ts's root for
// the fixture batch, and the §14 derivation fixture's journal matches its
// header. Rebuilding the guests (`pnpm stark:build`) must keep the same ids.
import { readFileSync } from 'node:fs'
import { merkleRoot, dsha256, utf8, toHex, fromHex } from '@nektar/core'
import { encodeZkm2, verifyZkm2, PINNED_IMAGE_ID, encodeZkder, verifyZkder, PINNED_DATALOG_IMAGE_ID } from '../src/index.js'

const fx = JSON.parse(readFileSync(new URL('../../test/fixtures/zkm2-n5i4.json', import.meta.url), 'utf8'))
const root = toHex(merkleRoot(Array.from({ length: 5 }, (_, i) => dsha256(utf8(`leaf-5-${i}`)))))
const r = verifyZkm2(
  encodeZkm2({
    imageId: fromHex(fx.imageId),
    root: fromHex(fx.journal.root),
    size: fx.journal.size,
    queryId: fromHex(fx.journal.queryId),
    receipt: fromHex(fx.receipt)
  })
)
const ok = r.ok && r.journal.root === root && fx.imageId === PINNED_IMAGE_ID
const dfx = JSON.parse(readFileSync(new URL('../../test/fixtures/zkder-permit.json', import.meta.url), 'utf8'))
const dr = verifyZkder(
  encodeZkder({
    imageId: fromHex(dfx.imageId),
    policyRoot: fromHex(dfx.journal.policyRoot),
    conclusionHash: fromHex(dfx.journal.conclusionHash),
    queryId: fromHex(dfx.journal.queryId),
    receipt: fromHex(dfx.receipt)
  })
)
const dok = dr.ok && dfx.imageId === PINNED_DATALOG_IMAGE_ID
console.log(
  ok && dok
    ? `zk reproduce OK — membership ${PINNED_IMAGE_ID.slice(0, 16)}…, derivation ${PINNED_DATALOG_IMAGE_ID.slice(0, 16)}…`
    : `zk reproduce FAIL: ${ok ? 'derivation ' + (dr.ok ? 'image mismatch' : dr.reason) : 'membership ' + (r.ok ? 'root/image mismatch' : r.reason)}`
)
process.exit(ok && dok ? 0 : 1)
