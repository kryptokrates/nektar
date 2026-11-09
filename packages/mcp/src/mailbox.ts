// F6 outbound-only exchange — agents need no inbound port. Requests, offers,
// acceptances, and delivery notices travel as signed envelopes through any
// mailbox keeper (the rv's /mailbox, or a peer keeper's). The keeper sees the
// envelope metadata; the payload is ECDH-encrypted to the recipient's key.
// This removes the single-server dependency: any surviving keeper serves.
import { randomUUID } from 'node:crypto'
import { sha256, toHex, utf8 } from '@nektar/core'
import { wrapFor, unwrapFor } from '@nektar/identity'
import { signEnvelope, verifyEnvelope } from '@nektar/overlay'
import type { PrivateKey } from '@bsv/sdk'

export interface CollectedMessage {
  messageId: string
  sender: string
  kind: string
  payload: unknown // the decrypted JSON payload
  createdAt: number
  conversationId?: string
  operationId?: string
}

const MB_CTX = 'nektar:mailbox:v1'

/** encrypt + sign + POST an envelope to a keeper (rv or peer mailbox) */
export async function sendEnvelope (
  key: PrivateKey, keeperUrl: string,
  fields: { recipient: string, kind: 'request' | 'offer' | 'acceptance' | 'delivery-reference' | 'acknowledgement' | 'cancellation', payload: unknown, conversationId?: string, operationId?: string, replyTo?: string, expiresAt?: number }
): Promise<{ messageId: string }> {
  const plaintext = utf8(JSON.stringify(fields.payload))
  const wrapped = wrapFor(key, fields.recipient, plaintext, MB_CTX)
  const ciphertext = toHex(Uint8Array.from(Buffer.concat([Buffer.from(wrapped.salt), Buffer.from(wrapped.nonce), Buffer.from(wrapped.ct)])))
  const ciphertextHash = toHex(Uint8Array.from(sha256(plaintext)))
  const se = signEnvelope(key, {
    messageId: randomUUID(), kind: fields.kind, recipient: fields.recipient,
    // topic-addressed — the keeper sees an opaque hash, not the recipient pub
    topic: toHex(Uint8Array.from(sha256(utf8(`mb:${fields.recipient}`)))),
    conversationId: fields.conversationId, operationId: fields.operationId, replyTo: fields.replyTo,
    createdAt: Date.now(), expiresAt: fields.expiresAt, ciphertextHash, ciphertext, encryptionProfile: 'ecdh+aes-256-gcm'
  })
  const r = await fetch(`${keeperUrl.replace(/\/+$/, '')}/mailbox`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(se) })
  if (!r.ok) throw new Error(`mailbox send → ${r.status}`)
  return { messageId: se.envelope.messageId }
}

/** poll a keeper for our envelopes, decrypt each, dedup by messageId */
export async function collect (
  key: PrivateKey, keeperUrl: string, seen: Set<string> = new Set()
): Promise<CollectedMessage[]> {
  const me = key.toPublicKey().toString()
  const topic = toHex(Uint8Array.from(sha256(utf8(`mb:${me}`))))
  const r = await fetch(`${keeperUrl.replace(/\/+$/, '')}/mailbox?recipient=${encodeURIComponent(me)}&topic=${topic}`)
  const j = await r.json().catch(() => ({}))
  const out: CollectedMessage[] = []
  for (const se of j.envelopes ?? []) {
    const e = se.envelope
    if (!verifyEnvelope(e, se.sig)) continue
    if (seen.has(e.messageId)) continue
    seen.add(e.messageId)
    const raw = Buffer.from(e.ciphertext, 'hex')
    const w = { salt: raw.subarray(0, 32), nonce: raw.subarray(32, 44), ct: raw.subarray(44) }
    try {
      const pt = unwrapFor(key, e.sender, w, MB_CTX, w.ct.length - 16) // ct−tag = plaintext len; unwrapFor defaults to 32B key length which truncates payloads
      if (toHex(Uint8Array.from(sha256(pt))) !== e.ciphertextHash) continue // tampered payload
      out.push({ messageId: e.messageId, sender: e.sender, kind: e.kind, payload: JSON.parse(Buffer.from(pt).toString('utf8')), createdAt: e.createdAt, conversationId: e.conversationId, operationId: e.operationId })
    } catch { /* not decryptable by us or corrupt — skip */ }
  }
  return out
}
