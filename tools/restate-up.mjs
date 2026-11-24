#!/usr/bin/env node
// tools/restate-up.mjs — install the SHA256-verified restate-server binary
// into packages/mcp/restate/ so local source-checkout runs (`pnpm init:nektar`,
// `pnpm demo`, `node packages/mcp/dist/src/bin.js`) auto-spawn the sidecar
// identically to the release artifact (on Windows through WSL).
import { existsSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { fetchRestateBinary, RESTATE_VERSION } from './restate-fetch.mjs'

const root = resolve(fileURLToPath(new URL('..', import.meta.url)))
const dest = join(root, 'packages/mcp/restate')
const bin = join(dest, 'restate-server')
const force = process.argv.includes('--force')

if (existsSync(bin) && !force) {
  console.log(`restate-server already installed at ${bin} (pass --force to re-download)`)
  process.exit(0)
}

console.log(`downloading restate-server v${RESTATE_VERSION} → ${dest} …`)
const r = await fetchRestateBinary(dest)
if (!r.installed) {
  console.error(`no prebuilt restate-server target for ${process.platform}-${process.arch} — set NEKTAR_RESTATE_URL to a running restate ingress`)
  process.exit(1)
}
console.log(`restate-server v${RESTATE_VERSION} (${r.target}) ready → ${r.path}`)
if (process.platform === 'win32') console.log('note: on Windows nektar runs the linux sidecar through WSL (wsl --install -d Ubuntu if not installed yet)')
