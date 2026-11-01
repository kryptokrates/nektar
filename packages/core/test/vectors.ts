// Deterministic vectors + reproduce gate.
// `pnpm reproduce` regenerates vectors/core.json from code and diffs it
// against the committed copy — a byte-level change anywhere in the hashing /
// serialization pipeline fails the gate.
import { writeFileSync, readFileSync, existsSync, mkdirSync } from 'node:fs'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { merkleRoot, merkleProof } from '../src/merkle.js'
import { computeLeaf, hdp, serializeFragment } from '../src/schema.js'
import { chainRp, anchorFields, GENESIS_PREV_RP } from '../src/anchor.js'
import { dsha256, toHex, utf8, fromHex } from '../src/hash.js'

const VEC_DIR = join(dirname(fileURLToPath(import.meta.url)), '..', '..', '..', '..', 'vectors')
const FILE = join(VEC_DIR, 'core.json')

const frag = (i: number) => ({
  d: utf8(`deterministic fragment ${i}`),
  p: { source: 'test-harness', time: 1700000000 + i, signature: fromHex('00'.repeat(64)), modelId: 'mamem-test-1' }
})

const leaves = Array.from({ length: 9 }, (_, i) => computeLeaf(frag(i)))
const root = merkleRoot(leaves)
const proofs = [0, 4, 8].map(i => ({ index: i, siblings: merkleProof(leaves, i).siblings.map(toHex) }))
const R0 = root
const Rp0 = chainRp(GENESIS_PREV_RP, R0, leaves.length)

const vectors = {
  description: 'mamem core deterministic vectors (regenerate: pnpm reproduce --write)',
  fragments: leaves.map((l, i) => ({
    index: i,
    serialized: toHex(serializeFragment({ ...frag(i)!, hdp: hdp(frag(i)!), E: new Uint8Array(0) })),
    leaf: toHex(l)
  })),
  merkleRoot: toHex(root),
  proofs,
  anchor: {
    genesisRp: toHex(Rp0),
    fields: anchorFields({ shard: 'vec', n: 0, size: leaves.length, R: R0, Rp: Rp0 }).map(f => toHex(Uint8Array.from(f)))
  }
}

const out = JSON.stringify(vectors, null, 2) + '\n'
if (process.argv.includes('--write')) {
  mkdirSync(VEC_DIR, { recursive: true })
  writeFileSync(FILE, out)
  console.log('wrote', FILE)
} else {
  if (!existsSync(FILE)) {
    console.error('vectors missing:', FILE, '— run with --write')
    process.exit(1)
  }
  const prev = readFileSync(FILE, 'utf8')
  if (prev !== out) {
    console.error('REPRODUCE GATE FAILED: vectors/core.json differs from regenerated output')
    process.exit(1)
  }
  console.log('reproduce gate OK: vectors/core.json identical')
}
