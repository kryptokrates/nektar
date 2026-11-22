// MemZk — verified callers, clearance, ZK prove/verify, attestations and the
// proof store. Also owns the replay-protection persistence pair
// (persistSecurityState/restoreSecurityState) since every map it saves is
// challenge/ZK/revocation state.
import type { CallerIdentity, ZkClearance } from './types.js'
import { MemPacks } from './packs.js'

export abstract class MemZk extends MemPacks {
  protected persistSecurityState(): void {
    this.opts.securityStore?.save({
      challenges: [...this.challenges].filter(([, exp]) => exp >= Date.now()),
      zkSeen: [...this.zkSeen],
      // consumed tombstones persist past expiry — they are the re-verify
      // ledger: a proof whose queryId was consumed IS the committed one.
      zkQueries: [...this.zkQueries]
        .filter(([, e]) => e.expiry >= Date.now() || e.consumed === true)
        .map(([q, e]) => [q, e.expiry, e.nonce, e.verifierId, e.consumed === true]),
      revocations: this.revocationOps.snapshot()
    })
  }
  protected restoreSecurityState(): void {
    const s = this.opts.securityStore?.load()
    if (!s) return
    for (const [n, exp] of s.challenges) this.challenges.set(n, exp)
    for (const k of s.zkSeen) this.zkSeen.add(k)
    for (const row of s.zkQueries) this.zkQueries.set(row[0], { expiry: row[1], nonce: row[2] ?? '', verifierId: row[3] ?? '', consumed: row[4] === true })
    this.revocationOps.restore(s.revocations)
  }
  /** verifier side: issue a single-use ZK membership query (default 10 min).
   *  queryId = dsha256('nektar:zkq:v2' ‖ nonce ‖ verifierId); the prover binds it into the STARK journal. */
  issueZkQuery(verifierId: string, ttlMs = 600_000): { nonce: string; verifierId: string; queryId: string } {
    return this.zkQueryOps.issueZkQuery(verifierId, ttlMs)
  }
  protected consumeZkQuery = (queryIdHex: string): boolean => this.zkQueryOps.consumeZkQuery(queryIdHex)
  /** issued-and-fresh query WITHOUT consuming it — verifyCombined checks the
   *  §13 Π binding first, consumes once at the end (single use stays). */
  protected peekZkQuery(queryIdHex: string): { nonce: string; verifierId: string } | undefined {
    return this.zkQueryOps.peekZkQuery(queryIdHex)
  }
  /** re-verify registry: a queryId passes iff it was CONSUMED — i.e. this
   *  exact proof already verified once (consume runs last, only after all
   *  other checks held). The tombstone survives expiry + restart. */
  protected zkQueryConsumed = (queryIdHex: string): boolean => this.zkQueryOps.zkQueryConsumed(queryIdHex)

  // ── verified callers (IDN-003) and clearance (ZKA-001..006) ──────────────
  /** The VERIFIER issues the challenge nonce; only nonces issued by this
   *  instance and still fresh (default 5 min) authenticate a caller. */
  issueChallenge(ttlMs = 300_000): string {
    return this.identityOps.issueChallenge(ttlMs)
  }
  /** caller identity from VERIFIED inputs only: the binding is for this
   *  pubkey, the nonce was issued here and is fresh, the answer is signed.
   *  id = hash160(pubkey). Anything else → null (no principal → DENY). */
  verifiedCaller(identity?: CallerIdentity): { pubkey: string; id: string } | null {
    return this.identityOps.verifiedCaller(identity)
  }
  /** ZK clearance gate: issuer ∈ instance-trusted authorities, credential
   *  bound to the verified caller, transcript scoped to THIS read and unused. */
  verifyClearance(caller: { pubkey: string } | null, zk: ZkClearance | undefined, scope: string): { ok: boolean; reason?: string } {
    return this.identityOps.verifyClearance(caller, zk, scope)
  }
  /** Cedar principal: verified id; level = the ZK-PROVEN bound, else 0. */
  protected cedarPrincipal(caller: { id: string } | null, zkOk: boolean, requiredLevel?: bigint): { type: 'Agent' | 'User'; id: string; level: number } | null {
    return this.identityOps.cedarPrincipal(caller, zkOk, requiredLevel)
  }

  /** authorize an action for the MCP `authorize` tool with the same verified
   *  inputs recall uses (never caller-asserted ids or levels). */
  async authorizeAction(
    action: import('@nektar/policy').CedarAction,
    args: { identity?: CallerIdentity; zk?: ZkClearance; resource?: { id?: string; attrs?: { level?: number; shard?: string; class?: string } } }
  ) {
    return await this.identityOps.authorizeAction(action, args)
  }

  /** holder side: STARK membership proof bound to a verifier-issued queryId
   *  (Wright §13 — contract/mechanism documented on ZkProveOps). */
  async proveMembership(shard: string, seq: number, queryIdHex: string): Promise<{ bytes: Uint8Array; ms: number }> {
    return await this.zkProveOps.proveMembership(shard, seq, queryIdHex)
  }

  /** anchor sha256(proofBytes) in an OP_FALSE OP_RETURN data tx funded by the
   *  fee key (~1 sat of fee at regtest rates). Returns the txid + commitment. */
  async anchorAttestation(proofBytes: Uint8Array): Promise<{ txid: string; commitment: string }> {
    return await this.attestOps.anchorAttestation(proofBytes)
  }

  /** holder side (§13/S1a): produce the STARK membership proof AND anchor its
   *  commitment on-chain. The queryId stays unconsumed — the verifier's own
   *  verifyMembership call still succeeds (single use is its privilege). */
  async attestMembership(shard: string, seq: number, queryIdHex: string): Promise<{ bytes: Uint8Array; ms: number; txid: string; commitment: string }> {
    return await this.attestOps.attestMembership(shard, seq, queryIdHex)
  }

  /** verifier side: fetch the attestation tx, extract the OP_RETURN hash and
   *  check it equals sha256(supplied proof bytes). Only txs broadcast by THIS
   *   *  instance are resolvable (chain fetch by txid is an ops gap, CHN-*). */
  async verifyAttestation(txid: string, proofBytes: Uint8Array): Promise<{ ok: boolean; reason?: string; commitment?: string; vout?: number }> {
    return await this.attestOps.verifyAttestation(txid, proofBytes)
  }

  /** verifier side: anchored-STARK verification against THIS shard's
   *  canonical chain; the queryId is ours, single use (ZkProveOps). */
  async verifyMembership(shard: string, seq: number, bytes: Uint8Array): Promise<{ ok: boolean; reason?: string; anchorTxid?: string; statement: string }> {
    return await this.zkProveOps.verifyMembership(shard, seq, bytes)
  }

  /** deploy an in-script Groth16 verifier (lock) and spend it with the proof
   *  (unlock) — consensus itself runs the verification. The spend txid is the
   *  attestation a receipt cites: it exists on-chain or it doesn't. */
  async attestInferenceOnchain(lockHex: string, unlockHex: string): Promise<{ deployTxid: string; spendTxid: string }> {
    return await this.attestOps.attestInferenceOnchain(lockHex, unlockHex)
  }

  // ── public proof store + private-cite re-verification ────────────────────
  // proofs committed inside receipts are public artifacts, served via
  // GET /v1/proof/<sha256> — storage, locator and integrity rules live in
  // ProofStore (canonical docs there).
  /** store a proof artifact; returns its sha256 commitment hex */
  putProof(bytes: Uint8Array): string {
    return this.proofStore.put(bytes)
  }
  /** fetch a stored proof by commitment — digest-checked (ProofStore). */
  getProof(commitHex: string): Uint8Array | undefined {
    return this.proofStore.get(commitHex)
  }
  /** the {endpoint,bucket} locator a receipt carries so a remote verifier
   *  needs no origin call (ProofStore). */
  proofStoreRef(commitHex: string): { endpoint: string; bucket: string } | undefined {
    return this.proofStore.ref(commitHex)
  }
  /** proof bytes by commitment — local dir first, then the locator a receipt
   *  carried; digest-checked (ProofStore). */
  async fetchProof(commitHex: string, store?: { endpoint?: string; bucket?: string }): Promise<Uint8Array | undefined> {
    return await this.proofStore.fetch(commitHex, store)
  }

  /** re-verify a committed private-cite proof WITHOUT consuming — tombstone
   *  registry check: this exact proof must already have verified once
   *  (ZkProveOps). */
  async reverifyMembershipBytes(bytes: Uint8Array, anchorTxid: string): Promise<{ ok: boolean; reason?: string; anchorTxid?: string }> {
    return await this.zkProveOps.reverifyMembershipBytes(bytes, anchorTxid)
  }

  /** verifier side: §13 Π_j = (π_j, ZKAccess) as ONE verifiable object —
   *  same-query binding across clearance/membership/derivation, single-use
   *  consume at the end (ZkProveOps). */
  async verifyCombined(
    shard: string,
    seq: number,
    membershipBytes: Uint8Array,
    zk: ZkClearance,
    opts?: { scope?: string; derivationBytes?: Uint8Array; callerPubkey?: string }
  ): Promise<{ ok: boolean; reason?: string; anchorTxid?: string; statement: string }> {
    return await this.zkProveOps.verifyCombined(shard, seq, membershipBytes, zk, opts)
  }

  /** holder side: selective subset-disclosure proof (L_ZKDis v2) over one
   *  anchored batch — reveals positions + leaf hashes, never contents
   *  (ZkProveOps). */
  async proveSubset(
    shard: string,
    seqs: number[]
  ): Promise<{ ok: true; bytes: Uint8Array; batchIndex: number; size: number; indices: number[] } | { ok: false; reason: string }> {
    return await this.zkProveOps.proveSubset(shard, seqs)
  }

  /** verifier side: claimed batch root → OUR anchored link, then the same
   *  shard/canonical/SPV checks + multiproof fold (ZkProveOps). */
  async verifySubset(
    shard: string,
    bytes: Uint8Array
  ): Promise<{ ok: boolean; reason?: string; anchorTxid?: string; size?: number; indices?: number[]; seqs?: number[]; statement: string }> {
    return await this.zkProveOps.verifySubset(shard, bytes)
  }

  /** holder side: §14 derivation STARK under the anchored policy DAG —
   *  journal = (policyRoot, conclusionHash, queryId) (ZkProveOps). */
  async proveDerivation(
    shard: string,
    query: { pred: string; args: string[]; edb?: import('@nektar/datalog').Fact[] },
    queryIdHex: string
  ): Promise<{ bytes: Uint8Array; ms: number }> {
    return await this.zkProveOps.proveDerivation(shard, query, queryIdHex)
  }

  /** verifier side: proven policyRoot must be this verifier's anchored DAG
   *  root; queryId ours, single use (ZkProveOps). */
  async verifyDerivation(
    shard: string,
    bytes: Uint8Array
  ): Promise<{ ok: boolean; reason?: string; policyRoot?: string; conclusionHash?: string; statement: string }> {
    return await this.zkProveOps.verifyDerivation(shard, bytes)
  }
}
