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
import { chmodSync, existsSync, writeFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { fetchRestateBinary, RESTATE_VERSION } from './restate-fetch.mjs'

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
const fetched = await fetchRestateBinary(join(out, 'restate'))
if (!fetched.installed) {
  console.warn(`no restate-server target for ${process.platform}-${process.arch} — artifact ships without the sidecar`)
} else {
  console.log(`restate-server ${RESTATE_VERSION} (${fetched.target}) → restate/`)
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
          Zero-env default boots on BSV testnet with a seeded 'welcome' shard,
          serves MCP over stdio + companion loopback HTTP (:18420) for /v1/verify,
          hooks, and the dashboard — and automatically bridges a 2nd AI app onto
          the already-running daemon with zero lock collisions.
Dock:     ./nektar dock          # auto-detect & wire Hermes, OpenClaw, Claude Code,
                                 # Codex, Cursor, Windsurf, Claude Desktop, OpenCode, Goose
          ./nektar dock --dry    # preview changes without writing
          ./nektar status        # check running daemon + Restate executor
Config:   optional .env beside this binary — e.g.
            NEKTAR_NETWORK=testnet   (default when unconfigured)
            NEKTAR_WALLET=client     # sign via local BSV Desktop (:3321)
          or omit NEKTAR_WALLET for the embedded wallet (generates a fee
          key on first boot and prints the address to fund).
Executor: a restate-server rides inside restate/ and auto-starts at boot —
          nothing else to install (on Windows it runs through WSL, which
          ships with Windows 11; an already-running restate or
          NEKTAR_RESTATE_URL is used instead).
First call: recall {} (reads the seeded 'welcome' shard).
Trading:  ./nektar policy approve --preset buyer|seller|both
          (sets up an operator-approved trading policy without a git checkout).
`
)
console.log(`deploy tree → ${out}`)
