// proveMembership — holder side: fragment leaf hash + blinding + path → L_ZKMem.
import * as snarkjs from 'snarkjs'
import { readFileSync } from 'node:fs'
import { PoseidonTree, leafCommit, splitLeafHash, DEPTH } from './tree.js'
import { encodeZkMem, type ZkMemProof } from './codec.js'
import { queryIdOf, type ZkQuery } from './query.js'
import { dsha256 } from '@mamem/core'

export interface Artifacts { wasm: string, zkey: string, vkey: string }
export function loadArtifacts (dir: string): Artifacts {
  return { wasm: `${dir}/membership_js/membership.wasm`, zkey: `${dir}/membership.zkey`, vkey: `${dir}/vkey.json` }
}
export function vkeyHashOf (vkeyPath: string): Uint8Array {
  return dsha256(readFileSync(vkeyPath))
}

export async function proveMembership (args: {
  leaf32: Uint8Array, blinding: bigint, index: number, tree: PoseidonTree,
  query: ZkQuery, artifacts: Artifacts, vkeyHash: Uint8Array
}): Promise<Uint8Array> {
  const path = args.tree.path(args.index)
  const { hi, lo } = splitLeafHash(args.leaf32)
  const input = {
    hi: hi.toString(), lo: lo.toString(), blinding: args.blinding.toString(),
    queryId: queryIdOf(args.query).toString(),
    pathElements: path.elements.map(String), pathBits: path.bits.map(String)
  }
  const { proof, publicSignals } = await snarkjs.groth16.fullProve(input, args.artifacts.wasm, args.artifacts.zkey)
  // circom public-signal order: [public outputs, then public inputs] →
  // [root, nullifier, queryId]
  const zm: ZkMemProof = {
    vkeyHash: args.vkeyHash,
    root: BigInt(publicSignals[0]), nullifier: BigInt(publicSignals[1]), queryId: BigInt(publicSignals[2]),
    pi_a: [BigInt(proof.pi_a[0]), BigInt(proof.pi_a[1])],
    pi_b: [[BigInt(proof.pi_b[0][0]), BigInt(proof.pi_b[0][1])], [BigInt(proof.pi_b[1][0]), BigInt(proof.pi_b[1][1])]],
    pi_c: [BigInt(proof.pi_c[0]), BigInt(proof.pi_c[1])]
  }
  return encodeZkMem(zm)
}
