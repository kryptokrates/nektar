// Spike 0.1 — Groth16 Poseidon Merkle membership prove/verify in Node.
const snarkjs = require('snarkjs')
const { buildPoseidon } = require('circomlibjs')
const fs = require('node:fs')

const DEPTH = 16

async function main () {
  const poseidon = await buildPoseidon()
  const F = poseidon.F

  // --- tree: 6 leaves, Poseidon(2) binary tree, pad with zeros ---
  const H = (a, b) => F.toObject(poseidon([a, b]))
  const leaves = [101n, 202n, 303n, 404n, 505n, 606n]
  const tree = [leaves.map(BigInt)]
  while (tree[tree.length - 1].length > 1) {
    const prev = tree[tree.length - 1]
    const next = []
    for (let i = 0; i < prev.length; i += 2) {
      next.push(i + 1 < prev.length ? H(prev[i], prev[i + 1]) : prev[i])
    }
    tree.push(next)
  }
  const realRoot = tree[tree.length - 1][0]

  // --- witness for leaf 2 (value 303) at index 2 ---
  const index = 2
  const pathEls = []; const pathBits = []
  let pos = index
  for (let d = 0; d < DEPTH; d++) {
    const lvl = tree[d]
    const sib = pos % 2 === 0 ? pos + 1 : pos - 1
    pathEls.push(sib < lvl.length ? lvl[sib] : lvl[pos]) // missing sibling → duplicate (matches pad)
    pathBits.push(pos % 2)
    pos = Math.floor(pos / 2)
  }
  const secret = 12345n
  const queryId = 9999n
  const input = {
    leaf: leaves[index], secret, queryId,
    pathElements: pathEls, pathBits
  }

  // --- prove ---
  const t0 = Date.now()
  const { proof, publicSignals } = await snarkjs.groth16.fullProve(
    input, 'build/membership_js/membership.wasm', 'build/membership.zkey')
  const proveMs = Date.now() - t0

  const vkey = JSON.parse(fs.readFileSync('build/vkey.json', 'utf8'))
  const t1 = Date.now()
  const ok = await snarkjs.groth16.verify(vkey, publicSignals, proof)
  const verifyMs = Date.now() - t1

  // publicSignals: [queryId, root, nullifier] (public queryId first, then outputs)
  const psRoot = publicSignals[1]
  const psNull = publicSignals[2]

  // --- negatives ---
  const bad1 = await snarkjs.groth16.verify(vkey, [...publicSignals], { ...proof, pi_a: proof.pi_b }).catch(() => false)
  const tamperedPub = [...publicSignals]; tamperedPub[1] = (BigInt(psRoot) + 1n).toString()
  const bad2 = await snarkjs.groth16.verify(vkey, tamperedPub, proof).catch(() => false)
  // forged proof for a WRONG root: witness computes root from path → wrong tree fails at prove
  const wrongLeaf = { ...input, leaf: 777n }
  const bad3 = await snarkjs.groth16.fullProve(wrongLeaf, 'build/membership_js/membership.wasm', 'build/membership.zkey')
    .then(() => 'PROVED').catch(() => 'REJECTED')

  console.log(JSON.stringify({
    realRoot: String(realRoot), psRoot, psNull: String(psNull).slice(0, 24) + '…',
    rootMatches: String(realRoot) === psRoot,
    ok, proveMs, verifyMs,
    proofBytes: JSON.stringify(proof).length,
    negatives: { tamperedProof: bad1, tamperedPublic: bad2, wrongLeaf: bad3 }
  }, null, 1))
}
main().catch(e => { console.log('ERR', e); process.exit(1) })
