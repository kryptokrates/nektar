// §8 delta encoding: Δ(K_i, K_j) as the single replaced span between two
// texts (common prefix/suffix kept). Deterministic and invertible:
//   apply(a, Δ) = b   and   revert(b, Δ) = a
// so the full lineage is recoverable from any end of a revision chain.
export interface Delta { pre: number, suf: number, del: string, ins: string }

export class DeltaError extends Error {}

export function diff (a: string, b: string): Delta {
  const A = [...a], B = [...b] // code points, never split a surrogate pair
  let pre = 0
  while (pre < A.length && pre < B.length && A[pre] === B[pre]) pre++
  let suf = 0
  while (suf < A.length - pre && suf < B.length - pre && A[A.length - 1 - suf] === B[B.length - 1 - suf]) suf++
  return { pre, suf, del: A.slice(pre, A.length - suf).join(''), ins: B.slice(pre, B.length - suf).join('') }
}

function splice (s: string, d: Delta, expect: string, put: string): string {
  const S = [...s]
  if (d.pre + d.suf > S.length) throw new DeltaError('delta does not fit its base')
  const mid = S.slice(d.pre, S.length - d.suf).join('')
  if (mid !== expect) throw new DeltaError('delta base mismatch')
  return S.slice(0, d.pre).join('') + put + S.slice(S.length - d.suf).join('')
}
/** K_j = Apply(K_i, Δ) — throws DeltaError if Δ was not made from this base */
export const apply = (a: string, d: Delta): string => splice(a, d, d.del, d.ins)
/** K_i = Revert(K_j, Δ) */
export const revert = (b: string, d: Delta): string => splice(b, d, d.ins, d.del)

export const encodeDelta = (d: Delta): string => JSON.stringify([d.pre, d.suf, d.del, d.ins])
export function decodeDelta (s: string): Delta {
  const v = JSON.parse(s) as unknown
  if (!Array.isArray(v) || v.length !== 4 || !Number.isInteger(v[0]) || !Number.isInteger(v[1]) || v[0] < 0 || v[1] < 0 || typeof v[2] !== 'string' || typeof v[3] !== 'string') throw new DeltaError('malformed delta')
  return { pre: v[0], suf: v[1], del: v[2], ins: v[3] }
}
