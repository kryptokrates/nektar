// keyops.ts — the instance's entire key-material surface, extracted from
// NektarMemory. Every raw-key vs wallet-custody branch lives here: SignerLike
// derivations for shards/log/fund, the operator wallet/signer accessors, and
// the wrapCustody edge (ECDH cannot go through a signer).
import { PrivateKey, ProtoWallet } from '@bsv/sdk'
import { custodySigner, heldRootKey, FUND_SIGN_ARGS, toSigner, type SignerLike, type DigestSigner, type SignerWallet } from '@nektar/core'
import type { WalletInterface } from '@bsv/sdk'
import type { NektarMemoryOptions, OperatorWallet } from './types.js'

export interface KeyOpsDeps {
  /** instance opts — shardKeys may be lazily created on it (matches the
   *  pre-extraction behavior; the map is shared state by design) */
  opts: NektarMemoryOptions
  /** legacy local-custody fund key — undefined under wallet custody */
  fundKey: () => PrivateKey | undefined
  /** pre-resolved shard owner pubkeys (runtime fills for wallet-mode shards) */
  shardPubs: Map<string, string>
}

export class KeyOps {
  private _fundSigner?: DigestSigner
  private _wallet: OperatorWallet | undefined

  constructor(private deps: KeyOpsDeps) {}

  /** the wallet-derived log signer (checkpoint signatures under custody) */
  walletLogSigner(): DigestSigner {
    return custodySigner(this.deps.opts.wallet as SignerWallet, { protocolID: [2, 'nektar'], keyID: 'nektar-log', counterparty: 'self' })
  }
  /** arbitrary wallet-derived signer for a nektar keyID — used by rotate to
   *  assign a new shard owner without key material crossing the API. Throws
   *  when the instance has no wallet custody. */
  walletKeySigner(keyID: string): DigestSigner {
    if (this.deps.opts.wallet === undefined) throw new Error('walletKeySigner requires wallet custody (NEKTAR_WALLET)')
    return custodySigner(this.deps.opts.wallet as SignerWallet, { protocolID: [2, 'nektar'], keyID, counterparty: 'self' })
  }

  shardKey(shard: string): SignerLike {
    const opts = this.deps.opts
    if (!opts.shardKeys) opts.shardKeys = new Map()
    if (!opts.shardKeys.has(shard)) {
      // wallet custody: a per-shard derived keyID — deterministic across
      // restarts (same wallet → same owner pubkey), zero stored key material.
      opts.shardKeys.set(
        shard,
        opts.wallet !== undefined
          ? custodySigner(opts.wallet as SignerWallet, { protocolID: [2, 'nektar'], keyID: `nektar-shard:${shard}`, counterparty: 'self' })
          : PrivateKey.fromRandom()
      )
    }
    return opts.shardKeys.get(shard)!
  }

  /** the operator fee key — throws under wallet custody (NEKTAR_WALLET=client);
   *  all signing call sites take `operatorSigner()`/`fundSigner()` instead. */
  operatorKey(): PrivateKey {
    const fundKey = this.deps.fundKey()
    if (fundKey === undefined) throw new Error('operatorKey: wallet-custody mode holds no private key — use operatorSigner()')
    return fundKey
  }

  /** the operator signing surface — the fund key when held, otherwise the
   *  wallet's identity key. Every legacy `.sign()` site routes here. */
  operatorSigner(): DigestSigner {
    return this.deps.opts.wallet !== undefined ? custodySigner(this.deps.opts.wallet as SignerWallet, { identityKey: true }) : this.fundSigner()
  }
  /** funding-signer — P2PKH inputs, covenant funding calls, change output
   *  derivation. Toolbox mode: keyDeriver root key (exact legacy custody —
   *  the app holds it). Client mode: a stable self-derived wallet keyID
   *  (nektar-fund) whose pkh the reserve funds and spends under. */
  fundSigner(): DigestSigner {
    if (this._fundSigner === undefined) {
      const w = this.deps.opts.wallet as SignerWallet | undefined
      // held-key custody (toolbox): the wallet's root IS the fee key the
      // reserve outputs lock to — derived schemes are external custody only.
      if (w !== undefined) this._fundSigner = custodySigner(w, heldRootKey(w) !== undefined ? { identityKey: true } : FUND_SIGN_ARGS)
      else if (this.deps.fundKey() !== undefined) this._fundSigner = toSigner(this.deps.fundKey()!)
      else throw new Error('no funding signer: configure a wallet or a fund key')
    }
    return this._fundSigner
  }
  /** register a pre-resolved shard owner pubkey (wallet-mode restore path) */
  registerShardPub(shard: string, pub: string): void {
    this.deps.shardPubs.set(shard, pub)
  }

  operatorWallet(): OperatorWallet {
    const fundKey = this.deps.fundKey()
    return (this._wallet ??=
      this.deps.opts.wallet ??
      (fundKey !== undefined
        ? new ProtoWallet(fundKey)
        : (() => {
            throw new Error('operatorWallet: no wallet and no fund key')
          })()))
  }
  /** disclosure-wrap custody: the wallet's BRC-42 encrypt/decrypt under
   *  wallet mode, else the raw fund key — ECDH cannot go through a signer
   *  (the wallet deliberately never exposes the shared point). */
  wrapCustody(): PrivateKey | WalletInterface {
    if (this.deps.opts.wallet !== undefined) return this.deps.opts.wallet as WalletInterface
    const fundKey = this.deps.fundKey()
    if (fundKey !== undefined) return fundKey
    throw new Error('wrapCustody: no wallet and no fund key')
  }
}
