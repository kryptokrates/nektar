// ports.ts — the ops-module slots the memory layers delegate through. These
// are PORTS: the mcp composition root injects its concrete ops classes
// (TxOps, AnchorOps, BondOps…) into slots typed here, so the memory domain
// is importable without dragging the app tier. Each interface names exactly
// the surface the layers call; an impl with a wider member set stays
// assignable as long as every signature here matches.
import type { PrivateKey, Transaction, MerklePath, WalletInterface } from '@bsv/sdk'
import type { AnchorChain, BondUtxo, CheckpointBond } from '@nektar/anchor'
import type { FundingOut, FundsPurpose, Ingested, TxFacts } from '@nektar/chain'
import type { Bytes, DigestSigner, Provenance, SignerLike } from '@nektar/core'
import type { FragmentContext } from '@nektar/crypto'
import type { Fact } from '@nektar/datalog'
import type * as idbind from '@nektar/identity'
import type { LogRecord } from '@nektar/log'
import type { CedarAction } from '@nektar/policy'
import type { ObjectRef, PacksStore } from '@nektar/store'
import type { AnchorLink, ProofBundle } from '@nektar/verify'
import type { CallerIdentity, ManifestOffer, OperatorWallet, ShardManifest, ShardPack, ZkClearance } from './types.js'

export interface TxOpsPort {
  fetchTx(txid: string): Promise<Transaction | undefined>
  rememberTx(tx: Transaction): void
  txWire(txid: string, maxBytes?: number): string | undefined
  readTxWire(hex: string): Promise<Ingested>
  onTxProof(cb: (txid: string, mp: MerklePath) => void): void
  deliverTxProof(txid: string, mp: MerklePath): void
  onTxRejected(cb: (txid: string) => void): void
  deliverTxRejected(txid: string): void
  deliverTxAbandoned(txid: string): void
  hintTxRejected(txid: string): void
  txFacts(txid: string): Promise<TxFacts>
  knownTx(txid: string): Transaction | undefined
}

export interface AnchorOpsPort {
  onAnchorQueued(cb: (txid: string, shard: string) => void): void
  anchorHead(shard: string): ReturnType<AnchorChain['getHead']>
  anchorTx(txid: string): Transaction | undefined
  tailLink(shard: string): AnchorLink | undefined
  anchorLinks(shard: string): AnchorLink[]
  unprovenAnchors(): Array<{ shard: string; txid: string }>
  attachProof(shard: string, txid: string, mp: MerklePath): void
  confirmAnchor(shard: string, txid: string): void
  anchorShard(shard: string): Promise<{ txid: string; n: number } | null>
}

export interface IdentityOpsPort {
  issueChallenge(ttlMs?: number): string
  verifiedCaller(identity?: CallerIdentity): { pubkey: string; id: string } | null
  verifyClearance(caller: { pubkey: string } | null, zk: ZkClearance | undefined, scope: string): { ok: boolean; reason?: string }
  cedarPrincipal(caller: { id: string } | null, zkOk: boolean, requiredLevel?: bigint): { type: 'Agent' | 'User'; id: string; level: number } | null
  authorizeAction(
    action: CedarAction,
    args: { identity?: CallerIdentity; zk?: ZkClearance; resource?: { id?: string; attrs?: { level?: number; shard?: string; class?: string } } }
  ): Promise<{
    policySetHash: string | null
    decision: 'allow' | 'deny' | 'not-required'
    action?: CedarAction
    reason?: string
    zkaccess?: string
  }>
}

export interface KeyOpsPort {
  walletLogSigner(): DigestSigner
  walletKeySigner(keyID: string): DigestSigner
  shardKey(shard: string): SignerLike
  operatorKey(): PrivateKey
  operatorSigner(): DigestSigner
  fundSigner(): DigestSigner
  registerShardPub(shard: string, pub: string): void
  operatorWallet(): OperatorWallet
  wrapCustody(): PrivateKey | WalletInterface
}

export interface CryptoOpsPort {
  ctx(shard: string, seq: number, level: number): FragmentContext
  canEncrypt(): boolean
  fragmentKeyFor(rec: LogRecord): Promise<Uint8Array>
  fragmentKeyCandidates(c: FragmentContext): Promise<Uint8Array[]>
  decryptRecord(rec: LogRecord): Promise<string | undefined>
  recordText(rec: LogRecord): Promise<string | undefined>
}

export interface CatalogOpsPort {
  records(shard: string): Promise<LogRecord[]>
  marketCandidates(shard: string): Promise<Array<{ seq: number; text: string }>>
  shardCatalog(): Promise<ShardManifest[]>
  shardManifest(shard: string): Promise<ShardManifest | undefined>
  contentPrice(shard: string, kind: 'read' | 'pack'): Promise<number>
  importRecord(args: {
    data: string
    provenance?: { sourcePub?: string; hash?: string; timestamp?: number; sig?: string }
    sourceShard?: string
    sourceSeq?: number
    leaf?: string
    saleTxid?: string
    shard?: string
  }): Promise<{ seq: number }>
  describeShard(shard: string, fields: { domains: string[]; desc?: string; offers?: ManifestOffer[] }): Promise<{ manifest: ShardManifest; seq: number }>
}

export interface PackOpsPort {
  storePack(shard: string, pack: ShardPack, network?: string): Promise<ObjectRef | undefined>
  packStoreRef(shard: string): ObjectRef | undefined
  attachPacksStore(store: PacksStore): void
  fetchStoredPack(ref: { bucket?: string; key?: string; sha256?: string; size?: number; chirp?: string; endpoint?: string }): Promise<ShardPack>
}

export interface PackWirePort {
  exportShardPack(shard: string): Promise<ShardPack>
  importShardPack(pack: unknown): Promise<{ imported: number; anchored: number; unverifiable: string[]; skipped: number }>
}

export type AuthorChainInfo = { genesisPub: string; certs: idbind.RotationCert[] } | null

export interface AuthorshipOpsPort {
  authorChain(shard: string): Promise<AuthorChainInfo>
  authorship(shard: string, seq: number, genesisPub: string, certs: idbind.RotationCert[]): Promise<{ ok: boolean; reason?: string }>
  rotateShard(shard: string, newKey: SignerLike): Promise<{ txid: string; n: number }>
  rotationChainProof(shard: string): Promise<{
    genesisPub: string | null
    certs: Array<{ cert: idbind.RotationCert; seq: number; data: string; prov?: Provenance; E?: Bytes; leaf: string; bundle?: ProofBundle; anchored: boolean }>
  } | null>
}

export interface ZkQueryOpsPort {
  issueZkQuery(verifierId: string, ttlMs?: number): { nonce: string; verifierId: string; queryId: string }
  consumeZkQuery(queryIdHex: string): boolean
  peekZkQuery(queryIdHex: string): { nonce: string; verifierId: string } | undefined
  zkQueryConsumed(queryIdHex: string): boolean
}

export interface AttestOpsPort {
  anchorAttestation(proofBytes: Uint8Array): Promise<{ txid: string; commitment: string }>
  attestMembership(shard: string, seq: number, queryIdHex: string): Promise<{ bytes: Uint8Array; ms: number; txid: string; commitment: string }>
  verifyAttestation(txid: string, proofBytes: Uint8Array): Promise<{ ok: boolean; reason?: string; commitment?: string; vout?: number }>
  attestInferenceOnchain(lockHex: string, unlockHex: string): Promise<{ deployTxid: string; spendTxid: string }>
}

export interface ZkProveOpsPort {
  proveMembership(shard: string, seq: number, queryIdHex: string): Promise<{ bytes: Uint8Array; ms: number }>
  reverifyMembershipBytes(bytes: Uint8Array, anchorTxid: string): Promise<{ ok: boolean; reason?: string; anchorTxid?: string }>
  verifyMembership(shard: string, seq: number, bytes: Uint8Array): Promise<{ ok: boolean; reason?: string; anchorTxid?: string; statement: string }>
  verifyCombined(
    shard: string,
    seq: number,
    membershipBytes: Uint8Array,
    zk: ZkClearance,
    opts?: { scope?: string; derivationBytes?: Uint8Array; callerPubkey?: string }
  ): Promise<{ ok: boolean; reason?: string; anchorTxid?: string; statement: string }>
  proveSubset(
    shard: string,
    seqs: number[]
  ): Promise<{ ok: true; bytes: Uint8Array; batchIndex: number; size: number; indices: number[] } | { ok: false; reason: string }>
  verifySubset(
    shard: string,
    bytes: Uint8Array
  ): Promise<{ ok: boolean; reason?: string; anchorTxid?: string; size?: number; indices?: number[]; seqs?: number[]; statement: string }>
  proveDerivation(shard: string, query: { pred: string; args: string[]; edb?: Fact[] }, queryIdHex: string): Promise<{ bytes: Uint8Array; ms: number }>
  verifyDerivation(
    shard: string,
    bytes: Uint8Array
  ): Promise<{ ok: boolean; reason?: string; policyRoot?: string; conclusionHash?: string; statement: string }>
}

export interface CustodyOpsPort {
  takeFunding(satoshis: number, purpose?: FundsPurpose): Promise<FundingOut>
  fundingStatus(): Promise<{ address: string; balanceSats: number; utxos: number; funded: boolean }>
  payInvoice(inv: { invoiceId: string; amount: number; lockingScriptHex: string; opReturn: string }): Promise<{
    payment: { invoiceId: string; transaction: string }
    txid: string
  }>
  sendSats(args: { amount: number; address?: string; scriptHex?: string; opReturn?: string; buildOnly?: boolean }): Promise<{
    txid: string
    paidSats: number
    tx?: Transaction
    txHex?: string
    utxo?: { txid: string; vout: number; satoshis: number; script: string }
  }>
  provisionPay(args: { outputs: Array<{ satoshis: number; scriptHex: string }>; address?: string; fundsSats?: number }): Promise<{
    txid: string
    hex: string
    outputs: number
    fee: number
  }>
}

export interface RevocationOpsPort {
  readonly smt: idbind.Smt
  restore(pubs: string[]): void
  snapshot(): string[]
  revoke(pubHex: string): Uint8Array
  isRevoked(pubHex: string): boolean
  revocationProof(pubHex: string): Promise<{
    seq: number
    data: string
    prov?: Provenance
    E?: Bytes
    leaf: string
    bundle?: ProofBundle
    anchored: boolean
    root?: string
    smtProof?: string
  } | null>
  revocationStatus(pubHex: string): Promise<{ revoked: boolean; root: string; matchesCurrent: boolean; smtProof: string; seq?: number; anchored: boolean }>
}

/** serialized into state.json — the bond's public constructor args + head utxo */
export interface BondSnapshot {
  txid: string
  vout: number
  satoshis: number
  script: string
  operatorPkh: string
  releaseHeight: string
  maxFee: string
  epochLen?: string
}

export interface BondOpsPort {
  readonly active: boolean
  deploy(sats?: number, releaseHeight?: bigint, force?: boolean): Promise<{ txid: string; releaseHeight: bigint }>
  status(): {
    n: number
    lastRoot: string
    headTxid: string
    sats: number
    releaseHeight: bigint
    deadline: bigint
    seized: boolean
    cfg?: { operatorPkh: string; bondReleaseHeight: bigint; maxFee: bigint; epochLen: bigint }
    utxo?: BondUtxo
  } | null
  seize(payToScript?: string, bond?: { cfg: ConstructorParameters<typeof CheckpointBond>[0]; utxo: BondUtxo }): Promise<{ txid: string } | null>
  commit(root: string): Promise<{ txid: string; n: bigint } | null>
  close(): Promise<{ txid: string } | null>
  renewIfDue(): Promise<'committed' | 'skipped' | 'failed'>
  mirror(lastRoot: string): Promise<boolean>
  snapshot(): BondSnapshot | undefined
  restore(s: BondSnapshot): void
}

export interface ProofStorePort {
  readonly stores: Map<string, { endpoint: string; bucket: string }>
  put(bytes: Uint8Array): string
  get(commitHex: string): Uint8Array | undefined
  ref(commitHex: string): { endpoint: string; bucket: string } | undefined
  fetch(commitHex: string, store?: { endpoint?: string; bucket?: string }): Promise<Uint8Array | undefined>
}

export interface MetricsPort {
  inc(name: string, labels?: Record<string, string>, by?: number, help?: string): void
  observe(name: string, v: number, help?: string): void
  gauge(name: string, fn: () => number, help?: string): void
  value(name: string, labels?: Record<string, string>): number
  render(): string
}
