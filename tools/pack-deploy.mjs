#!/usr/bin/env node
// tools/pack-deploy.mjs — self-contained nektar-mcp deploy tree (the no-build
// release artifact). `pnpm deploy --prod` inlines every workspace/file: dep
// into a real flat node_modules (--node-linker hoisted), so requires resolve
// exactly as in dev — the esbuild single-file bundle missed
// createRequire('@bsv/sdk') and shipped MODULE_NOT_FOUND; this tree is
// verified by a clean-dir boot smoke in release.yml. sqlite3 is a native dep
// → one artifact per OS.
//   node tools/pack-deploy.mjs   → dist-deploy/   (node dist/src/bin.js)
import { spawnSync } from 'node:child_process'
import { chmodSync, existsSync, writeFileSync } from 'node:fs'
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
Client:   point your MCP client at the command above (stdio), or at
          http://localhost:8080/mcp when NEKTAR_HTTP_PORT is set.
`
)
console.log(`deploy tree → ${out}`)
