// STARK prover adapter: runs the pinned RISC Zero membership program.
//   NEKTAR_STARK_PROVER_URL=https://host     → remote prover (POST {path}, JSON in/out)
//   NEKTAR_STARK_PROVER=/path/to/nektar-stark  → native binary (Linux image)
//   otherwise → `docker run -i nektar-stark-build` with the cached target volume
// Bounded by a timeout; the prover sees the private inputs — remote proving
// means trusting that host with witnesses (use your own prover service).
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

export function proverUrl(): string | null {
  const u = process.env.NEKTAR_STARK_PROVER_URL
  return u ? u.replace(/\/$/, '') : null
}

/** POST a prover payload to NEKTAR_STARK_PROVER_URL/{path} — the service is a
 *  thin shim over the same `nektar-stark` binary (JSON stdin → JSON stdout). */
export async function remoteProve(payload: string, path: 'prove' | 'prove-der', timeoutMs: number): Promise<string> {
  const url = `${proverUrl()}/${path}`
  const ctrl = new AbortController()
  const timer = setTimeout(() => ctrl.abort(), timeoutMs)
  const headers: Record<string, string> = { 'Content-Type': 'application/json' }
  if (process.env.NEKTAR_STARK_PROVER_TOKEN) headers.Authorization = `Bearer ${process.env.NEKTAR_STARK_PROVER_TOKEN}`
  let res: Response
  try {
    res = await fetch(url, { method: 'POST', headers, body: payload, signal: ctrl.signal })
  } catch (e) {
    clearTimeout(timer)
    throw new StarkProverError(`remote prover unreachable: ${(e as Error).message}`)
  }
  clearTimeout(timer)
  const text = await res.text()
  if (!res.ok) throw new StarkProverError(`remote prover refused (HTTP ${res.status}): ${text.slice(0, 300)}`)
  return text
}

function proverCommand(): [string, string[]] {
  const bin = process.env.NEKTAR_STARK_PROVER
  if (bin) return [bin, ['prove']]
  return [
    'docker',
    [
      'run',
      '--rm',
      '-i',
      '-v',
      `${process.env.NEKTAR_STARK_TARGET_VOLUME ?? 'nektar-stark-target'}:/work/target`,
      process.env.NEKTAR_STARK_IMAGE ?? 'nektar-stark-build',
      '/work/target/release/nektar-stark',
      'prove'
    ]
  ]
}

/** prove leaf ∈ batch(root) and return L_ZKMem v2 bytes */
export async function proveMembershipStark(
  inp: ProveMembershipInput,
  timeoutMs = Number(process.env.NEKTAR_STARK_TIMEOUT_MS ?? 300_000)
): Promise<{ bytes: Uint8Array; ms: number; root: string }> {
  const payload = JSON.stringify({ leaf: hex(inp.leaf), index: inp.index, size: inp.size, siblings: inp.siblings.map(hex), queryId: hex(inp.queryId) })
  const out = proverUrl()
    ? await remoteProve(payload, 'prove', timeoutMs)
    : await new Promise<string>((resolve, reject) => {
        const [cmd, args] = proverCommand()
        const child = spawn(cmd, args, { stdio: ['pipe', 'pipe', 'pipe'], env: { ...process.env, MSYS_NO_PATHCONV: '1' } })
        let so = ''
        let se = ''
        const timer = setTimeout(() => {
          child.kill('SIGKILL')
          reject(new StarkProverError(`prover timed out after ${timeoutMs} ms`))
        }, timeoutMs)
        child.stdout.on('data', (d) => {
          so += d
        })
        child.stderr.on('data', (d) => {
          se += d
        })
        child.on('error', (e) => {
          clearTimeout(timer)
          reject(new StarkProverError(`prover failed to start: ${e.message}`))
        })
        child.on('close', (code) => {
          clearTimeout(timer)
          if (code !== 0)
            reject(
              new StarkProverError(
                `prover refused (exit ${code}): ${
                  se
                    .split('\n')
                    .filter((l) => /panicked|assert|error/i.test(l))
                    .slice(-2)
                    .join(' ')
                    .slice(0, 300) || 'no detail'
                }`
              )
            )
          else resolve(so)
        })
        child.stdin.end(payload)
      })
  const r = JSON.parse(out.trim().split('\n').pop()!) as {
    receipt: string
    imageId: string
    ms: number
    journal: { root: string; size: number; queryId: string }
  }
  const bytes = encodeZkm2({
    imageId: unhex(r.imageId),
    root: unhex(r.journal.root),
    size: r.journal.size,
    queryId: unhex(r.journal.queryId),
    receipt: unhex(r.receipt)
  })
  return { bytes, ms: r.ms, root: r.journal.root }
}
