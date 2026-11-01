// JSONL of executor cases: every leaf of sizes 1..33 (must reproduce core merkleRoot)
// plus negatives that the guest MUST refuse.
import { merkleRoot, merkleProof, dsha256, utf8, toHex } from '../../packages/core/dist/src/index.js'
const q = toHex(dsha256(utf8('q')))
const out = []
for (let n = 1; n <= 33; n++) {
  const leaves = Array.from({ length: n }, (_, i) => dsha256(utf8(`leaf-${n}-${i}`)))
  const root = toHex(merkleRoot(leaves))
  for (let i = 0; i < n; i++) {
    const pr = merkleProof(leaves, i)
    out.push({ case: `n${n}i${i}`, expect: root, leaf: toHex(leaves[i]), index: i, size: n, siblings: pr.siblings.map(toHex), queryId: q })
  }
}
const leaves = Array.from({ length: 5 }, (_, i) => dsha256(utf8(`leaf-5-${i}`)))
const p4 = merkleProof(leaves, 4)
const base = { leaf: toHex(leaves[4]), index: 4, size: 5, siblings: p4.siblings.map(toHex), queryId: q }
out.push({ ...base, case: 'neg:index>=size', index: 5, expect: 'refuse' })
out.push({ ...base, case: 'neg:short-path', siblings: base.siblings.slice(1), expect: 'refuse' })
// duplicate-last-leaf attack: claim a 6th leaf (index 5 of size 6) using leaf 4's self-pair path
out.push({ ...base, case: 'neg:odd-not-self-paired', siblings: [toHex(dsha256(utf8('x'))), ...base.siblings.slice(1)], expect: 'refuse' })
// non-member leaf with a real path → executes, but root ≠ anchored root (verifier rejects)
// (at the odd last position the guest already refuses a foreign leaf via the self-pair rule)
out.push({ ...base, case: 'neg:odd-last-foreign-leaf', leaf: toHex(dsha256(utf8('not-in-batch'))), expect: 'refuse' })
const p1 = merkleProof(leaves, 1)
out.push({ case: 'neg:non-member-root-differs', leaf: toHex(dsha256(utf8('not-in-batch'))), index: 1, size: 5, siblings: p1.siblings.map(toHex), queryId: q, expect: 'root!=' + toHex(merkleRoot(leaves)) })
for (const o of out) console.log(JSON.stringify(o))
