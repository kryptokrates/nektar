// JSON wire format for clearance credentials and presentations (MCP / HTTP).
// bigint → decimal string, bytes → hex. Decoders are strict: anything that is
// not exactly the expected shape throws WireError.
import type { ClearanceCommitment, ClearanceCredential, ZkTranscript, RangeProof } from './zkaccess.js'
import type { RangeProofBP } from './bp.js'

export class WireError extends Error {}

export interface ClearancePresentationWire {
  commitment: ClearanceCommitment
  proof: RangeProof | RangeProofBP
  transcript: { queryId: string; fragmentId: string; nonce: string }
  requiredLevel: string
}
export interface ClearancePresentation {
  commitment: ClearanceCommitment
  proof: RangeProof | RangeProofBP
  transcript: ZkTranscript
  requiredLevel: bigint
}

const HEX = /^[0-9a-f]*$/
const isStr = (v: unknown): v is string => typeof v === 'string'
const hexStr = (v: unknown, what: string, len?: number): string => {
  if (!isStr(v) || !HEX.test(v) || v.length % 2 !== 0 || (len !== undefined && v.length !== len))
    throw new WireError(`${what}: expected ${len ? `${len / 2}-byte ` : ''}lowercase hex`)
  return v
}
const decStr = (v: unknown, what: string): bigint => {
  if (!isStr(v) || !/^(0|[1-9][0-9]{0,77})$/.test(v)) throw new WireError(`${what}: expected a non-negative decimal string`)
  return BigInt(v)
}
const obj = (v: unknown, what: string): Record<string, unknown> => {
  if (v === null || typeof v !== 'object' || Array.isArray(v)) throw new WireError(`${what}: expected an object`)
  return v as Record<string, unknown>
}

function decodeCommitment(v: unknown): ClearanceCommitment {
  const o = obj(v, 'commitment')
  const keys = Object.keys(o).sort().join(',')
  if (keys !== 'hex,holder,pub,sig') throw new WireError(`commitment: unexpected fields (${keys})`)
  return {
    pub: hexStr(o.pub, 'commitment.pub', 66),
    holder: hexStr(o.holder, 'commitment.holder', 66),
    hex: hexStr(o.hex, 'commitment.hex', 66),
    sig: hexStr(o.sig, 'commitment.sig')
  }
}

export function encodePresentation(p: ClearancePresentation): ClearancePresentationWire {
  return {
    commitment: p.commitment,
    proof: p.proof,
    transcript: { queryId: p.transcript.queryId, fragmentId: p.transcript.fragmentId, nonce: Buffer.from(p.transcript.nonce).toString('hex') },
    requiredLevel: p.requiredLevel.toString()
  }
}
export function decodePresentation(v: unknown): ClearancePresentation {
  const o = obj(v, 'zk')
  const t = obj(o.transcript, 'zk.transcript')
  if (!isStr(t.queryId) || !isStr(t.fragmentId)) throw new WireError('zk.transcript: queryId and fragmentId must be strings')
  const proof = obj(o.proof, 'zk.proof')
  if (typeof proof.bits !== 'number') throw new WireError('zk.proof: missing bits')
  return {
    commitment: decodeCommitment(o.commitment),
    proof: proof as unknown as RangeProof | RangeProofBP, // verified cryptographically by verifyAccess
    transcript: { queryId: t.queryId, fragmentId: t.fragmentId, nonce: Uint8Array.from(Buffer.from(hexStr(t.nonce, 'zk.transcript.nonce'), 'hex')) },
    requiredLevel: decStr(o.requiredLevel, 'zk.requiredLevel')
  }
}

/** holder-side storage of an issued credential (contains the SECRET opening) */
export const encodeCredential = (c: ClearanceCredential) => ({ commitment: c.commitment, level: c.level.toString(), blinding: c.blinding })
export function decodeCredential(v: unknown): ClearanceCredential {
  const o = obj(v, 'credential')
  if (!isStr(o.blinding) || !/^[0-9]+$/.test(o.blinding)) throw new WireError('credential.blinding: expected a decimal string')
  return { commitment: decodeCommitment(o.commitment), level: decStr(o.level, 'credential.level'), blinding: o.blinding }
}
