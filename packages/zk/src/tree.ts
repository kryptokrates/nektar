// PoseidonTree — companion Merkle tree over batch leaves, same order as the
// SHA batch tree. Sparse zero padding Z_{d+1}=H(Z_d,Z_d), depth 16 → 65,536
// leaves max. Built with circomlibjs Poseidon to match the circuit exactly.
import { buildPoseidon } from 'circomlibjs'

export const DEPTH = 16
export const MAX_LEAVES = 1 << DEPTH

let _poseidon: any = null
export async function poseidon (): Promise<any> {
  if (!_poseidon) _poseidon = await buildPoseidon()
  return _poseidon
}
export function fieldToBigInt (poseidon: any, out: Uint8Array | bigint): bigint {
  return BigInt(poseidon.F.toObject(out))
}

// domain zero elements: Z_0=0, Z_{d+1}=H(Z_d,Z_d)
export function zeroChain (poseidon: any): bigint[] {
  const Z = [0n]
  for (let d = 1; d <= DEPTH; d++) Z.push(fieldToBigInt(poseidon, poseidon([Z[d - 1], Z[d - 1]])))
  return Z
}

export class PoseidonTree {
  readonly leaves: bigint[]
  private readonly Z: bigint[]
  constructor (private poseidon: any, leaves: bigint[]) {
    if (leaves.length > MAX_LEAVES) {
      throw new Error(`PoseidonTree capacity exceeded: ${leaves.length} leaves > ${MAX_LEAVES} max (depth ${DEPTH})`)
    }
    this.leaves = leaves.slice()
    this.Z = zeroChain(poseidon)
  }
  /** node value at level `d`, index `i` — sparse: missing → Z[d] */
  private nodeAt (lvl: bigint[], d: number, i: number): bigint {
    return i < lvl.length ? lvl[i] : this.Z[d]
  }
  /** level array for level d (computed nodes only) */
  level (d: number): bigint[] {
    let lvl = this.leaves
    for (let dd = 0; dd < d; dd++) {
      const next: bigint[] = []
      for (let i = 0; i < lvl.length; i += 2) {
        next.push(fieldToBigInt(this.poseidon, this.poseidon([lvl[i], this.nodeAt(lvl, dd, i + 1)])))
      }
      lvl = next.length ? next : [this.Z[dd + 1]]
    }
    return lvl
  }
  root (): bigint {
    let cur = this.leaves
    for (let d = 0; d < DEPTH; d++) {
      const next: bigint[] = []
      for (let i = 0; i < cur.length; i += 2) {
        next.push(fieldToBigInt(this.poseidon, this.poseidon([cur[i], this.nodeAt(cur, d, i + 1)])))
      }
      cur = next
    }
    return cur[0] ?? this.Z[DEPTH]
  }
  /** path for index: {elements, bits} for the circuit */
  path (index: number): { elements: bigint[], bits: number[] } {
    const elements: bigint[] = []; const bits: number[] = []
    let pos = index
    for (let d = 0; d < DEPTH; d++) {
      const lvl = this.level(d)
      const sib = pos % 2 === 0 ? pos + 1 : pos - 1
      elements.push(sib < lvl.length ? lvl[sib] : this.Z[d])
      bits.push(pos % 2)
      pos = Math.floor(pos / 2)
    }
    return { elements, bits }
  }
}

// leaf commitment for the circuit: Poseidon(3)(hi, lo, blinding) where hi/lo
// are the 128-bit halves of the fragment's SHA-256 leaf hash.
export function splitLeafHash (leaf32: Uint8Array): { hi: bigint, lo: bigint } {
  return { hi: BigInt('0x' + Buffer.from(leaf32.subarray(0, 16)).toString('hex')), lo: BigInt('0x' + Buffer.from(leaf32.subarray(16)).toString('hex')) }
}
export function leafCommit (poseidon: any, leaf32: Uint8Array, blinding: bigint): bigint {
  const { hi, lo } = splitLeafHash(leaf32)
  return fieldToBigInt(poseidon, poseidon([hi, lo, blinding]))
}
