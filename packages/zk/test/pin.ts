// reproduce gate (ZK): the pinned program id verifies the committed real
// receipt, and its proven root is core/merkle.ts's root for the fixture batch.
// Rebuilding the guest (`pnpm stark:build`) must keep the same image id.
import { readFileSync } from 'node:fs'
import { merkleRoot, dsha256, utf8, toHex, fromHex } from '@mamem/core'
import { encodeZkm2, verifyZkm2, PINNED_IMAGE_ID } from '../src/index.js'

const fx = JSON.parse(readFileSync(new URL('../../test/fixtures/zkm2-n5i4.json', import.meta.url), 'utf8'))
const root = toHex(merkleRoot(Array.from({ length: 5 }, (_, i) => dsha256(utf8(`leaf-5-${i}`)))))
const r = verifyZkm2(encodeZkm2({ imageId: fromHex(fx.imageId), root: fromHex(fx.journal.root), size: fx.journal.size, queryId: fromHex(fx.journal.queryId), receipt: fromHex(fx.receipt) }))
const ok = r.ok && r.journal.root === root && fx.imageId === PINNED_IMAGE_ID
console.log(ok ? `zk reproduce OK — image id ${PINNED_IMAGE_ID.slice(0, 16)}…, STARK fixture root = core merkleRoot` : `zk reproduce FAIL: ${r.ok ? 'root/image mismatch' : r.reason}`)
process.exit(ok ? 0 : 1)
