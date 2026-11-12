// gen-params.mjs — build a 16-leaf nektar-style Merkle tree (leaf=dsha256(item),
// node=dsha256(L||R) — same fold as packages/core/src/merkle.ts) and emit
// parameters.json for the circuit: root (baked constant), one leaf, its
// 4-deep path, index.
import { createHash } from 'node:crypto'
import { writeFileSync } from 'node:fs'
const dsha256 = (b) => createHash('sha256').update(createHash('sha256').update(b).digest()).digest()

const INDEX = Number(process.argv[2] ?? 5)
const leaves = Array.from({ length: 16 }, (_, i) => dsha256(Buffer.from(`record-${i} — sealed memory leaf`)))
let level = leaves.map((l) => Buffer.from(l))
const path = []
let pos = INDEX
while (level.length > 1) {
  path.push(level[pos % 2 === 0 ? pos + 1 : pos - 1])
  const next = []
  for (let i = 0; i < level.length; i += 2) next.push(dsha256(Buffer.concat([level[i], level[i + 1]])))
  level = next
  pos >>= 1
}
const root = level[0]
const params = {
  root: root.toString('hex'),
  leaf: leaves[INDEX].toString('hex'),
  siblings: path.map((b) => b.toString('hex')),
  index: INDEX
}
writeFileSync(new URL('./parameters.json', import.meta.url), JSON.stringify(params, null, 2))
console.log('root', params.root, 'index', INDEX, 'depth', path.length)
