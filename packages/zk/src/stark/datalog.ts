// L_ZKDer v3 — Wright §14 ZK derivation attestation on the wire.
//   'ZKD3' ‖ ver(1) ‖ imageId(32) ‖ policyRoot(32) ‖ conclusionHash(32) ‖ queryId(32) ‖ receiptLen(u32 LE) ‖ receipt
// The header repeats the receipt's public journal so verifiers can route and
// pre-check cheaply; verification REQUIRES header == journal byte for byte.
// The guest is tools/stark/methods/guest-datalog; the receipt is the same
// RISC Zero succinct format verified by the same WASM verifier.
import { createRequire } from 'node:module'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { spawn } from 'node:child_process'
import { StarkProverError } from './prove.js'

export const ZKD3_MAGIC = Uint8Array.from([0x5a, 0x4b, 0x44, 0x33]) // "ZKD3"
export const ZKD3_VERSION = 2 // rev 2: GEN-007 algorithm id byte
/** GEN-007: 0x01 = the sha256d suite (defined once in codec.ts) */
export { ALG_SHA256D } from './codec.js'
import { ALG_SHA256D } from './codec.js'
export const ZKD3_MAX_RECEIPT = 1_500_000 // succinct receipts are ~224 KB; bound the parser
const HEADER_V1 = 4 + 1 + 32 + 32 + 32 + 32 + 4
const HEADER = HEADER_V1 + 1 // + algorithm byte

export class ZkdCodecError extends Error {}

export interface ZkDerivationProof {
  /** GEN-007 algorithm suite id (sha256d); rev-1 wires imply it */
  algorithm?: number
  imageId: Uint8Array // pinned guest-datalog program identity
  policyRoot: Uint8Array // the anchored policy-DAG root (rule + fact nodes)
  conclusionHash: Uint8Array // h_k = dsha256(serialise(conclusion)) — the fact itself stays private
  queryId: Uint8Array // verifier-issued, single use
  receipt: Uint8Array // RISC Zero succinct receipt (one ZK-STARK), bincode
}

const u32 = (v: number) => { const b = new Uint8Array(4); new DataView(b.buffer).setUint32(0, v, true); return b }

export function encodeZkder (p: ZkDerivationProof): Uint8Array {
  const alg = p.algorithm ?? ALG_SHA256D
  if (alg !== ALG_SHA256D) throw new ZkdCodecError(`unsupported algorithm ${alg}`)
  for (const [k, v] of [['imageId', p.imageId], ['policyRoot', p.policyRoot], ['conclusionHash', p.conclusionHash], ['queryId', p.queryId]] as const) if (v.length !== 32) throw new ZkdCodecError(`${k} must be 32 bytes`)
  if (p.receipt.length === 0 || p.receipt.length > ZKD3_MAX_RECEIPT) throw new ZkdCodecError('receipt length out of range')
  const out = new Uint8Array(HEADER + p.receipt.length)
  let o = 0
  const put = (b: Uint8Array) => { out.set(b, o); o += b.length }
  put(ZKD3_MAGIC); put(Uint8Array.of(ZKD3_VERSION)); put(Uint8Array.of(alg)); put(p.imageId); put(p.policyRoot); put(p.conclusionHash); put(p.queryId); put(u32(p.receipt.length)); put(p.receipt)
  return out
}

export function decodeZkder (b: Uint8Array): ZkDerivationProof {
  if (b.length < HEADER_V1) throw new ZkdCodecError('truncated header')
  for (let i = 0; i < 4; i++) if (b[i] !== ZKD3_MAGIC[i]) throw new ZkdCodecError('bad magic (not L_ZKDer v3)')
  const ver = b[4]!
  if (ver !== 1 && ver !== ZKD3_VERSION) throw new ZkdCodecError(`unsupported version ${ver}`)
  const off = ver === 1 ? 0 : 1 // rev 2 inserts the algorithm byte at index 5
  const alg = ver === 1 ? ALG_SHA256D : b[5]!
  if (alg !== ALG_SHA256D) throw new ZkdCodecError(`unsupported algorithm ${alg}`)
  const header = HEADER_V1 + off
  if (b.length < header) throw new ZkdCodecError('truncated header')
  const dv = new DataView(b.buffer, b.byteOffset, b.byteLength)
  const len = dv.getUint32(header - 4, true)
  if (len === 0 || len > ZKD3_MAX_RECEIPT) throw new ZkdCodecError('receipt length out of range')
  if (b.length !== header + len) throw new ZkdCodecError(b.length < header + len ? 'truncated receipt' : 'trailing bytes')
  return { algorithm: alg, imageId: b.slice(5 + off, 37 + off), policyRoot: b.slice(37 + off, 69 + off), conclusionHash: b.slice(69 + off, 101 + off), queryId: b.slice(101 + off, 133 + off), receipt: b.slice(header) }
}

// ── verification (off-chain WASM risc0 verifier; pure: bytes → journal) ──
const require = createRequire(import.meta.url)
const wasm = require('../../../stark/verifier.js') as { verify: (receipt: Uint8Array, imageId: Uint8Array) => string }

/** the derivation program identity mamem trusts (built by `pnpm stark:build`) */
export const PINNED_DATALOG_IMAGE_ID: string = JSON.parse(readFileSync(fileURLToPath(new URL('../../../stark/image-id.json', import.meta.url)), 'utf8')).datalogImageId

export interface DerJournal { version: number, policyRoot: string, conclusionHash: string, queryId: string }
export type DerVerdict = { ok: true, journal: DerJournal, proof: ZkDerivationProof } | { ok: false, reason: string }

const hex = (b: Uint8Array) => Buffer.from(b).toString('hex')

/** decode L_ZKDer v3, check the pinned datalog image id, verify the ZK-STARK
 *  and require header == journal. policyRoot/queryId checks are the caller's. */
export function verifyZkder (bytes: Uint8Array, pinnedImageId = PINNED_DATALOG_IMAGE_ID): DerVerdict {
  let p: ZkDerivationProof
  try { p = decodeZkder(bytes) } catch (e) { return { ok: false, reason: e instanceof ZkdCodecError ? `codec: ${e.message}` : 'codec error' } }
  if (hex(p.imageId) !== pinnedImageId) return { ok: false, reason: 'image id is not the pinned mamem derivation program' }
  const r = JSON.parse(wasm.verify(p.receipt, p.imageId)) as { ok: boolean, journal?: DerJournal, error?: string }
  if (!r.ok || !r.journal) return { ok: false, reason: `stark: ${r.error ?? 'invalid'}` }
  const j = r.journal
  if (j.version !== 1) return { ok: false, reason: `journal version ${j.version}` }
  if (j.policyRoot !== hex(p.policyRoot) || j.conclusionHash !== hex(p.conclusionHash) || j.queryId !== hex(p.queryId)) return { ok: false, reason: 'header does not match the proven journal' }
  return { ok: true, journal: j, proof: p }
}

/**
 * bound verifier: journal.policyRoot must be the verifier's anchored policy
 * root (never read from the proof) and the queryId one the verifier issued.
 */
export function verifyDerivationBound (
  bytes: Uint8Array,
  queries: { consume: (queryIdHex: string) => boolean },
  expectedPolicyRoot: string,
  pinnedImageId = PINNED_DATALOG_IMAGE_ID
): { ok: boolean, reason?: string, journal?: DerJournal } {
  if (!/^[0-9a-f]{64}$/.test(expectedPolicyRoot)) return { ok: false, reason: 'no anchored policy root configured' }
  const v = verifyZkder(bytes, pinnedImageId)
  if (!v.ok) return { ok: false, reason: v.reason }
  if (v.journal.policyRoot !== expectedPolicyRoot) return { ok: false, reason: 'proven policyRoot is not the anchored policy DAG root' }
  if (!queries.consume(v.journal.queryId)) return { ok: false, reason: 'queryId not issued by this verifier, expired, or already used' }
  return { ok: true, journal: v.journal }
}

// ── proving (local only: the prover sees the private path + fact contents) ──
export interface DerStepInputWire { fact: { pred: string, args: string[] }, fromStep: number | null, committedIndex?: number | null, committedSiblings?: string[] }
export interface DerStepWire {
  rule: { id: string, layer: string, head: { pred: string, args: string[], neg: boolean }, body: Array<{ pred: string, args: string[], neg: boolean }> }
  ruleIndex: number
  ruleSiblings: string[]
  inputs: DerStepInputWire[]
  output: { pred: string, args: string[] }
}
export interface ProveDerivationInput {
  nodeCount: number // total committed DAG nodes the policy root covers
  policyRoot: Uint8Array
  queryId: Uint8Array
  steps: DerStepWire[]
}
const unhex = (h: string) => Uint8Array.from(Buffer.from(h, 'hex'))

function proverCommand (): [string, string[]] {
  const bin = process.env.MAMEM_STARK_PROVER
  if (bin) return [bin, ['prove-der']]
  return ['docker', ['run', '--rm', '-i', '-v', `${process.env.MAMEM_STARK_TARGET_VOLUME ?? 'mamem-stark-target'}:/work/target`, process.env.MAMEM_STARK_IMAGE ?? 'mamem-stark-build', '/work/target/release/mamem-stark', 'prove-der']]
}

/** prove a §14 committed derivation path and return L_ZKDer v3 bytes */
export async function proveDerivationStark (inp: ProveDerivationInput, timeoutMs = Number(process.env.MAMEM_STARK_TIMEOUT_MS ?? 300_000)): Promise<{ bytes: Uint8Array, ms: number, policyRoot: string, conclusionHash: string }> {
  const [cmd, args] = proverCommand()
  const payload = JSON.stringify({
    nodeCount: inp.nodeCount,
    policyRoot: hex(inp.policyRoot),
    queryId: hex(inp.queryId),
    steps: inp.steps.map(s => ({
      rule: s.rule,
      ruleIndex: s.ruleIndex,
      ruleSiblings: s.ruleSiblings,
      inputs: s.inputs.map(i => ({ fact: i.fact, fromStep: i.fromStep, committedIndex: i.committedIndex ?? null, committedSiblings: i.committedSiblings ?? [] })),
      output: s.output
    }))
  })
  const out = await new Promise<string>((resolve, reject) => {
    const child = spawn(cmd, args, { stdio: ['pipe', 'pipe', 'pipe'], env: { ...process.env, MSYS_NO_PATHCONV: '1' } })
    let so = ''; let se = ''
    const timer = setTimeout(() => { child.kill('SIGKILL'); reject(new StarkProverError(`prover timed out after ${timeoutMs} ms`)) }, timeoutMs)
    child.stdout.on('data', d => { so += d })
    child.stderr.on('data', d => { se += d })
    child.on('error', e => { clearTimeout(timer); reject(new StarkProverError(`prover failed to start: ${e.message}`)) })
    child.on('close', code => {
      clearTimeout(timer)
      if (code !== 0) reject(new StarkProverError(`prover refused (exit ${code}): ${se.split('\n').filter(l => /panicked|assert|error/i.test(l)).slice(-2).join(' ').slice(0, 300) || 'no detail'}`))
      else resolve(so)
    })
    child.stdin.end(payload)
  })
  const r = JSON.parse(out.trim().split('\n').pop()!) as { receipt: string, imageId: string, ms: number, journal: { policyRoot: string, conclusionHash: string, queryId: string } }
  const bytes = encodeZkder({ imageId: unhex(r.imageId), policyRoot: unhex(r.journal.policyRoot), conclusionHash: unhex(r.journal.conclusionHash), queryId: unhex(r.journal.queryId), receipt: unhex(r.receipt) })
  return { bytes, ms: r.ms, policyRoot: r.journal.policyRoot, conclusionHash: r.journal.conclusionHash }
}
