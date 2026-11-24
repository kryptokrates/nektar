import { spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import { chmodSync, cpSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

export const RESTATE_VERSION = '1.8.1'
const RESTATE_TARGETS = {
  'linux-x64': 'x86_64-unknown-linux-musl',
  'linux-arm64': 'aarch64-unknown-linux-musl',
  'darwin-arm64': 'aarch64-apple-darwin',
  'darwin-x64': 'x86_64-apple-darwin',
  'win32-x64': 'x86_64-unknown-linux-musl' // via WSL — no win32 build exists
}

export async function fetchRestateBinary(destDir) {
  const target = RESTATE_TARGETS[`${process.platform}-${process.arch}`]
  if (!target) return { installed: false, target: null }
  const base = `https://github.com/restatedev/restate/releases/download/v${RESTATE_VERSION}`
  const name = `restate-server-${target}.tar.xz`
  const dl = async (f) => {
    const res = await fetch(`${base}/${f}`)
    if (!res.ok) throw new Error(`${f}: HTTP ${res.status}`)
    return Buffer.from(await res.arrayBuffer())
  }
  const [txz, sha] = await Promise.all([dl(name), dl(`${name}.sha256`)])
  const want = sha.toString('utf8').split(/\s+/)[0].toLowerCase()
  const got = createHash('sha256').update(txz).digest('hex')
  if (got !== want) throw new Error(`restate-server sha256 mismatch: got ${got}, want ${want}`)
  const tmp = mkdtempSync(join(tmpdir(), 'restate-'))
  try {
    writeFileSync(join(tmp, name), txz)
    const x = spawnSync('tar', ['-xJf', name], { cwd: tmp, stdio: 'inherit' })
    if (x.status !== 0) throw new Error(`tar extract failed (${x.status}) — need bsdtar with xz support`)
    const src = join(tmp, `restate-server-${target}`, 'restate-server')
    mkdirSync(destDir, { recursive: true })
    const out = join(destDir, 'restate-server')
    cpSync(src, out)
    try {
      chmodSync(out, 0o755)
    } catch {
      /* windows */
    }
    return { installed: true, target, path: out }
  } finally {
    rmSync(tmp, { recursive: true, force: true })
  }
}
