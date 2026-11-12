#!/usr/bin/env node
// Legacy inventory: lists source files ranked by last-commit age. Cold files
// are where leftover code hides — a file nobody has touched in a year that
// nothing imports (see check:cruise orphans / check:knip) is a removal
// candidate; a cold file that IS imported is legacy to review.
// Report-only: always exits 0.
import { execSync } from 'node:child_process'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..')
const DAYS = (ms) => Math.floor(ms / 86_400_000)
const LIMIT = Number(process.argv[2] ?? 40)

const tracked = execSync('git ls-files', { cwd: ROOT, encoding: 'utf8' })
  .split('\n')
  .filter((f) => /\.(ts|mts|mjs)$/.test(f) && !f.includes('.runar.') && (f.startsWith('tools/') || /^packages\/[^/]+\/src\//.test(f)))

// one log scan: newest-first, first sighting of a file = its last-change date
const log = execSync('git log --format=@@@%ct --name-only -- packages tools', {
  cwd: ROOT,
  encoding: 'utf8',
  maxBuffer: 64 * 1024 * 1024
})
const lastTouch = new Map()
for (const line of log.split('\n')) {
  if (line.startsWith('@@@')) {
    var ts = Number(line.slice(3)) * 1000
    continue
  }
  const f = line.trim()
  if (f && !lastTouch.has(f)) lastTouch.set(f, ts)
}

const now = Date.now()
const rows = tracked.map((f) => ({ file: f, age: lastTouch.has(f) ? DAYS(now - lastTouch.get(f)) : -1 })).sort((a, b) => b.age - a.age)

console.log(`coldest source files (by days since last commit, top ${LIMIT}):\n`)
for (const r of rows.slice(0, LIMIT)) {
  console.log(`${String(r.age).padStart(5)}d  ${r.file}`)
}
const cold = rows.filter((r) => r.age > 180).length
console.log(`\n${cold} of ${rows.length} files untouched for >180 days`)
