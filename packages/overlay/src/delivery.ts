// BRC-149 wire forms for the presence feed — one implementation, no duplicate
// framing: the binary submission record (ingress), the delivery record
// (egress), and the 92-byte FrameVer 0x09 multicast frame. Presence artifacts
// ride as their canonical JSON object; on-chain submits ride as real BEEF.
import { sha256 } from '@nektar/core'

const utf8 = (s: string) => new TextEncoder().encode(s)
const dsha256 = (b: Uint8Array | number[]) => sha256(Uint8Array.from(sha256(Uint8Array.from(b))))

export const MAGIC = 0xE3E1F3E8
export const PROTO_VER = 0x02BF
export const FRAME_VER_OBJECT = 0x09
export const RECORD_TAG = 0xBEEF

/** TopicID = SHA-256(UTF-8 topic name) — the shard key */
export const topicId = (topic: string) => sha256(utf8(topic))

/** ContentID = SHA-256d(payload bytes) */
export const contentId = (payload: Uint8Array | number[]) => dsha256(payload)

/** BRC-149 submission record (ingress): the pair (topic list, object). */
export function submissionRecord (topics: string[], object: Uint8Array | number[]): number[] {
  if (topics.length < 1 || topics.length > 15) throw new Error('submissionRecord: topics 1..15')
  const out: number[] = [0xBE, 0xEF, 0x01, topics.length]
  for (const t of topics) {
    const n = utf8(t)
    if (n.length > 64) throw new Error('submissionRecord: topic name >64B')
    out.push(n.length, ...n)
  }
  const len = object.length
  out.push((len >>> 24) & 0xff, (len >>> 16) & 0xff, (len >>> 8) & 0xff, len & 0xff, ...object)
  return out
}

/** BRC-149 delivery record (egress): TopicID + PayloadLen + payload verbatim. */
export function deliveryRecord (topic: string, payload: Uint8Array | number[]): number[] {
  const id = topicId(topic)
  return [...id, (payload.length >>> 24) & 0xff, (payload.length >>> 16) & 0xff, (payload.length >>> 8) & 0xff, payload.length & 0xff, ...payload]
}

/** BRC-149 FrameVer 0x09 — the 92-byte multicast frame + payload.
 *  deliverCount: how many leading topics are deliverable (open ingress = 1).
 *  seqNum/hashKey: flow fields, 0 when unstamped (ingress stamps them). */
export function beefFrame (payload: Uint8Array | number[], topic: string, opts: { deliverCount?: number, seqNum?: number | bigint, hashKey?: number | bigint } = {}): number[] {
  const frame = new Uint8Array(92)
  const dv = new DataView(frame.buffer)
  dv.setUint32(0, MAGIC)
  dv.setUint16(4, PROTO_VER)
  frame[6] = FRAME_VER_OBJECT
  frame[7] = opts.deliverCount ?? 1
  frame.set(contentId(payload), 8)
  dv.setBigUint64(40, BigInt(opts.hashKey ?? 0))
  dv.setBigUint64(48, BigInt(opts.seqNum ?? 0))
  frame.set(topicId(topic), 56)
  dv.setUint32(88, payload.length)
  return [...frame, ...payload]
}

// bstack pattern vocabulary — our market is their pattern family:
// directory (who's who + what they know), audit (history), messagebox
// (demand/answer), grant (delegated authority). Cross-impl clients map by this.
export const KIND_PATTERN: Record<string, string> = {
  ad: 'directory', catalog: 'directory', beacon: 'directory', offer: 'directory',
  settlement: 'audit', revocation: 'audit',
  want: 'messagebox', respond: 'messagebox',
  delegation: 'grant', attestation: 'grant', reputation: 'directory'
}
export const kindPattern = (kind: string, payload?: unknown): string =>
  (kind === 'offer' && (payload as any)?.offer?.replyTo) ? 'messagebox' : (KIND_PATTERN[kind] ?? 'audit')

/** The JSON-level envelope for /events — seq cursor + content address, so a
 *  consumer can detect gaps (seq) AND tampering (contentId) without parsing. */
export function eventEnvelope (e: { seq: number, kind: string, pub: string, domains: string[], ts: number, payload: unknown }): { seq: number, kind: string, pub: string, domains: string[], ts: number, payload: unknown, topicId: string, contentId: string, pattern: string } {
  const payload = utf8(JSON.stringify(e.payload))
  return { ...e, topicId: Buffer.from(topicId('tm_presence')).toString('hex'), contentId: Buffer.from(contentId(payload)).toString('hex'), pattern: kindPattern(e.kind, e.payload) }
}
