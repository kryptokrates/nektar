// emit prover input for leaf `index` of an n-leaf batch built with core/merkle.ts
import { merkleRoot, merkleProof, dsha256, utf8, toHex } from '../../packages/core/dist/src/index.js'
const n = Number(process.argv[2] ?? 5),
  index = Number(process.argv[3] ?? 2)
const leaves = Array.from({ length: n }, (_, i) => dsha256(utf8(`leaf-${n}-${i}`)))
const pr = merkleProof(leaves, index)
console.log(
  JSON.stringify({
    leaf: toHex(leaves[index]),
    index,
    size: n,
    siblings: pr.siblings.map(toHex),
    queryId: toHex(dsha256(utf8('q1'))),
    expectRoot: toHex(merkleRoot(leaves))
  })
)
