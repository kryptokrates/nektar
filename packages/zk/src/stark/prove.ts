// STARK prover adapter: runs the pinned RISC Zero membership program.
//   MAMEM_STARK_PROVER=/path/to/mamem-stark  → native binary (Linux image)
//   otherwise → `docker run -i mamem-stark-build` with the cached target volume
// Bounded by a timeout; the prover sees the private inputs (local proving only).
import { spawn } from 'node:child_process'
import { encodeZkm2 } from './codec.js'

export interface ProveMembershipInput {
  leaf: Uint8Array
  index: number
  size: number
  siblings: Uint8Array[]
  queryId: Uint8Array
}
export class StarkProverError extends Error {}

const hex = (b: Uint8Array) => Buffer.from(b).toString('hex')
const unhex = (h: string) => Uint8Array.from(Buffer.from(h, 'hex'))

function proverCommand (): [string, string[]] {
  const bin = process.env.MAMEM_STARK_PROVER
  if (bin) return [bin, ['prove']]
  return ['docker', ['run', '--rm', '-i', '-v', `${process.env.MAMEM_STARK_TARGET_VOLUME ?? 'mamem-stark-target'}:/work/target`, process.env.MAMEM_STARK_IMAGE ?? 'mamem-stark-build', '/work/target/release/mamem-stark', 'prove']]
}

/** prove leaf ∈ batch(root) and return L_ZKMem v2 bytes */
export async function proveMembershipStark (inp: ProveMembershipInput, timeoutMs = Number(process.env.MAMEM_STARK_TIMEOUT_MS ?? 300_000)): Promise<{ bytes: Uint8Array, ms: number, root: string }> {
  const [cmd, args] = proverCommand()
  const payload = JSON.stringify({ leaf: hex(inp.leaf), index: inp.index, size: inp.size, siblings: inp.siblings.map(hex), queryId: hex(inp.queryId) })
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
  const r = JSON.parse(out.trim().split('\n').pop()!) as { receipt: string, imageId: string, ms: number, journal: { root: string, size: number, queryId: string } }
  const bytes = encodeZkm2({ imageId: unhex(r.imageId), root: unhex(r.journal.root), size: r.journal.size, queryId: unhex(r.journal.queryId), receipt: unhex(r.receipt) })
  return { bytes, ms: r.ms, root: r.journal.root }
}
