// Client kit: the cryptography an AGENT HARNESS does on behalf of a model.
// The model only decides *what* to recall; the wallet proves *who* asks and
// that the caller is cleared — keys never enter the model's context.
//   const wallet = new ClientWallet()
//   const identity = await wallet.identify(call)           // challenge → signed answer
//   const zk = wallet.presentClearance(cred, 'shard', seq, 2n)
//   await call('recall', { shard, seq, identity, zk })
import { randomBytes } from 'node:crypto'
import { PrivateKey } from '@bsv/sdk'
import { signBinding, answerChallenge, issueClearance, proveAccess, encodePresentation, encodeCredential, decodeCredential, type ClearanceCredential, type ClearancePresentationWire } from '@nektar/identity'
import type { CallerIdentity } from './memory.js'

export type ToolCall = (name: string, args: Record<string, unknown>) => Promise<any>

export class ClientWallet {
  readonly key: PrivateKey
  readonly uuid: string
  constructor (key = PrivateKey.fromRandom(), uuid = 'agent') { this.key = key; this.uuid = uuid }
  get pubkey (): string { return this.key.toPublicKey().toString() }
  /** answer a fresh challenge issued by the server (only server nonces are accepted) */
  async identify (call: ToolCall): Promise<CallerIdentity> {
    const { nonce } = await call('challenge', {}) as { nonce: string }
    return { pubkey: this.pubkey, binding: signBinding(this.key, this.uuid, '00'.repeat(32), 0), challengeNonce: nonce, challengeSig: answerChallenge(this.key, nonce) }
  }
  /** a single-use Bulletproof that this holder's credential covers `requiredLevel`
   *  for exactly shard/seq (fresh transcript each time) */
  presentClearance (cred: ClearanceCredential, shard: string, seq: number, requiredLevel: bigint): ClearancePresentationWire {
    if (cred.commitment.holder !== this.pubkey) throw new Error('credential was issued to a different holder')
    const transcript = { queryId: randomBytes(8).toString('hex'), fragmentId: `${shard}/${seq}`, nonce: Uint8Array.from(randomBytes(16)) }
    const proof = proveAccess(cred, requiredLevel, 8, transcript)
    if (!proof) throw new Error(`credential level is below ${requiredLevel}`)
    return encodePresentation({ commitment: cred.commitment, proof, transcript, requiredLevel })
  }
}

/** clearance authority (trusted by the server via NEKTAR_CLEARANCE_AUTHORITIES) */
export class ClearanceAuthority {
  readonly key: PrivateKey
  constructor (key = PrivateKey.fromRandom()) { this.key = key }
  get pubkey (): string { return this.key.toPublicKey().toString() }
  issue (holderPub: string, level: bigint): ClearanceCredential { return issueClearance(this.key, level, holderPub) }
}
export { encodeCredential, decodeCredential }
