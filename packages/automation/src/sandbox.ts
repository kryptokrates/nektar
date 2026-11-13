import { execFile } from 'node:child_process'
import { chmodSync, cpSync, existsSync, lstatSync, mkdtempSync, readdirSync, readFileSync, realpathSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { isAbsolute, join, normalize, relative, resolve, sep } from 'node:path'

export interface PatchProposal {
  edits: Array<{ file: string; find: string; replace: string }>
}

export interface SandboxSpec {
  fixtureDir: string
  allowedPaths: string[]
  testCommand: string[]
  hiddenTestsDir?: string
  hiddenTestCommand?: string[]
  image: string
  timeoutMs: number
  maxPatchBytes: number
}

interface RunResult {
  exitCode: number | null
  timedOut: boolean
  outputTail: string
}
export interface SandboxResult {
  outcome: 'PASS' | 'FAIL' | 'BLOCKED'
  reason?: string
  isolation?: { backend: string; network: 'none'; image: string }
  baseline?: RunResult
  patched?: RunResult
  hidden?: RunResult
  edits?: PatchProposal['edits']
}

export interface IsolationBackend {
  name: string
  available: () => Promise<boolean>
  run: (workDir: string, hiddenDir: string | undefined, cmd: string[], image: string, timeoutMs: number) => Promise<RunResult>
}

const PROTECTED = /(^|\/)(package\.json|package-lock\.json|pnpm-lock\.yaml|yarn\.lock|\.npmrc|\.yarnrc[^/]*|\.env[^/]*|\.git|\.github|node_modules)(\/|$)/i

export function parsePatch(text: string): PatchProposal | undefined {
  const m = /```nektar-patch\s*\n([\s\S]*?)\n```/.exec(text)
  if (!m) return undefined
  try {
    const j = JSON.parse(m[1])
    if (!Array.isArray(j?.edits)) return undefined
    return { edits: j.edits.map((e: any) => ({ file: String(e.file), find: String(e.find), replace: String(e.replace) })) }
  } catch {
    return undefined
  }
}

export function validatePatch(spec: Pick<SandboxSpec, 'allowedPaths' | 'maxPatchBytes'>, patch: PatchProposal): string | undefined {
  if (!patch || !Array.isArray(patch.edits) || patch.edits.length === 0 || patch.edits.length > 32) return 'patch must contain 1..32 edits'
  let bytes = 0
  for (const e of patch.edits) {
    if (typeof e.file !== 'string' || typeof e.find !== 'string' || typeof e.replace !== 'string' || !e.find.length) return 'malformed edit'
    const f = e.file.replace(/\\/g, '/')
    if (isAbsolute(e.file) || /^[A-Za-z]:/.test(f) || f.startsWith('/') || f.split('/').some((s) => s === '..' || s === '' || s === '.'))
      return `path escapes the checkout: ${e.file}`
    if (f.includes('\0') || PROTECTED.test(f)) return `protected path: ${e.file}`
    if (!spec.allowedPaths.some((a) => f === a || f.startsWith(a.endsWith('/') ? a : `${a}/`))) return `path outside the allowed set: ${e.file}`
    bytes += Buffer.byteLength(e.find) + Buffer.byteLength(e.replace)
  }
  if (bytes > spec.maxPatchBytes) return 'patch too large'
  return undefined
}

function noSymlinks(dir: string): string | undefined {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name)
    const st = lstatSync(p)
    if (st.isSymbolicLink()) return p
    if (st.isDirectory()) {
      const hit = noSymlinks(p)
      if (hit) return hit
    }
  }
  return undefined
}

export const dockerBackend: IsolationBackend = {
  name: 'docker',
  available: () =>
    new Promise((res) => {
      execFile('docker', ['ps', '-q'], { timeout: 15_000 }, (err) => res(!err))
    }),
  run: (workDir, hiddenDir, cmd, image, timeoutMs) =>
    new Promise((res) => {
      const args = [
        'run',
        '--rm',
        '--network',
        'none',
        '--cap-drop',
        'ALL',
        '--security-opt',
        'no-new-privileges',
        '--pids-limit',
        '128',
        '--memory',
        '512m',
        '--cpus',
        '1',
        '--read-only',
        '--tmpfs',
        '/tmp',
        '-v',
        `${workDir}:/work`,
        ...(hiddenDir ? ['-v', `${hiddenDir}:/hidden:ro`] : []),
        '-w',
        '/work',
        image,
        ...cmd
      ]
      execFile('docker', args, { timeout: timeoutMs, maxBuffer: 4 * 1024 * 1024 }, (err, stdout, stderr) => {
        const e = err as (Error & { code?: number | string; killed?: boolean }) | null
        res({ exitCode: e ? (typeof e.code === 'number' ? e.code : null) : 0, timedOut: !!e?.killed, outputTail: `${stdout}${stderr}`.slice(-4000) })
      })
    })
}

export async function evaluatePatch(spec: SandboxSpec, patch: PatchProposal, backend: IsolationBackend = dockerBackend): Promise<SandboxResult> {
  const bad = validatePatch(spec, patch)
  if (bad) return { outcome: 'FAIL', reason: `patch refused: ${bad}` }
  if (!existsSync(spec.fixtureDir)) return { outcome: 'BLOCKED', reason: 'fixture missing' }
  const link = noSymlinks(spec.fixtureDir)
  if (link) return { outcome: 'FAIL', reason: `fixture contains a symlink: ${link}` }
  if (!(await backend.available()))
    return { outcome: 'BLOCKED', reason: `isolation backend "${backend.name}" is not available — refusing to run untrusted code on the host` }
  const isolation = { backend: backend.name, network: 'none' as const, image: spec.image }
  const work = mkdtempSync(join(tmpdir(), 'nektar-sandbox-'))
  try {
    // mkdtempSync creates 0700 — the container runs with --cap-drop ALL, so its
    // uid-0 lacks CAP_DAC_OVERRIDE and cannot traverse a runner-owned 0700 dir
    // on a real dockerd (Docker Desktop maps perms, which is why it hid locally)
    chmodSync(work, 0o755)
    cpSync(spec.fixtureDir, work, { recursive: true })
    const root = realpathSync(work)
    const baseline = await backend.run(root, undefined, spec.testCommand, spec.image, spec.timeoutMs)
    if (baseline.exitCode === null || baseline.exitCode >= 125)
      return { outcome: 'BLOCKED', reason: 'the isolation backend could not run the baseline', isolation, baseline }
    if (baseline.exitCode === 0)
      return { outcome: 'BLOCKED', reason: 'the fixture does not fail before the patch — nothing to demonstrate', isolation, baseline }
    for (const e of patch.edits) {
      const target = resolve(root, normalize(e.file))
      const rel = relative(root, target)
      if (rel.startsWith('..') || isAbsolute(rel) || !existsSync(target) || lstatSync(target).isSymbolicLink() || !realpathSync(target).startsWith(root + sep))
        return { outcome: 'FAIL', reason: `edit target not a regular file inside the checkout: ${e.file}`, isolation, baseline }
      const src = readFileSync(target, 'utf8')
      if (!src.includes(e.find)) return { outcome: 'FAIL', reason: `edit does not apply: ${e.file}`, isolation, baseline }
      writeFileSync(target, src.split(e.find).join(e.replace))
    }
    const patched = await backend.run(root, undefined, spec.testCommand, spec.image, spec.timeoutMs)
    if (patched.exitCode !== 0)
      return { outcome: 'FAIL', reason: 'acceptance tests still fail after the patch', isolation, baseline, patched, edits: patch.edits }
    let hidden: RunResult | undefined
    if (spec.hiddenTestsDir && spec.hiddenTestCommand) {
      hidden = await backend.run(root, realpathSync(spec.hiddenTestsDir), spec.hiddenTestCommand, spec.image, spec.timeoutMs)
      if (hidden.exitCode !== 0) return { outcome: 'FAIL', reason: 'hidden regression tests fail', isolation, baseline, patched, hidden, edits: patch.edits }
    }
    return { outcome: 'PASS', isolation, baseline, patched, hidden, edits: patch.edits }
  } finally {
    rmSync(work, { recursive: true, force: true })
  }
}
