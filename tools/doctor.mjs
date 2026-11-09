#!/usr/bin/env node
// tools/doctor.mjs — standalone diagnose + repair for when the instance
// can't boot (a corrupt instance.lock crashes startup before MCP can answer).
//   node tools/doctor.mjs          diagnose only
//   node tools/doctor.mjs --fix    apply safe repairs
// Safe = can't lose state: stale/corrupt lock, missing dir, missing VERSION.
// A live-instance lock is reported and refused — never deleted.
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = resolve(fileURLToPath(new URL('..', import.meta.url)))
const FIX = process.argv.includes('--fix')
const envPath = join(root, '.env')
const env = existsSync(envPath)
  ? Object.fromEntries(readFileSync(envPath, 'utf8').split(/\r?\n/).map(l => l.match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/)).filter(Boolean).map(m => [m[1], m[2].trim()]))
  : {}
const dataDir = resolve(process.env.NEKTAR_DATA_DIR ?? env.NEKTAR_DATA_DIR ?? join(root, '.nektar'))

const issues = [], fixed = [], refused = []
const report = (ok, msg) => { issues.push({ ok, msg }); console.log(`  ${ok ? 'ok ' : '!  '} ${msg}`) }
const repair = (msg, fn) => { if (FIX) { fn(); fixed.push(msg); console.log(`  fix ${msg}`) } else { refused.push(msg); console.log(`  !   ${msg} (fixable)`) } }

console.log(`doctor${FIX ? ' --fix' : ''} — dataDir ${dataDir}\n`)

// data dir
if (!existsSync(dataDir)) repair(`create missing data dir ${dataDir}`, () => mkdirSync(dataDir, { recursive: true }))
else report(true, 'data dir exists')

// instance.lock — a live heartbeat (<20s) means another instance owns it
const lock = join(dataDir, 'instance.lock')
if (existsSync(lock)) {
  let l = null
  try { l = JSON.parse(readFileSync(lock, 'utf8')) } catch { /* corrupt */ }
  if (l === null) repair('remove corrupt instance.lock (unparseable — blocks startup)', () => rmSync(lock))
  else {
    const age = Date.now() - (l.beat ?? 0)
    if (age < 20_000) { report(false, `instance.lock held by live instance ${l.owner} — stop it first`); refused.push('live lock untouched') }
    else repair(`remove stale instance.lock (beat ${Math.round(age / 1000)}s old)`, () => rmSync(lock))
  }
} else report(true, 'no instance.lock')

// expected files — presence is diagnosis, never repair-deleted
for (const f of ['keys.json', 'records.sqlite', 'VERSION']) {
  const p = join(dataDir, f)
  if (existsSync(p)) report(true, f)
  else if (f === 'VERSION') repair('write VERSION=1 (migration baseline)', () => writeFileSync(p, '1'))
  else report(f !== 'records.sqlite' || existsSync(dataDir), `${f} absent${f === 'keys.json' ? ' — identity not generated yet (first boot creates it)' : ''}`)
}

if (!FIX && refused.length) console.log(`\n${refused.length} repairable issue(s) — re-run with --fix`)
if (FIX) console.log(`\n${fixed.length} fixed, ${refused.length} refused`)
process.exit(refused.length && !fixed.length && FIX ? 1 : 0)
