// Receipt token + on-the-fly verification — the "scan this answer" surface.
//
// Token: `nektarr1.<base64url receiptHash>` — a ~51-char pointer a verifier
// resolves against the issuer's /v1/verify endpoint (or the verifyReceipt
// tool). The heavy data stays server-side: the __receipts__ record carries
// `receipt:<hash>:<b64url receipt>` so the check can re-hash and inspect
// the committed payload, not just its digest.
//
// What verification means here (honest scope): the receipt existed, was
// provenance-signed by the log (recall's COM-003 check), was anchored, and
// its cited fragments are anchored. It does NOT say the answer was true —
// the receipt itself carries TRUTH_NOTE saying exactly that.
import { toHex, fromHex } from '@nektar/core'
import { hashOf } from '@nektar/reasoning'
import { paymentsEnabled } from './pay.js'
import type { NektarMemory } from './memory.js'

const RECEIPT_TOKEN_PREFIX = 'nektarr1.'

export function receiptToken(receiptHashHex: string): string {
  return RECEIPT_TOKEN_PREFIX + Buffer.from(fromHex(receiptHashHex)).toString('base64url')
}

/** `nektarr1.<b64url 32B>` → hash hex; also accepts a bare 64-hex hash. */
function parseReceiptToken(tok: string): { receiptHash: string } | null {
  const t = (tok ?? '').trim()
  if (/^[0-9a-fA-F]{64}$/.test(t)) return { receiptHash: t.toLowerCase() }
  if (!t.startsWith(RECEIPT_TOKEN_PREFIX)) return null
  try {
    const b = Buffer.from(t.slice(RECEIPT_TOKEN_PREFIX.length), 'base64url')
    if (b.length !== 32) return null
    return { receiptHash: toHex(new Uint8Array(b)) }
  } catch {
    return null
  }
}

interface ReceiptCheck {
  name: string
  ok: boolean | 'info' | 'skip'
  detail: string
}
export interface VerifyReceiptResult {
  ok: boolean
  receiptHash: string
  checks: ReceiptCheck[]
  receipt?: unknown
  /** txid of the anchor transaction holding the receipt record — lets the
   *  verify page link straight to the on-chain proof (block explorer). */
  anchorTxid?: string
}

/** Decode a token, find the receipt record on __receipts__ via recall (the
 *  decrypt+provenance path — records may be encrypted at rest), and run the
 *  traffic-light checks: found → body-hash → provenance → anchored → each
 *  cited fragment anchored. `ok` = every hard check passed. */
export async function verifyReceipt(mem: NektarMemory, arg: { token?: string; receiptHash?: string }): Promise<VerifyReceiptResult> {
  const parsed = arg.token ? parseReceiptToken(arg.token) : arg.receiptHash ? { receiptHash: arg.receiptHash.toLowerCase() } : null
  if (!parsed) throw new Error('verifyReceipt: pass token (nektarr1.…) or receiptHash (64-hex)')
  const checks: ReceiptCheck[] = []

  const items = (await mem.recall('__receipts__')).items
  const rec = items.find((i) => i.text?.startsWith(`receipt:${parsed.receiptHash}`))
  if (!rec)
    return {
      ok: false,
      receiptHash: parsed.receiptHash,
      checks: [{ name: 'found', ok: false, detail: `no record receipt:${parsed.receiptHash.slice(0, 16)}… on __receipts__` }]
    }
  checks.push({ name: 'found', ok: true, detail: `record on __receipts__ at seq ${rec.seq}` })

  const bodyB64 = rec.text!.split(':')[2]
  let receipt: { fragmentRefs?: Array<{ shard: string; seq: number }>; checkResult?: string; groundingReportHash?: string } | undefined
  if (bodyB64) {
    try {
      receipt = JSON.parse(Buffer.from(bodyB64, 'base64url').toString('utf8'))
      const ok = hashOf(receipt) === parsed.receiptHash
      checks.push({ name: 'hash', ok, detail: ok ? 'stored body re-hashes to the token' : 'body hash MISMATCH — record tampered' })
      if (!ok) return { ok: false, receiptHash: parsed.receiptHash, checks, receipt }
    } catch {
      checks.push({ name: 'hash', ok: false, detail: 'record body is not parseable receipt JSON' })
    }
  } else {
    checks.push({ name: 'hash', ok: 'info', detail: 'hash-only record (pre-body format) — payload not re-checkable' })
  }

  // recall already re-verified the originator provenance signature on the
  // decrypted plaintext (COM-003) — unverifiable means it failed.
  checks.push(
    rec.unverifiable
      ? { name: 'provenance', ok: false, detail: `originator signature failed: ${rec.reason ?? 'unverifiable'}` }
      : { name: 'provenance', ok: true, detail: 'originator signature verifies (COM-003, via recall)' }
  )
  if (rec.unverifiable) return { ok: false, receiptHash: parsed.receiptHash, checks, receipt }

  const st = await mem.anchorStatus('__receipts__', rec.seq)
  const anchorTxid = (await mem.records('__receipts__')).find((r) => r.seq === rec.seq)?.anchorTxid
  checks.push({
    name: 'anchored',
    ok: rec.state === 'anchored',
    detail:
      rec.state === 'anchored' ? `anchor tx — ${st.state}, ${st.confirmations} conf (depth ${st.depth})` : `record state "${rec.state}" — not yet anchored`
  })

  if (receipt?.fragmentRefs?.length) {
    for (const ref of receipt.fragmentRefs as Array<{
      shard?: string
      seq?: number
      private?: true
      hdp?: string
      anchorTxid?: string
      proofCommit?: string
      proofStore?: { endpoint?: string; bucket?: string }
    }>) {
      if (ref.private === true) {
        // private shard: the cite is redacted — when the receipt commits
        // proofCommit and the proof is fetchable, the STARK is RE-RUN here:
        // independently verifiable, not service-attested. (The hdp itself
        // stays unverifiable by design — ZK hides which leaf it was.)
        // proofStore makes fetchable mean resolvable off-origin: the receipt
        // carries the bstore endpoint/bucket and chirp/<commit> is the key.
        const atx = ref.anchorTxid ? mem.anchorTx(ref.anchorTxid) : undefined
        const proof = ref.proofCommit ? await mem.fetchProof(ref.proofCommit, ref.proofStore) : undefined
        if (proof !== undefined && ref.anchorTxid !== undefined) {
          const rv = await mem.reverifyMembershipBytes(proof, ref.anchorTxid)
          checks.push({
            name: 'fragment (private)',
            ok: rv.ok,
            detail: rv.ok
              ? `ZK proof re-verified — sha256 ${ref.proofCommit!.slice(0, 16)}…; shard withheld; anchor ${ref.anchorTxid.slice(0, 16)}… proven on-chain`
              : `ZK re-verify failed: ${rv.reason} (proof ${ref.proofCommit!.slice(0, 16)}…)`
          })
        } else {
          checks.push({
            name: 'fragment (private)',
            ok: ref.proofCommit === undefined ? !!atx?.merklePath : false,
            detail:
              ref.proofCommit === undefined
                ? atx?.merklePath
                  ? `ZK-verified at commit — shard withheld; anchor ${ref.anchorTxid!.slice(0, 16)}… proven on-chain (no proof committed — attested only)`
                  : 'private cite — no proven anchor for the committed ref'
                : `proof ${ref.proofCommit.slice(0, 16)}… not fetchable — cannot re-verify (committed but artifact missing)`
          })
        }
        continue
      }
      const f = (await mem.recall(ref.shard!, { seq: ref.seq! })).items[0]
      const fst = f ? await mem.anchorStatus(ref.shard!, ref.seq!) : undefined
      checks.push({
        name: `fragment ${ref.shard}:${ref.seq}`,
        ok: !!f && !f.unverifiable && f.state === 'anchored',
        detail: !f
          ? 'cited record NOT FOUND'
          : `state "${f.state}"${f.unverifiable ? `, unverifiable (${f.reason ?? ''})` : ''}, ${fst!.state} ${fst!.confirmations}conf`
      })
    }
  } else {
    checks.push({ name: 'fragments', ok: 'info', detail: 'receipt cites no fragments' })
  }
  if (receipt?.groundingReportHash)
    checks.push({ name: 'grounding', ok: 'info', detail: `grounding report committed (${receipt.groundingReportHash.slice(0, 16)}…)` })
  if (receipt?.checkResult) checks.push({ name: 'checkResult', ok: 'info', detail: receipt.checkResult })

  // paid mode: the receipt commits the settlement txid — "was this answer paid
  // for?" is part of what a paid-service receipt claims to prove.
  const pay = (receipt as { payment?: { txid: string; amountSats: number; payTo: string } } | undefined)?.payment
  if (pay) {
    checks.push({
      name: 'paid',
      ok: true,
      detail: `${pay.amountSats} sats settled — tx ${pay.txid.slice(0, 20)}… pays ${pay.payTo}; auditable on the public chain`
    })
  } else if (paymentsEnabled()) {
    checks.push({ name: 'paid', ok: false, detail: 'payments are enabled but this receipt commits no settlement txid' })
  } else {
    checks.push({ name: 'paid', ok: 'info', detail: 'unpaid (NEKTAR_PAYMENTS off)' })
  }

  // model attestation (nChain ai_inference Groth16): four states —
  //   absent                → declared model only (modelId, unproven)
  //   present + valid       → statement re-derived (statement-verified)
  //   + attestationTxid     → sha256(proof) anchored in an OP_RETURN tx; the
  //                         proof bytes are re-fetched and the commitment
  //                         re-checked (attested+anchored)
  //   + verifierTxid        → the in-script verifier spend (consensus-checked)
  //   present + invalid     → hard fail: an attestation that doesn't re-derive is a lie
  const att = (
    receipt as
      | {
          modelAttestation?: {
            modelPoseidon: string
            input?: number[]
            output: string | number
            publicStatement: string
            proofCommit?: string
            proofStore?: { endpoint?: string; bucket?: string }
            attestationTxid?: string
            verifierTxid?: string
            circuit?: string
          }
        }
      | undefined
  )?.modelAttestation
  if (att) {
    const { validateAttestation } = await import('@nektar/zk')
    const v = att.input
      ? validateAttestation({ modelPoseidon: att.modelPoseidon, input: att.input, output: att.output, publicStatement: att.publicStatement })
      : { ok: false, reason: 'attestation carries no input — statement not re-derivable' }
    let detail = v.ok
      ? `statement re-derived ✓ — poseidon(input‖output‖poseidon(model)) binds ${att.circuit ?? 'circuit'} (statement ${att.publicStatement.slice(0, 18)}…)`
      : `attestation INVALID: ${v.reason}`
    let attOk = v.ok
    if (v.ok && att.verifierTxid) {
      detail += mem.knownTx(att.verifierTxid)
        ? `; verifier spend ${att.verifierTxid.slice(0, 16)}… resolved — consensus-checked Groth16 verification`
        : `; verifierTxid ${att.verifierTxid.slice(0, 16)}… claimed — not resolvable by this instance (exists on the public chain or not)`
    } else if (v.ok && att.attestationTxid) {
      // anchored commitment: fetch the exact proof bytes the attestation
      // binds (proof store serves /v1/zkproof/<commit>) and re-check
      const bytes = att.proofCommit ? await mem.fetchProof(att.proofCommit, att.proofStore) : undefined
      if (!bytes) {
        attOk = false
        detail += `; attestationTxid ${att.attestationTxid.slice(0, 16)}… claimed but proof bytes unavailable (proofCommit ${att.proofCommit?.slice(0, 16) ?? 'missing'}…) — cannot re-check the anchored commitment`
      } else {
        const va = await mem.verifyAttestation(att.attestationTxid, bytes)
        detail += va.ok
          ? `; attestation ${att.attestationTxid.slice(0, 16)}… anchors sha256(proof) ✓ — attested+anchored`
          : `; attestation ${att.attestationTxid.slice(0, 16)}… does NOT carry sha256(proof): ${va.reason}`
        if (!va.ok) attOk = false
      }
    } else if (v.ok) {
      detail += '; statement-verified only — no on-chain commitment'
    }
    checks.push({ name: 'model', ok: attOk, detail })
  } else {
    checks.push({
      name: 'model',
      ok: 'info',
      detail: `declared model only (${(receipt as { modelId?: { provider?: string; release?: string } } | undefined)?.modelId?.provider ?? 'undeclared'}/${(receipt as { modelId?: { release?: string } } | undefined)?.modelId?.release ?? 'undeclared'}) — no execution attestation`
    })
  }

  const ok = checks.every((c) => c.ok === true || c.ok === 'info' || c.ok === 'skip')
  return { ok, receiptHash: parsed.receiptHash, checks, receipt, anchorTxid }
}
