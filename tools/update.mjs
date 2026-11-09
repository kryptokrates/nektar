#!/usr/bin/env node
// tools/update.mjs — `pnpm update:nektar`: pull → install → build → migrate.
// Migrations come from tools/migrations.mjs and run against the configured
// data dir (NEKTAR_DATA_DIR in .env, default ./.nektar).
//
//   node tools/update.mjs             full update
//   node tools/update.mjs --migrate-only   skip pull/install/build
import { execFileSync, spawnSync } from 'node:child_process'
import { existsSync, mkdirSync, writeFileSync, readFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { MIGRATIONS, currentVersion } from './migrations.mjs'

const root = resolve(fileURLToPath(new URL('..', import.meta.url)))
const MIGRATE_ONLY = process.argv.includes('--migrate-only')
const run = (cmd, args) => { const r = spawnSync(cmd, args, { cwd: root, stdio: 'inherit', shell: process.platform === 'win32' }); if (r.status !== 0) throw new Error(`${cmd} ${args.join(' ')} exited ${r.status}`) }

if (!MIGRATE_ONLY) {
  if (existsSync(join(root, '.git'))) {
    const dirty = execFileSync('git', ['status', '--porcelain'], { cwd: root, encoding: 'utf8' }).trim()
    if (dirty) console.warn(`warning: working tree is dirty — pull may fail\n${dirty.split('\n').slice(0, 5).join('\n')}\n`)
    console.log('1/4 — git pull')
    run('git', ['pull', '--ff-only'])
    console.log('2/4 — pnpm install')
    run('pnpm', ['install', '--frozen-lockfile'])
    console.log('3/4 — pnpm -r build')
    run('pnpm', ['-r', 'build'])
  } else console.log('no .git — skipping pull/install/build (packaged install)')
}

console.log('4/4 — data-dir migrations')
const env = existsSync(join(root, '.env'))
  ? Object.fromEntries(readFileSync(join(root, '.env'), 'utf8').split(/\r?\n/).map(l => l.match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/)).filter(Boolean).map(m => [m[1], m[2].trim()]))
  : {}
const dataDir = resolve(process.env.NEKTAR_DATA_DIR ?? env.NEKTAR_DATA_DIR ?? join(root, '.nektar'))
if (!existsSync(dataDir)) { mkdirSync(dataDir, { recursive: true }); console.log(`  created ${dataDir}`) }
let v = currentVersion(dataDir)
const pending = MIGRATIONS.filter(m => m.v > v)
if (!pending.length) console.log(`  already current (v${v})`)
for (const m of pending) {
  const note = await m.run({ dataDir, env })
  v = m.v
  writeFileSync(join(dataDir, 'VERSION'), String(v))
  console.log(`  v${m.v} ${m.name}${note ? ` — ${note}` : ''}`)
}
console.log(`\ndone — restart any running nektar instance to pick up the update.`)
