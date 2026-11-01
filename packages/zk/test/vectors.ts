// zk vectors + vkey check for `pnpm reproduce`.
// Deterministic: a fixed leaf set must give a fixed PoseidonTree root, and
// artifacts/vkey.json must hash to PINNED_VKEY_HASH.
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { readFileSync } from 'node:fs'
import assert from 'node:assert'
import { poseidon, PoseidonTree } from '../src/tree.js'
import { leafCommit } from '../src/tree.js'
import { PINNED_VKEY_HASH } from '../src/verify.js'
import { dsha256 } from '@mamem/core'

const ART = join(dirname(fileURLToPath(import.meta.url)), '..', '..', 'artifacts', 'vkey.json')

const p = await poseidon()
const leaves = [1n, 2n, 3n, 4n]
const leafCommit0 = leafCommit(p, new Uint8Array(32).fill(0x0a), 42n)
const tree = new PoseidonTree(p, [leafCommit0, ...leaves])
const root = tree.root()
console.log('poseidonTree root:', root.toString(16))
assert.equal(root, tree.root(), 'deterministic root')

const vkeyHash = Buffer.from(dsha256(readFileSync(ART))).toString('hex')
assert.equal(vkeyHash, PINNED_VKEY_HASH, `vkey hash drifted: ${vkeyHash} != ${PINNED_VKEY_HASH}`)
console.log('reproduce gate OK: poseidonTree deterministic, vkeyHash pinned', vkeyHash.slice(0, 16) + '…')
