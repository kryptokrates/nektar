// S0.5 gate: WASM verifier in native Node — ok / tampered / wrong image, timed
import { readFileSync } from 'node:fs'
import { encodeZkm2 } from '../../packages/zk/dist/src/stark/codec.js'
import { verifyZkm2, PINNED_IMAGE_ID } from '../../packages/zk/dist/src/stark/verify.js'
const p = JSON.parse(readFileSync(process.argv[2], 'utf8'))
const H = h => Uint8Array.from(Buffer.from(h, 'hex'))
const base = { imageId: H(p.imageId), root: H(p.journal.root), size: p.journal.size, queryId: H(p.journal.queryId), receipt: H(p.receipt) }
const run = (name, proof, pinned) => {
  const t = performance.now(); const r = verifyZkm2(encodeZkm2(proof), pinned); const ms = (performance.now() - t).toFixed(1)
  console.log(`${name}: ${r.ok ? 'ACCEPT' : 'reject — ' + r.reason} (${ms} ms)`)
  return r.ok
}
const tampered = base.receipt.slice(); tampered[Math.floor(tampered.length / 2)] ^= 1
const results = [
  run('ok', base) === true,
  run('ok again (warm)', base) === true,
  run('tampered receipt', { ...base, receipt: tampered }) === false,
  run('header root swapped', { ...base, root: new Uint8Array(32).fill(7) }) === false,
  run('header queryId swapped', { ...base, queryId: new Uint8Array(32).fill(7) }) === false,
  run('foreign image id (not pinned)', { ...base, imageId: new Uint8Array(32).fill(1) }) === false
]
console.log(`pinned=${PINNED_IMAGE_ID.slice(0, 16)}… gate ${results.every(Boolean) ? 'PASS' : 'FAIL'}`)
