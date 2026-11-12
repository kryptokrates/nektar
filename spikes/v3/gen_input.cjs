const { buildPoseidon } = require('circomlibjs')
const fs = require('node:fs')

const DEPTH = 16

async function main() {
  const poseidon = await buildPoseidon()
  const F = poseidon.F
  const H = (a, b) => F.toObject(poseidon([a, b]))

  // sparse zero convention: Z_0 = 0, Z_{d+1} = H(Z_d, Z_d)
  const Z = [0n]
  for (let d = 1; d <= DEPTH; d++) Z.push(H(Z[d - 1], Z[d - 1]))

  const leaves = [101n, 202n, 303n, 404n, 505n, 606n]
  // level arrays: each level holds computed nodes; missing siblings use Z[d]
  const tree = [leaves.slice()]
  while (tree.length <= DEPTH) {
    const prev = tree[tree.length - 1]
    const next = []
    for (let i = 0; i < prev.length; i += 2) {
      next.push(H(prev[i], i + 1 < prev.length ? prev[i + 1] : Z[tree.length - 1]))
    }
    if (next.length === 0) next.push(Z[tree.length]) // fully empty subtree
    tree.push(next)
  }
  const root = tree[DEPTH][0]

  const idx = 2
  const pe = []
  const pb = []
  let pos = idx
  for (let d = 0; d < DEPTH; d++) {
    const lvl = tree[d]
    const sib = pos % 2 === 0 ? pos + 1 : pos - 1
    const node = pos < lvl.length ? lvl[pos] : Z[d]
    // sibling is either present, or missing → the level's zero element
    pe.push(sib < lvl.length ? lvl[sib] : Z[d])
    pb.push(pos % 2)
    pos = Math.floor(pos / 2)
    void node
  }
  const input = {
    leaf: leaves[idx].toString(),
    secret: '12345',
    queryId: '9999',
    pathElements: pe.map(String),
    pathBits: pb.map(String)
  }
  fs.writeFileSync('witness_input.json', JSON.stringify(input))
  console.log('root', String(root))
}
main().catch((e) => {
  console.log('ERR', e)
  process.exit(1)
})
