// Receipt token + on-the-fly verification — the "scan this answer" surface.
//
// Token: `mamemr1.<base64url receiptHash>` — a ~51-char pointer a verifier
// resolves against the issuer's /v1/verify endpoint (or the verifyReceipt
// tool). The heavy data stays server-side: the __receipts__ record carries
// `receipt:<hash>:<b64url receipt>` so the check can re-hash and inspect
// the committed payload, not just its digest.
//
// What verification means here (honest scope): the receipt existed, was
// provenance-signed by the log (recall's COM-003 check), was anchored, and
// its cited fragments are anchored. It does NOT say the answer was true —
// the receipt itself carries TRUTH_NOTE saying exactly that.
import { toHex, fromHex } from '@mamem/core'
import { hashOf } from '@mamem/reasoning'
import { paymentsEnabled } from './pay.js'
import type { MamemMemory } from './memory.js'

export const RECEIPT_TOKEN_PREFIX = 'mamemr1.'

export function receiptToken (receiptHashHex: string): string {
  return RECEIPT_TOKEN_PREFIX + Buffer.from(fromHex(receiptHashHex)).toString('base64url')
}

/** `mamemr1.<b64url 32B>` → hash hex; also accepts a bare 64-hex hash. */
export function parseReceiptToken (tok: string): { receiptHash: string } | null {
  const t = (tok ?? '').trim()
  if (/^[0-9a-fA-F]{64}$/.test(t)) return { receiptHash: t.toLowerCase() }
  if (!t.startsWith(RECEIPT_TOKEN_PREFIX)) return null
  try {
    const b = Buffer.from(t.slice(RECEIPT_TOKEN_PREFIX.length), 'base64url')
    if (b.length !== 32) return null
    return { receiptHash: toHex(new Uint8Array(b)) }
  } catch { return null }
}

export interface ReceiptCheck { name: string, ok: boolean | 'info' | 'skip', detail: string }
export interface VerifyReceiptResult {
  ok: boolean
  receiptHash: string
  checks: ReceiptCheck[]
  receipt?: unknown
}

/** Decode a token, find the receipt record on __receipts__ via recall (the
 *  decrypt+provenance path — records may be encrypted at rest), and run the
 *  traffic-light checks: found → body-hash → provenance → anchored → each
 *  cited fragment anchored. `ok` = every hard check passed. */
export async function verifyReceipt (mem: MamemMemory, arg: { token?: string, receiptHash?: string }): Promise<VerifyReceiptResult> {
  const parsed = arg.token ? parseReceiptToken(arg.token) : (arg.receiptHash ? { receiptHash: arg.receiptHash.toLowerCase() } : null)
  if (!parsed) throw new Error('verifyReceipt: pass token (mamemr1.…) or receiptHash (64-hex)')
  const checks: ReceiptCheck[] = []

  const items = (await mem.recall('__receipts__')).items
  const rec = items.find(i => i.text?.startsWith(`receipt:${parsed.receiptHash}`))
  if (!rec) return { ok: false, receiptHash: parsed.receiptHash, checks: [{ name: 'found', ok: false, detail: `no record receipt:${parsed.receiptHash.slice(0, 16)}… on __receipts__` }] }
  checks.push({ name: 'found', ok: true, detail: `record on __receipts__ at seq ${rec.seq}` })

  const bodyB64 = rec.text!.split(':')[2]
  let receipt: { fragmentRefs?: Array<{ shard: string, seq: number }>, checkResult?: string, groundingReportHash?: string } | undefined
  if (bodyB64) {
    try {
      receipt = JSON.parse(Buffer.from(bodyB64, 'base64url').toString('utf8'))
      const ok = hashOf(receipt) === parsed.receiptHash
      checks.push({ name: 'hash', ok, detail: ok ? 'stored body re-hashes to the token' : 'body hash MISMATCH — record tampered' })
      if (!ok) return { ok: false, receiptHash: parsed.receiptHash, checks, receipt }
    } catch { checks.push({ name: 'hash', ok: false, detail: 'record body is not parseable receipt JSON' }) }
  } else {
    checks.push({ name: 'hash', ok: 'info', detail: 'hash-only record (pre-body format) — payload not re-checkable' })
  }

  // recall already re-verified the originator provenance signature on the
  // decrypted plaintext (COM-003) — unverifiable means it failed.
  checks.push(rec.unverifiable
    ? { name: 'provenance', ok: false, detail: `originator signature failed: ${rec.reason ?? 'unverifiable'}` }
    : { name: 'provenance', ok: true, detail: 'originator signature verifies (COM-003, via recall)' })
  if (rec.unverifiable) return { ok: false, receiptHash: parsed.receiptHash, checks, receipt }

  const st = await mem.anchorStatus('__receipts__', rec.seq)
  checks.push({
    name: 'anchored',
    ok: rec.state === 'anchored',
    detail: rec.state === 'anchored'
      ? `anchor tx — ${st.state}, ${st.confirmations} conf (depth ${st.depth})`
      : `record state "${rec.state}" — not yet anchored`
  })

  if (receipt?.fragmentRefs?.length) {
    for (const ref of receipt.fragmentRefs) {
      const f = (await mem.recall(ref.shard, { seq: ref.seq })).items[0]
      const fst = f ? await mem.anchorStatus(ref.shard, ref.seq) : undefined
      checks.push({
        name: `fragment ${ref.shard}:${ref.seq}`,
        ok: !!f && !f.unverifiable && f.state === 'anchored',
        detail: !f ? 'cited record NOT FOUND' : `state "${f.state}"${f.unverifiable ? `, unverifiable (${f.reason ?? ''})` : ''}, ${fst!.state} ${fst!.confirmations}conf`
      })
    }
  } else {
    checks.push({ name: 'fragments', ok: 'info', detail: 'receipt cites no fragments' })
  }
  if (receipt?.groundingReportHash) checks.push({ name: 'grounding', ok: 'info', detail: `grounding report committed (${receipt.groundingReportHash.slice(0, 16)}…)` })
  if (receipt?.checkResult) checks.push({ name: 'checkResult', ok: 'info', detail: receipt.checkResult })

  // paid mode: the receipt commits the settlement txid — "was this answer paid
  // for?" is part of what a paid-service receipt claims to prove.
  const pay = (receipt as { payment?: { txid: string, amountSats: number, payTo: string } } | undefined)?.payment
  if (pay) {
    checks.push({ name: 'paid', ok: true, detail: `${pay.amountSats} sats settled — tx ${pay.txid.slice(0, 20)}… pays ${pay.payTo}; auditable on the public chain` })
  } else if (paymentsEnabled()) {
    checks.push({ name: 'paid', ok: false, detail: 'payments are enabled but this receipt commits no settlement txid' })
  } else {
    checks.push({ name: 'paid', ok: 'info', detail: 'unpaid (MAMEM_PAYMENTS off)' })
  }

  const ok = checks.every(c => c.ok === true || c.ok === 'info' || c.ok === 'skip')
  return { ok, receiptHash: parsed.receiptHash, checks, receipt }
}
