#!/usr/bin/env node
// tools/bundle-mcp.mjs — single-file nektar-mcp bundle via esbuild.
//   pnpm bundle:mcp   → dist-bundle/
//     nektar-mcp.cjs        the whole server (mcp, http, all deps)
//     stark/                wasm verifier runtime files — the bundle reads
//                           them via the same '../../../stark/*' paths the
//                           sources use (bundle sits at a matching depth)
//   node dist-bundle/nektar-mcp.cjs   → NEKTAR_HTTP_PORT=8080 … and go
import { build } from 'esbuild'
import { cpSync, mkdirSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const out = join(root, 'dist-bundle')
mkdirSync(out, { recursive: true })

await build({
  entryPoints: [join(root, 'packages/mcp/dist/src/bin.js')],
  bundle: true,
  platform: 'node',
  format: 'esm',
  target: 'node22',
  outfile: join(out, 'pkg/mcp/dist/nektar-mcp.mjs'),
  external: ['../../../stark/verifier.js', 'dockerode', 'better-sqlite3', 'pg', 'mysql2', 'sqlite3', 'pg-query-stream', 'oracledb', 'tedious', 'mariadb/callback', 'mysql', '*.wasm'],
  banner: { js: `import { createRequire as __cr } from 'node:module'; import { fileURLToPath as __u } from 'node:url'; import { dirname as __d } from 'node:path';
const require = __cr(import.meta.url); const __filename = __u(import.meta.url); const __dirname = __d(__filename);` },
  minify: false,
  sourcemap: false,
  logLevel: 'warning'
})
cpSync(join(root, 'packages/zk/stark'), join(out, 'stark'), { recursive: true })
// cedar-wasm resolves its .wasm next to the importing module — copy beside the bundle
cpSync(join(root, 'node_modules/.pnpm/@cedar-policy+cedar-wasm@4.14.0/node_modules/@cedar-policy/cedar-wasm/esm'), join(out, 'pkg/mcp/dist'), { recursive: true })
// contract artifacts are require('../../../Nektar*.json') relative to contracts/dist — put them at the matching depth
import { readdirSync } from 'node:fs'
for (const f of readdirSync(join(root, 'packages/contracts')).filter(f => f.endsWith('.json') && !['package.json', 'package-lock.json', 'tsconfig.json'].includes(f))) {
  cpSync(join(root, 'packages/contracts', f), join(out, 'pkg', f))
}
console.log(`bundle → ${out}/nektar-mcp.mjs (+ stark/ runtime files)`)
