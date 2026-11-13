// revocations.ts — IDN-007 revocation registry extracted from NektarMemory.
// The SMT is keyed by derive_key(pubkey); every revoke inserts and persists,
// and §19 third-party proofs read the __revocation__ shard + its inclusion
// bundles so a verifier checks "key X was revoked" against chain data alone.
import * as idbind from '@nektar/identity'
import { fromHex, toHex, utf8, sha256, type Bytes } from '@nektar/core'

export interface RevocationOpsDeps {
  store: { all(shard: string): Promise<import('@nektar/log').LogRecord[]> }
  /** encrypted deployments: the operator seed decrypts revocation records;
   *  plaintext records are read directly */
  canEncrypt: () => boolean
  decryptRecord: (rec: import('@nektar/log').LogRecord) => Promise<string | undefined>
  prove: (shard: string, seq: number) => Promise<{ bundle?: import('@nektar/verify').ProofBundle; result: { ok?: boolean } }>
  /** persists the shared security blob — challenges/zk/revocations are one
   *  file, so this module only asks the owner to save */
  persistSecurityState: () => void
}

export class RevocationOps {
  /** revocation registry (IDN-007): SMT keyed by derive_key(pubkey); its root
   *  is anchored with every revoke event */
  readonly smt = new idbind.Smt()
  private revokedPubs: string[] = []
  constructor(private deps: RevocationOpsDeps) {}

  /** restore persisted revocations (boot): re-insert so the SMT root rebuilds
   *  deterministically and the pub list round-trips on the next save */
  restore(pubs: string[]): void {
    for (const pub of pubs) {
      this.smt.insert(idbind.deriveKey(fromHex(pub)), sha256(utf8('revoked')))
      this.revokedPubs.push(pub)
    }
  }
  /** pubs as persisted in the shared security-state blob */
  snapshot(): string[] {
    return [...this.revokedPubs]
  }

  /** revoke a 33-byte pubkey: SMT key = derive_key(pubkey); persisted */
  revoke(pubHex: string): Uint8Array {
    const pk = fromHex(pubHex)
    if (pk.length !== 33) throw new Error('revoke: pubkey must be a 33-byte compressed public key')
    const root = this.smt.insert(idbind.deriveKey(pk), sha256(utf8('revoked')))
    if (!this.revokedPubs.includes(pubHex)) this.revokedPubs.push(pubHex)
    this.deps.persistSecurityState()
    return root
  }
  isRevoked(pubHex: string): boolean {
    return this.smt.has(idbind.deriveKey(fromHex(pubHex)))
  }

  /** §19 third-party revocation proof: the `__revocation__` shard record for
   *  `revoke:<pub>` plus its SPV-verifiable inclusion bundle — an external
   *  verifier can check "key X was revoked" against chain data alone rather
   *  than trusting an operator's local revocation list. Returns the record's
   *  plaintext `data`, its `prov` and `E` so the verifier can recompute
   *  `leaf = computeLeaf({d,p,E})` and bind the TEXT to the proven leaf —
   *  an inclusion proof for a DIFFERENT record is not evidence. */
  async revocationProof(pubHex: string): Promise<{
    seq: number
    data: string
    prov?: import('@nektar/core').Provenance
    E?: Bytes
    leaf: string
    bundle?: import('@nektar/verify').ProofBundle
    anchored: boolean
    root?: string
    smtProof?: string
  } | null> {
    for (const rec of await this.deps.store.all('__revocation__')) {
      let data: string
      if (rec.fragment.d.length) data = Buffer.from(rec.fragment.d).toString('utf8')
      else if (rec.fragment.E?.length && this.deps.canEncrypt()) {
        // encrypted deployment — the operator holds the seed; decrypt to read
        const t = await this.deps.decryptRecord(rec)
        if (t === undefined) continue
        data = t
      } else continue
      const entry = idbind.parseRevocationRecord(data)
      if (!entry || entry.pk.toLowerCase() !== pubHex.toLowerCase()) continue
      const { bundle, result } = await this.deps.prove('__revocation__', rec.seq)
      return {
        seq: rec.seq,
        data,
        prov: rec.fragment.p,
        E: rec.fragment.E,
        leaf: toHex(rec.leaf),
        bundle,
        anchored: result.ok === true,
        root: entry.root,
        smtProof: entry.proof ? idbind.serializeSmtProof(entry.proof) : undefined
      }
    }
    return null
  }

  /** §19 CRL status: is `pubHex` revoked, provable against the LATEST anchored
   *  SMT root? The newest `revoke:` record's embedded root equals the current
   *  SMT root only when no revoke happened after it — `matchesCurrent` says so;
   *  a verifier recomputes via idbind.verifyRevocationStatus(pk, root, proof)
   *  which accepts both membership (revoked) and non-membership proofs. */
  async revocationStatus(
    pubHex: string
  ): Promise<{ revoked: boolean; root: string; matchesCurrent: boolean; smtProof: string; seq?: number; anchored: boolean }> {
    const key = idbind.revocationKey(pubHex)
    const smtProof = idbind.serializeSmtProof(this.smt.prove(key))
    const current = toHex(this.smt.root())
    let root = '',
      seq: number | undefined,
      anchored = false
    for (const rec of await this.deps.store.all('__revocation__')) {
      if (!rec.fragment.d.length) continue
      const data = Buffer.from(rec.fragment.d).toString('utf8')
      const entry = idbind.parseRevocationRecord(data)
      if (!entry) continue
      const { result } = await this.deps.prove('__revocation__', rec.seq)
      root = entry.root
      seq = rec.seq
      anchored = result.ok === true
    }
    return { revoked: this.smt.has(key), root, matchesCurrent: root === current, smtProof, seq, anchored }
  }
}
