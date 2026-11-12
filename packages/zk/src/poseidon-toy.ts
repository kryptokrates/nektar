// poseidon-toy — a verbatim port of the Poseidon sponge configuration used by
// nChain's zkscript examples/ai_inference circuit (arkworks ark-crypto-
// primitives PoseidonSponge over BLS12-381 Fr).
//
// HONEST SCOPE: the parameters are nChain's DEMO constants — MDS [[1..9]],
// sequential ARK — not a security-audited parameterization. This exists to
// recompute the proof's public statement poseidon(input‖output‖poseidon(model))
// off-circuit so a verifier can re-derive what the on-chain Groth16 verifier
// checked. A production parameterization swaps the constants only — same code.
//
// Semantics (ark-crypto-primitives 0.4 PoseidonSponge duplex construction):
//   state = capacity(1) ‖ rate(2) — capacity FIRST (state[0] is capacity)
//   absorb:   state[capacity + i] += elem, permuting whenever a full rate
//             block is reached before the next element
//   squeeze:  absorbing→squeezing transition permutes ONCE, then emits
//             state[capacity + 0] (squeeze_field_elements(1) → state[1])
//   permute:  65 rounds total = 4 full + 57 partial + 4 full
//             each round: state += ARK[round]; s-box x^5 (all elements on
//             full rounds, state[0] only on partial); state = MDS · state

/** BLS12-381 scalar field order */
export const FR_MODULUS = 0x73eda753299d7d483339d80809a1d80553bda402fffe5bfeffffffff00000001n

const FULL_ROUNDS = 8
const PARTIAL_ROUNDS = 57
const ALPHA = 5n
const RATE = 2
const CAPACITY = 1
const SIZE = RATE + CAPACITY

const MDS = [
  [1n, 2n, 3n],
  [4n, 5n, 6n],
  [7n, 8n, 9n]
]
/** ark[i] = (i, i+1, i+2) — nChain's sequential round constants */
const ARK = Array.from({ length: FULL_ROUNDS + PARTIAL_ROUNDS }, (_, i) => [BigInt(i), BigInt(i + 1), BigInt(i + 2)])

/** field normalize — negatives wrap (arkworks Fr::from(i64) semantics) */
export const fr = (x: bigint | number): bigint => {
  const r = BigInt(x) % FR_MODULUS
  return r < 0n ? r + FR_MODULUS : r
}

function permute(s: bigint[]): void {
  const hf = FULL_ROUNDS / 2
  for (let r = 0; r < FULL_ROUNDS + PARTIAL_ROUNDS; r++) {
    for (let j = 0; j < SIZE; j++) s[j] = fr(s[j] + ARK[r][j])
    if (r < hf || r >= hf + PARTIAL_ROUNDS) {
      for (let j = 0; j < SIZE; j++) s[j] = fr(s[j] ** ALPHA)
    } else {
      s[0] = fr(s[0] ** ALPHA)
    }
    const next = MDS.map((row) => row.reduce((acc, m, j) => acc + m * s[j], 0n) % FR_MODULUS)
    for (let j = 0; j < SIZE; j++) s[j] = next[j]!
  }
}

/** poseidon(input elements) → one field element (arkworks squeeze(1)).
 *  Elements are raw field inputs — callers wrap negatives via fr(). */
export function poseidonToy(elems: ReadonlyArray<bigint | number>): bigint {
  const s = [0n, 0n, 0n] // [capacity | rate, rate]
  let pos = 0
  for (const e of elems) {
    if (pos === RATE) {
      permute(s)
      pos = 0
    }
    s[CAPACITY + pos] = fr(s[CAPACITY + pos] + BigInt(e))
    pos++
  }
  permute(s) // absorbing → squeezing transition
  return s[CAPACITY]
}

/** nChain circuit's model commitment: poseidon(W1 ‖ W2 ‖ b1 ‖ b2 ‖ zero_relu) */
export function modelPoseidon(w1: number[][], w2: number[][], b1: readonly number[], b2: readonly number[], zeroRelu: number): bigint {
  return poseidonToy([...w1.flat(), ...w2.flat(), ...b1, ...b2, zeroRelu])
}

/** the circuit's single public input: poseidon(input ‖ output ‖ poseidon(model)) */
export function publicStatement(input: readonly number[], output: number, model: bigint): bigint {
  return poseidonToy([...input, output, model])
}
