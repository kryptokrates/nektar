#!/usr/bin/env node
// tools/pack-deploy.mjs — self-contained nektar-mcp deploy tree (the no-build
// release artifact). `pnpm deploy --prod` inlines every workspace/file: dep
// into a real flat node_modules (--node-linker hoisted), so requires resolve
// exactly as in dev — the esbuild single-file bundle missed
// createRequire('@bsv/sdk') and shipped MODULE_NOT_FOUND; this tree is
// verified by a clean-dir boot smoke in release.yml. sqlite3 is a native dep
// → one artifact per OS. The restate-server binary rides in restate/ —
// restatedev ships no win32 build, so the windows artifact carries the musl
// binary for the WSL bridge (bin.ts spawns it through wsl.exe; WSL2's
// localhostForwarding mirrors the ports onto windows localhost).
//   node tools/pack-deploy.mjs   → dist-deploy/   (node dist/src/bin.js)
import { spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import { chmodSync, cpSync, existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = resolve(fileURLToPath(new URL('..', import.meta.url)))
const out = join(root, 'dist-deploy')

if (!existsSync(join(root, 'packages/mcp/dist/src/bin.js'))) {
  console.error('packages/mcp/dist/src/bin.js missing — run pnpm -r build first')
  process.exit(2)
}

const r =
  process.platform === 'win32'
    ? // pnpm resolves to pnpm.cmd — needs the shell; quote the path manually
      // since shell:true concatenates args unescaped
      spawnSync('pnpm', ['deploy', '--filter', '@nektar/mcp', '--prod', '--node-linker', 'hoisted', `"${out}"`], {
        cwd: root,
        stdio: 'inherit',
        shell: true
      })
    : spawnSync('pnpm', ['deploy', '--filter', '@nektar/mcp', '--prod', '--node-linker', 'hoisted', out], {
        cwd: root,
        stdio: 'inherit'
      })
if (r.status !== 0) process.exit(r.status ?? 1)

// ─── restate-server sidecar ─────────────────────────────────────────────────
// the executor plane's only external process — version pinned to the compose
// sidecar's. win32 gets the musl build (run through wsl.exe at boot).
const RESTATE_VERSION = '1.8.1'
const RESTATE_TARGETS = {
  'linux-x64': 'x86_64-unknown-linux-musl',
  'linux-arm64': 'aarch64-unknown-linux-musl',
  'darwin-arm64': 'aarch64-apple-darwin',
  'darwin-x64': 'x86_64-apple-darwin',
  'win32-x64': 'x86_64-unknown-linux-musl' // via WSL — no win32 build exists
}
const target = RESTATE_TARGETS[`${process.platform}-${process.arch}`]
if (!target) {
  console.warn(`no restate-server target for ${process.platform}-${process.arch} — artifact ships without the sidecar`)
} else {
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
  writeFileSync(join(tmp, name), txz)
  // --force-local + posix paths: git-bash tar reads `C:\` as a remote host
  // spec and its -C can't open backslashed windows dirs
  const posix = (p) => p.replace(/\\/g, '/')
  const x = spawnSync('tar', ['--force-local', '-xJf', posix(join(tmp, name)), '-C', posix(tmp)], { stdio: 'inherit' })
  if (x.status !== 0) throw new Error(`tar extract failed (${x.status}) — need bsdtar with xz support`)
  const src = join(tmp, `restate-server-${target}`, 'restate-server')
  const rd = join(out, 'restate')
  mkdirSync(rd, { recursive: true })
  cpSync(src, join(rd, 'restate-server'))
  try {
    chmodSync(join(rd, 'restate-server'), 0o755)
  } catch {
    /* windows */
  }
  rmSync(tmp, { recursive: true, force: true })
  console.log(`restate-server ${RESTATE_VERSION} (${target}) → restate/`)
}

writeFileSync(join(out, 'nektar'), '#!/bin/sh\nexec node "$(dirname "$0")/dist/src/bin.js" "$@"\n')
try {
  chmodSync(join(out, 'nektar'), 0o755)
} catch {
  /* Windows has no POSIX modes */
}
writeFileSync(join(out, 'nektar.cmd'), '@echo off\r\nnode "%~dp0dist\\src\\bin.js" %*\r\n')
writeFileSync(
  join(out, 'README.txt'),
  `nektar-mcp — verifiable agent memory on BSV (self-contained, no build)
Requires: Node.js 22.12+ (node --version)
Run:      ./nektar   (unix)   or   nektar.cmd   (windows)
          stdio MCP by default; NEKTAR_HTTP_PORT=8080 adds /v1 + /mcp over HTTP
Config:   a .env in this directory is read at boot — e.g.
            NEKTAR_NETWORK=testnet
            NEKTAR_WALLET=client     # sign via local BSV Desktop (:3321)
          or omit NEKTAR_WALLET for the embedded wallet (generates a fee
          key on first boot and prints the address to fund).
Executor: a restate-server rides inside restate/ and auto-starts at boot —
          nothing else to install (on Windows it runs through WSL, which
          ships with Windows 11; an already-running restate or
          NEKTAR_RESTATE_URL is used instead).
Client:   point your MCP client at the command above (stdio), or at
          http://localhost:8080/mcp when NEKTAR_HTTP_PORT is set.
`
)
console.log(`deploy tree → ${out}`)
