// ZkQuery — one nonce/verifier pair feeding BOTH the membership proof and the
// ZKAccess proof (π_access binds its own queryId; the two MUST match in
// verifyCombined).
import { dsha256, utf8 } from '@mamem/core'
import { BN254_R } from './codec.js'

export interface ZkQuery { nonce: Uint8Array, verifierId: string }

// queryId = sha256('mamem:zkq:v1' ‖ verifierNonce ‖ verifierId) mod p
export function queryIdOf (q: ZkQuery): bigint {
  const h = dsha256(Buffer.concat([utf8('mamem:zkq:v1'), q.nonce, utf8(q.verifierId)]))
  return BigInt('0x' + Buffer.from(h).toString('hex')) % BN254_R
}
