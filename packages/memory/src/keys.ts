// MemKeys — key custody + crypto delegates: every signing surface
// (log/wallet/fund/operator) and the fragment-key/decrypt gates the record
// and read layers call. Sits lowest above MemBase because anchor-layer
// helpers (promiseViolations → logPublicKey) depend on it.
import type { PrivateKey } from '@bsv/sdk'
import { signerPubHex, type SignerLike, type DigestSigner } from '@nektar/core'
import type { LogRecord } from '@nektar/log'
import type { FragmentContext } from '@nektar/crypto'
import type { OperatorWallet } from './types.js'
import { MemBase } from './base.js'

export abstract class MemKeys extends MemBase {
  /** the log signing key's pubkey (verifies promises and checkpoints) */
  async logPublicKey(): Promise<string> {
    return (this._logPub ??= await signerPubHex(this.logKey))
  }

  /** log receipt signer under wallet custody: createSignature under a stable
   *  keyID — same pub every restart, no key material in-process. */
  protected walletLogSigner(): DigestSigner {
    return this.keyOps.walletLogSigner()
  }

  /** arbitrary wallet-derived signer for a nektar keyID — used by rotate to
   *  assign a new shard owner without key material crossing the API. Throws
   *  when the instance has no wallet custody. */
  walletKeySigner(keyID: string): DigestSigner {
    return this.keyOps.walletKeySigner(keyID)
  }

  protected shardKey(shard: string): SignerLike {
    return this.keyOps.shardKey(shard)
  }

  /** the operator fee key — throws under wallet custody (NEKTAR_WALLET=client);
   *  all signing call sites take `operatorSigner()`/`fundSigner()` instead. */
  operatorKey(): PrivateKey {
    return this.keyOps.operatorKey()
  }

  /** the operator signing surface — the fund key when held, otherwise the
   *  wallet's identity key. Every legacy `.sign()` site routes here. */
  operatorSigner(): DigestSigner {
    return this.keyOps.operatorSigner()
  }
  /** funding-signer — P2PKH inputs, covenant funding calls, change output
   *  derivation. Toolbox mode: keyDeriver root key (exact legacy custody —
   *  the app holds it). Client mode: a stable self-derived wallet keyID
   *  (nektar-fund) whose pkh the reserve funds and spends under. */
  fundSigner(): DigestSigner {
    return this.keyOps.fundSigner()
  }
  /** register a pre-resolved shard owner pubkey (wallet-mode restore path) */
  registerShardPub(shard: string, pub: string): void {
    this.keyOps.registerShardPub(shard, pub)
  }

  operatorWallet(): OperatorWallet {
    return this.keyOps.operatorWallet()
  }
  /** disclosure-wrap custody: the wallet's BRC-42 encrypt/decrypt under
   *  wallet mode, else the raw fund key — ECDH cannot go through a signer
   *  (the wallet deliberately never exposes the shared point). */
  wrapCustody(): PrivateKey | import('@bsv/sdk').WalletInterface {
    return this.keyOps.wrapCustody()
  }
  /** §7 key-sale (GOAL-v7 V3): re-derive the fragment key for an encrypted
   *  record — the decryption capability being sold. Caller must ensure the
   *  record's level is within this instance's maxLevel. Wallet custody: the
   *  fragment key is a deterministic wallet hmac under the record's canonical
   *  keyID (self counterparty) — no seed bytes leave the wallet and the
   *  derived capability can still be handed over in a sale. */
  async fragmentKeyFor(rec: LogRecord): Promise<Uint8Array> {
    return await this.cryptoOps.fragmentKeyFor(rec)
  }
  /** all fragment-key candidates this instance can derive, wallet scheme
   *  first (the write scheme under wallet custody), legacy seed second —
   *  dual-read: a record decrypts under exactly one (GCM tag enforces it). */
  protected async fragmentKeyCandidates(c: FragmentContext): Promise<Uint8Array[]> {
    return await this.cryptoOps.fragmentKeyCandidates(c)
  }
  /** decrypt an encrypted record — tries every custody scheme (dual-read) */
  protected async decryptRecord(rec: LogRecord): Promise<string | undefined> {
    return await this.cryptoOps.decryptRecord(rec)
  }
  protected canEncrypt(): boolean {
    return this.cryptoOps.canEncrypt()
  }

  protected ctx(shard: string, seq: number, level: number): FragmentContext {
    return this.cryptoOps.ctx(shard, seq, level)
  }
}
