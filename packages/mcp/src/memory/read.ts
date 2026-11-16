// MemRead — the served read path: recall (gates → SPV → decrypt → receipt)
// and the doctor/doctorRepair health surface. Also hosts the receipt helpers
// — canonicalJson, receiptDigest and the two exported receipt verifiers.
import { existsSync } from 'node:fs'
import { execFile } from 'node:child_process'
import { PublicKey, Signature } from '@bsv/sdk'
import { utf8, toHex, dsha256, concat, field, TAG_RECEIPT_SIG, toSigner, nektarEnv, type Bytes } from '@nektar/core'
import { verifyProvenance } from '@nektar/log'
import { decryptFragment, shredKey } from '@nektar/crypto'
import * as idbind from '@nektar/identity'
import type { AnswerReceipt, CallerIdentity, ZkClearance } from '../types.js'
import { MemRecords } from './records.js'

/** recursive canonical JSON (sorted keys) — same scheme as reasoning/canonicalJson
 *  so digest bindings are stable across verifiers. */
function canonicalJson(v: unknown): string {
  if (v === null || typeof v !== 'object') return JSON.stringify(v)
  if (Array.isArray(v)) return `[${v.map(canonicalJson).join(',')}]`
  return `{${Object.keys(v as Record<string, unknown>)
    .sort()
    .map((k) => `${JSON.stringify(k)}:${canonicalJson((v as Record<string, unknown>)[k])}`)
    .join(',')}}`
}

/** §18: the signed receipt digest — every receipt field except signature.
 *  Explicit field order (no JSON ambiguity): answer, each ref
 *  (shard,seq,hdp,txid), checkResult, truthNote, modelId, zkaccess, cedar,
 *  dag triple, delegation map, zk artifact, timestamp. */
function receiptDigest(r: Omit<AnswerReceipt, 'signature' | 'signerPub'>): Bytes {
  const refs = r.fragmentRefs.map((f) => concat(utf8(f.shard), utf8(String(f.seq)), utf8(f.hdp), utf8(f.anchorTxid ?? '')))
  const dagBytes = r.dag ? concat(utf8(r.dag.policyRoot), utf8(r.dag.derivationHash ?? ''), utf8(r.dag.state)) : new Uint8Array(0)
  const delBytes = concat(...(r.delegations ?? []).map((d) => concat(utf8(d.delegator), utf8(d.decision), utf8(d.signature))))
  const zkBytes = r.zk ? utf8(canonicalJson(r.zk)) : new Uint8Array(0)
  const modelBytes = r.model ? utf8(canonicalJson(r.model)) : new Uint8Array(0)
  const t = Buffer.alloc(8)
  t.writeBigUInt64LE(BigInt(r.timestamp ?? 0))
  return dsha256(
    concat(
      TAG_RECEIPT_SIG,
      field(utf8(r.answer)),
      field(concat(...refs)),
      field(utf8(r.checkResult)),
      field(utf8(r.truthNote)),
      field(utf8(r.modelId)),
      field(utf8(r.zkaccess ?? '')),
      field(utf8(r.cedar ?? '')),
      field(dagBytes),
      field(delBytes),
      field(zkBytes),
      field(modelBytes),
      Uint8Array.from(t)
    )
  )
}

/** §13: re-verify the receipt's ZKAccess artifact — the Π_j a third party
 *  checks rather than trusting the `zkaccess` flag. Fails when the artifact is
 *  absent (nothing to re-verify), malformed, or the proof doesn't hold. */
export function verifyReceiptZk(r: Pick<AnswerReceipt, 'zk'>, opts?: { trustedAuthorities?: string[]; holder?: string }): { ok: boolean; reason?: string } {
  if (!r.zk) return { ok: false, reason: 'no zk artifact on receipt' }
  let p: import('@nektar/identity').ClearancePresentation
  try {
    p = idbind.decodePresentation(r.zk)
  } catch (e) {
    return { ok: false, reason: `malformed zk artifact: ${(e as Error).message}` }
  }
  return idbind.verifyAccess(p.commitment, p.requiredLevel, p.proof, p.transcript, { trustedAuthorities: opts?.trustedAuthorities, holder: opts?.holder })
    ? { ok: true }
    : { ok: false, reason: 'clearance proof does not verify' }
}

/** verify a receipt's terminal signature against its embedded signerPub */
export function verifyAnswerReceipt(r: AnswerReceipt): { ok: boolean; reason?: string } {
  if (!r.signature || !r.signerPub) return { ok: false, reason: 'unsigned receipt' }
  const { signature, signerPub, ...unsigned } = r
  try {
    const ok = PublicKey.fromString(signerPub).verify([...receiptDigest(unsigned)], Signature.fromDER(signature, 'hex'))
    return ok ? { ok: true } : { ok: false, reason: 'signature does not bind this receipt to the signer' }
  } catch (e) {
    return { ok: false, reason: `malformed signature: ${(e as Error).message}` }
  }
}

export abstract class MemRead extends MemRecords {
  /**
   * recall: fetch fragments for a shard (optionally only anchored), decrypt
   * when an agentSeed is held, and attach a receipt.
   */
  async recall(
    shard: string,
    opts?: {
      seq?: number
      question?: string
      /** ZKAccess gate: a clearance proof covering `requiredLevel` is checked
       * before any level-gated ciphertext is decrypted. Keys alone still enforce
       * confidentiality; the proof additionally gates the READ PATH. */
      zk?: ZkClearance
      /** Verified caller identity (challenge-response against a nonce issued by
       * THIS instance via issueChallenge): the Cedar principal id and the
       * clearance holder — never a self-asserted claim. */
      identity?: CallerIdentity
      /** CHN-006: withhold records whose anchor has fewer than finalityDepth confirmations */
      requireFinal?: boolean
      /** §16 delegation map: sub-decisions the caller asserts were made by other
       * agents (each = makeDelegation(key, decision, answer)). Bound into the
       * signed receipt so the map is anchored, not post-hoc. */
      delegations?: Array<{ delegator: string; decision: string; signature: string }>
      /** per-call invocation params merged into receipt.model.params (the
       * caller knows temperature/seed; the operator owns id + weightsHash) */
      modelParams?: Record<string, unknown>
    }
  ): Promise<{ items: Array<{ seq: number; state: string; text?: string; unverifiable?: boolean; reason?: string; zk?: string }>; receipt: AnswerReceipt }> {
    const single = opts?.seq !== undefined ? await this.store.bySeq(shard, opts.seq) : undefined
    const items = opts?.seq !== undefined ? (single ? [single] : []) : await this.store.all(shard)
    const caller = this.verifiedCaller(opts?.identity)
    let zkState: AnswerReceipt['zkaccess'] = 'not-required'
    let zkOk = false
    let zkReason: string | undefined
    if (opts?.zk) {
      // a clearance transcript is scoped to one fragment (seq given) or the shard
      const v = this.verifyClearance(caller, opts.zk, opts.seq !== undefined ? `${shard}/${opts.seq}` : shard)
      zkOk = v.ok
      zkReason = v.reason
      zkState = zkOk ? 'verified' : 'rejected'
    }
    const provenLevel = zkOk ? opts!.zk!.requiredLevel : 0n
    // Cedar enforcement is an INSTANCE property — no policy option in the
    // request, no caller-supplied policy text or hash, no bypass.
    let cedarState: 'allow' | 'deny' | 'hash-mismatch' | 'not-required' = 'not-required'
    const policy = this.opts.policy
    if (policy) {
      const { authorize, canonicalizePolicySet } = await import('@nektar/policy')
      // hash the INSTANCE policy text ourselves — never trust a request hash
      const computed = canonicalizePolicySet('instance', policy.policyText).hash
      if (computed !== policy.anchoredPolicySetHash) {
        cedarState = 'hash-mismatch'
      } else {
        // principal identity must be VERIFIED: id = hash160 of the bound pubkey,
        // level = the ZK proof's proven bound (requiredLevel), not a claim.
        const principal = this.cedarPrincipal(caller, zkOk, opts?.zk?.requiredLevel)
        if (!principal) {
          cedarState = 'deny' // no verified identity → nothing served
        } else {
          cedarState = 'allow'
          for (const r of items) {
            const d = authorize(policy.policyText, principal, 'read', {
              type: 'Fragment',
              id: `${shard}/${r.seq}`,
              attrs: { level: r.level ?? 0, shard, class: 'fragment' }
            })
            if (!d.allow) {
              cedarState = 'deny'
              break
            }
          }
        }
      }
    }
    // Policy-DAG gate (POL-004/008/016): when the instance configures a DAG,
    // the read is allowed only when Cedar ALLOWs AND the DAG derives permit
    // with no conflict. The DAG + its EDB come from the instance and the
    // VERIFIED caller facts below — never from the request.
    let dagState: string | undefined
    let dagPolicyRoot: string | undefined
    let dagDerivationHash: string | undefined
    const dagPermit = new Map<number, boolean>() // per-fragment DAG permit
    const dag = this.opts.policyDag
    if (dag) {
      const { Datalog } = await import('@nektar/datalog')
      // mirror the v2 Cedar fix: the configured DAG must match the anchored
      // root decoded from the DAG-root record — a mismatch denies everything
      const recomputed = Buffer.from(dag.policyRoot()).toString('hex')
      dagPolicyRoot = recomputed
      if (this.opts.anchoredPolicyRoot !== undefined && this.opts.anchoredPolicyRoot !== recomputed) {
        dagState = 'dag-deny:policy DAG root mismatch'
        for (const r of items) dagPermit.set(r.seq, false)
      } else {
        const facts = caller
          ? [
              { pred: 'member', args: [caller.id, 'verified'] },
              ...(zkOk ? [{ pred: 'level', args: [caller.id, String(provenLevel)] }] : []),
              ...items.map((r) => ({ pred: 'fragment', args: [`${shard}/${r.seq}`] }))
            ]
          : []
        const { facts: derived, conflicts } = dag.evaluate(facts)
        dagDerivationHash = new Datalog(dag.rules).derivHash(derived)
        if (conflicts.length > 0) {
          dagState = `dag-deny:policy conflict on ${conflicts[0].claim}`
          for (const r of items) dagPermit.set(r.seq, false)
        } else {
          // per-item: each fragment needs its own permit; non-permitted items
          // are withheld (consistent with Cedar per-item denial)
          let denied = 0
          for (const r of items) {
            const ok = caller != null && (cedarState === 'allow' || cedarState === 'not-required') && derived.has(`permit(${caller.id},read,${shard}/${r.seq})`)
            dagPermit.set(r.seq, ok)
            if (!ok) denied++
          }
          dagState = denied === 0 ? 'dag-allow' : `dag-deny:${denied} item(s) lack DAG permit`
        }
      }
      // global deny only for conflicts/root mismatch (ambiguous or untrusted
      // policy); per-item non-permits are withheld individually below
      if (/conflict|mismatch/.test(dagState)) cedarState = 'deny'
    }
    const out: AnswerReceipt['fragmentRefs'] = []
    const texts: Array<{ seq: number; state: string; text?: string; unverifiable?: boolean; reason?: string; zk?: string }> = []
    // S6.1: any item whose SPV verification fails flips the whole receipt to
    // 'unverifiable' — anchor presence alone never earns 'verified'
    let verifyFailed = false
    for (const r of items) {
      const ref = { shard, seq: r.seq, hdp: toHex(r.hdp), anchorTxid: r.anchorTxid }
      out.push(ref)
      const item: { seq: number; state: string; text?: string; unverifiable?: boolean; reason?: string; zk?: string } = { seq: r.seq, state: r.state }
      if (opts?.zk) item.zk = zkOk ? 'verified' : 'rejected'
      const level = BigInt(r.level ?? 0)
      const fin = opts?.requireFinal ? await this.anchorStatus(shard, r.seq) : undefined
      // §19 author guard: revocation + epoch-window authorship, evaluated once
      // per record before the serve guards. Local check uses the live SMT;
      // third parties use revocationProof/revocationStatus — SMT membership
      // proofs verified against roots embedded in anchored `revoke:` records.
      const authorDeny = r.authorPub ? await this.authorGuard(shard, r) : undefined
      if (fin && fin.state !== 'final') {
        item.unverifiable = true
        item.reason = `not final: ${fin.confirmations}/${fin.depth} confirmations`
      } else if (level > 0n && provenLevel < level) {
        // ZKA gate — applies to EVERY level>0 record, plaintext or encrypted:
        // nothing is served without a valid clearance proof covering its level
        item.unverifiable = true
        item.reason = zkOk
          ? `zkaccess: proven level ${provenLevel} < ${level}`
          : `zkaccess ${opts?.zk ? 'rejected' : 'required'}${zkReason ? `: ${zkReason}` : ''}`
        item.zk = 'rejected'
        zkState = 'rejected'
      } else if (dagPermit.get(r.seq) === false) {
        // policy-DAG per-item deny — withhold this fragment only
        item.unverifiable = true
        item.reason = 'dag deny'
      } else if (cedarState !== 'allow' && cedarState !== 'not-required') {
        // Cedar: DENY or a policy-hash mismatch — never serve anything
        item.unverifiable = true
        item.reason = `cedar ${cedarState}`
      } else if (authorDeny) {
        item.unverifiable = true
        item.reason = authorDeny
      } else if (r.fragment.E && r.fragment.E.length > 0) {
        // KEY-008: decrypt ONLY after verifyFragment succeeds for this record.
        const { result } = await this.prove(shard, r.seq)
        if (!result.ok) {
          item.unverifiable = true
          item.reason = `verify: ${(result as any).reason}`
          verifyFailed = true
        } else if (this.canEncrypt()) {
          try {
            const buf = Buffer.from(r.fragment.E)
            const aadLen = buf.readUInt16BE(0)
            const ef = {
              ciphertext: new Uint8Array(buf.subarray(2 + aadLen)),
              keyID: '',
              aad: new Uint8Array(buf.subarray(2, 2 + aadLen))
            }
            // try each granted level key — disjoint keyspaces mean only the
            // level the fragment was written at will pass GCM auth.
            for (let l = 0; l <= (this.opts.maxLevel ?? 0); l++) {
              for (const fragmentKey of await this.fragmentKeyCandidates(this.ctx(shard, r.seq, l))) {
                let pt: Uint8Array
                try {
                  pt = decryptFragment(fragmentKey, ef)
                } catch {
                  continue
                }
                // defence in depth: the key level that decrypted must also be
                // covered by the proven clearance (the record's level was
                // already checked above)
                if (BigInt(l) > provenLevel) {
                  item.unverifiable = true
                  item.reason = `zkaccess: key level ${l} > proven ${provenLevel}`
                  item.zk = 'rejected'
                  zkState = 'rejected'
                } else {
                  // COM-003 strict audit: re-verify the originator signature
                  // over the DECRYPTED plaintext (it was bound at ingest).
                  if (r.authorPub && r.fragment.p.signature?.length && !verifyProvenance(r.authorPub, pt, r.fragment.p)) {
                    item.unverifiable = true
                    item.reason = 'provenance sig invalid post-decrypt'
                  } else {
                    item.text = Buffer.from(pt).toString('utf8')
                    await this.logAccess({ event: 'decrypt', shard, seq: r.seq, level: l, caller: opts?.identity?.pubkey })
                  }
                }
                shredKey(fragmentKey)
                break
              }
            }
            if (item.text === undefined && item.reason === undefined) {
              item.unverifiable = true
              item.reason = 'no held level key decrypts'
            }
          } catch {
            item.unverifiable = true
            item.reason = 'decrypt failed'
          }
        } else {
          item.unverifiable = true
          item.reason = 'no encryption custody held'
        }
      } else {
        // S6.1: plaintext items run the SAME SPV verification as encrypted
        // ones (verify only — there is no decrypt step). An unanchored or
        // failed record is withheld and marked, never served as verified.
        const { result } = await this.prove(shard, r.seq)
        if (!result.ok) {
          item.unverifiable = true
          item.reason = `verify: ${(result as any).reason}`
          verifyFailed = true
        } else {
          item.text = r.fragment.d.length ? Buffer.from(r.fragment.d).toString('utf8') : undefined
        }
      }
      if (item.unverifiable)
        this.metrics.inc(
          'nektar_rejected_reads_total',
          { reason: (item.reason ?? 'unknown').split(/[:\d]/)[0].trim() },
          1,
          'records withheld from a read, by reason'
        )
      texts.push(item)
    }
    const answer = texts.map((i) => i.text ?? '[unverifiable]').join('\n')
    const verified = out.length === 0 || (out.every((r) => r.anchorTxid) && !verifyFailed)
    const unsigned: Omit<AnswerReceipt, 'signature' | 'signerPub'> = {
      answer,
      fragmentRefs: out,
      checkResult: verified ? 'verified' : 'unverifiable',
      truthNote:
        'inclusion and anchor-chain integrity verified; the TRUTH of the recorded statements is NOT established — a lie, once written, is preserved and marked',
      modelId: this.opts.model?.id ?? this.opts.modelId ?? 'unknown',
      model:
        this.opts.model || opts?.modelParams
          ? {
              id: this.opts.model?.id ?? this.opts.modelId ?? 'unknown',
              params: { ...this.opts.model?.params, ...opts?.modelParams },
              weightsHash: this.opts.model?.weightsHash
            }
          : undefined,
      zkaccess: zkState,
      zk: opts?.zk
        ? idbind.encodePresentation({
            commitment: opts.zk.commitment,
            proof: opts.zk.proof,
            transcript: opts.zk.transcript,
            requiredLevel: opts.zk.requiredLevel
          })
        : undefined,
      cedar: cedarState,
      dag: dagState && dagPolicyRoot ? { policyRoot: dagPolicyRoot, derivationHash: dagDerivationHash, state: dagState } : undefined,
      delegations: opts?.delegations,
      timestamp: Date.now()
    }
    // §18 terminal commitment: σ = Sign_logKey(H(answer‖trace‖τ))
    const receipt: AnswerReceipt = {
      ...unsigned,
      signerPub: await this.logPublicKey(),
      signature: (await toSigner(this.logKey).sign([...receiptDigest(unsigned)])).toDER('hex') as string
    }
    return { items: texts, receipt }
  }

  /** doctor: one-call health surface — funding, shard/record/anchor counts,
   *  unproven anchors, payments, prover availability. The tool layer may
   *  replace `funding` with the richer ops.wallet() view when present. */
  async doctor(): Promise<{
    network: string
    /** mem-side reserve view by default; the server swaps in ops.wallet()
     *  (spend caps, runway) on live networks — hence the open shape */
    funding: Record<string, unknown>
    shards: number
    records: number
    anchoredRecords: number
    unprovenAnchors: number
    importedBundles: number
    publicPacks: number
    payments: 'on' | 'off'
    prover: string
    bond: { n: number; lastRoot: string; headTxid: string; sats: number; releaseHeight: string; deadline: string; seized: boolean } | null
  }> {
    const shards = this.shards()
    let records = 0,
      anchoredRecords = 0
    for (const s of shards)
      for (const r of await this.store.all(s)) {
        records++
        if (r.anchorTxid !== undefined) anchoredRecords++
      }
    const proverBin = nektarEnv('NEKTAR_STARK_PROVER')
    const prover = proverBin
      ? existsSync(proverBin)
        ? 'native binary'
        : 'NEKTAR_STARK_PROVER set but file missing'
      : await new Promise<string>((res) => {
          execFile('docker', ['image', 'inspect', nektarEnv('NEKTAR_STARK_IMAGE') ?? 'nektar-stark-build'], { timeout: 5_000 }, (e) =>
            res(e ? 'absent (set NEKTAR_STARK_PROVER or build nektar-stark-build)' : 'docker image')
          )
        })
    return {
      network: this.opts.network ?? 'main',
      funding: await this.fundingStatus(),
      shards: shards.length,
      records,
      anchoredRecords,
      unprovenAnchors: this.unprovenAnchors().length,
      importedBundles: this.importedBundles.size,
      publicPacks: (await this.publicShards()).length,
      payments: /^(1|true|yes)$/i.test(nektarEnv('NEKTAR_PAYMENTS') ?? '') ? 'on' : 'off',
      prover,
      bond: (() => {
        const b = this.bondStatus()
        return b ? { ...b, releaseHeight: b.releaseHeight.toString(), deadline: b.deadline.toString() } : null
      })()
    }
  }

  /** doctorRepair: the fix half of `doctor{fix:true}` — applies only repairs
   *  that cannot lose state. Anything needing a spend or a judgment call is
   *  reported as `refused` with a reason, never attempted silently. */
  async doctorRepair(): Promise<{ fixed: string[]; refused: string[] }> {
    const fixed: string[] = [],
      refused: string[] = []
    const dir = this.opts.dataDir
    if (!dir) {
      refused.push('no dataDir on this instance — nothing to repair')
      return { fixed, refused }
    }
    const { existsSync, readFileSync, rmSync, writeFileSync, mkdirSync } = await import('node:fs')
    if (!existsSync(dir)) {
      mkdirSync(dir, { recursive: true })
      fixed.push(`recreated missing data dir ${dir}`)
    }
    // instance.lock — refuse only when a *live* heartbeat owns it
    const lock = (await import('node:path')).join(dir, 'instance.lock')
    if (existsSync(lock)) {
      let l: { owner?: string; beat?: number } | null = null
      try {
        l = JSON.parse(readFileSync(lock, 'utf8'))
      } catch {
        l = null
      }
      const live = l?.beat !== undefined && Date.now() - l.beat < 20_000
      if (live) refused.push(`instance.lock held by live instance ${l?.owner} — stop it first, do not delete`)
      else {
        rmSync(lock)
        fixed.push(l === null ? 'removed corrupt instance.lock' : `removed stale instance.lock (beat ${Math.round((Date.now() - (l?.beat ?? 0)) / 1000)}s old)`)
      }
    }
    // data-dir VERSION marker — establishes the migration baseline (v1)
    const ver = (await import('node:path')).join(dir, 'VERSION')
    if (!existsSync(ver)) {
      writeFileSync(ver, '1')
      fixed.push('wrote VERSION=1 (migration baseline)')
    }
    // unproven anchors need a spend — report, never fix silently
    const unproven = this.unprovenAnchors().length
    if (unproven > 0) refused.push(`${unproven} unproven anchor(s) — re-anchoring spends sats; handle deliberately`)
    return { fixed, refused }
  }
}
