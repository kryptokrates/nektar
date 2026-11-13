// cryptoops.ts — the fragment-encryption custody surface extracted from
// NektarMemory: key candidates (wallet-BRC42 first, legacy agentSeed second),
// record decryption with dual-read scheme detection, and the plaintext read
// edge every catalog/market read goes through.
import { walletSigner, type SignerWallet } from '@nektar/core'
import { deriveFragmentKey, deriveLevelKey, decryptFragment, canonicalKeyID, CRYPTO_PROTOCOL_ID, type FragmentContext } from '@nektar/crypto'
import type { LogRecord } from '@nektar/log'
import type { NektarMemoryOptions } from './memory.js'

export interface CryptoOpsDeps {
  opts: NektarMemoryOptions
}

export class CryptoOps {
  constructor(private deps: CryptoOpsDeps) {}

  ctx(shard: string, seq: number, level: number): FragmentContext {
    return { t: 'nektar', psi: shard, rho: 'rec', pi: String(seq), l: level }
  }
  canEncrypt(): boolean {
    return this.deps.opts.agentSeed !== undefined || typeof (this.deps.opts.wallet as SignerWallet | undefined)?.createHmac === 'function'
  }
  /** §7 key-sale (GOAL-v7 V3): re-derive the fragment key for an encrypted
   *  record — the decryption capability being sold. Caller must ensure the
   *  record's level is within this instance's maxLevel. Wallet custody: the
   *  fragment key is a deterministic wallet hmac under the record's canonical
   *  keyID (self counterparty) — no seed bytes leave the wallet and the
   *  derived capability can still be handed over in a sale. */
  async fragmentKeyFor(rec: LogRecord): Promise<Uint8Array> {
    const ks = await this.fragmentKeyCandidates(this.ctx(rec.shard, rec.seq, rec.level ?? 0))
    if (ks.length === 0) throw new Error('fragmentKeyFor: no encryption custody (neither wallet nor agentSeed)')
    // yields the key that actually decrypts THIS record (scheme is not tagged
    // on-chain — the GCM tag discriminates); fall back to the write-scheme key.
    if (rec.fragment.E?.length) {
      const E = rec.fragment.E
      const aadLen = (E[0] << 8) | E[1]
      const ef = { ciphertext: E.slice(2 + aadLen), aad: E.slice(2, 2 + aadLen), keyID: '' }
      for (const k of ks) {
        try {
          decryptFragment(k, ef)
          return k
        } catch {
          /* try the next custody scheme */
        }
      }
    }
    return ks[0]
  }
  /** all fragment-key candidates this instance can derive, wallet scheme
   *  first (the write scheme under wallet custody), legacy seed second —
   *  dual-read: a record decrypts under exactly one (GCM tag enforces it). */
  async fragmentKeyCandidates(c: FragmentContext): Promise<Uint8Array[]> {
    const out: Uint8Array[] = []
    const w = this.deps.opts.wallet as SignerWallet | undefined
    if (w !== undefined && typeof w.createHmac === 'function') {
      const s = walletSigner(w, { protocolID: CRYPTO_PROTOCOL_ID, keyID: canonicalKeyID(c), counterparty: 'self' })
      out.push(await s.deriveSecret!('nektar:fragkey'))
    }
    if (this.deps.opts.agentSeed !== undefined) out.push(deriveFragmentKey(deriveLevelKey(this.deps.opts.agentSeed, c.l), c).fragmentKey)
    return out
  }
  /** decrypt an encrypted record — tries every custody scheme (dual-read) */
  async decryptRecord(rec: LogRecord): Promise<string | undefined> {
    const E = rec.fragment.E
    if (!E?.length) return undefined
    const aadLen = (E[0] << 8) | E[1]
    const ef = { ciphertext: E.slice(2 + aadLen), aad: E.slice(2, 2 + aadLen), keyID: '' }
    for (const k of await this.fragmentKeyCandidates(this.ctx(rec.shard, rec.seq, rec.level ?? 0))) {
      try {
        return Buffer.from(decryptFragment(k, ef)).toString('utf8')
      } catch {
        /* next scheme */
      }
    }
    return undefined
  }
  /** read a record's plaintext — decrypting E via the level key when the
   *  instance holds the seed (same path recall/disclose use). */
  async recordText(rec: LogRecord): Promise<string | undefined> {
    if (rec.fragment.d.length) return Buffer.from(rec.fragment.d).toString('utf8')
    if (rec.fragment.E?.length && this.canEncrypt()) {
      return await this.decryptRecord(rec)
    }
    return undefined
  }
}
