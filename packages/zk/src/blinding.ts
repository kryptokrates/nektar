// Blinding derivation — deterministic per fragment from the owner key,
// BRC-42-style key id 'mamem zkblind <fragmentId>', reduced mod BN254 p.
// Delivered to authorized readers via the existing key-wrap/disclosure path —
// this API is what a holder calls to get its blinding.
import { PrivateKey } from '@bsv/sdk'
import { deriveFragmentKey } from '@mamem/crypto'
import { BN254_R } from './codec.js'

export function blindingOf (ownerKey: PrivateKey, fragmentId: string): bigint {
  const { fragmentKey } = deriveFragmentKey(ownerKey as any, { t: 'mamem', psi: 'zkblind', rho: 'blind', pi: fragmentId, l: 0 } as any)
  return BigInt('0x' + Buffer.from(fragmentKey).toString('hex')) % BN254_R
}
