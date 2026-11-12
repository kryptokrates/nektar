#!/usr/bin/env node
// Debt ratchet — counts leftover patterns (as any, ts-suppress, TODO markers,
// console output, commented-out code) per directory and compares against
// tools/.leftover-baseline.json. Fails when any bucket grows; `--update`
// regenerates the baseline after a cleanup pass. Zero deps by design.
import { execSync } from 'node:child_process'
import { readFileSync, writeFileSync, existsSync } from 'node:fs'
import { join, dirname, relative } from 'node:path'
import { fileURLToPath } from 'node:url'

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..')
const BASELINE = join(ROOT, 'tools', '.leftover-baseline.json')
const UPDATE = process.argv.includes('--update')

const SRC_GLOBS = ['packages/*/src/**/*.{ts,mts,mjs}', 'tools/**/*.mjs']
const EXCLUDE = /node_modules|\/dist\/|\/artifacts\/|\/\.state\/|\/\.mod\/|vendor\/|dist-bundle|\.runar\.ts|\.d\.ts$/

const PATTERNS = {
  'as-any': { re: /\bas any\b/g, files: SRC_GLOBS },
  'ts-suppress': { re: /@ts-ignore|@ts-expect-error/g, files: ['packages/**/*.{ts,mts}'] },
  markers: { re: /\b(TODO|FIXME|HACK|XXX)\b/g, files: SRC_GLOBS },
  'debug-output': {
    re: /console\.(log|debug)\(/g,
    files: ['packages/*/src/**/*.{ts,mts}'],
    exclude: /e2e|init\.ts|watcher\.mjs|bin\.ts|demo/
  },
  'commented-code': {
    re: /^\s*\/\/\s*(const|let|var|function|return|await|import|export)\s/gm,
    files: SRC_GLOBS
  }
}

const files = execSync('git ls-files', { cwd: ROOT, encoding: 'utf8' })
  .split('\n')
  .map((f) => f.trim())
  .filter(Boolean)

const globToRe = (glob) => {
  let re = ''
  let i = 0
  while (i < glob.length) {
    const c = glob[i]
    if (c === '*' && glob[i + 1] === '*' && glob[i + 2] === '/') {
      re += '(?:[^/]+/)*'
      i += 3
    } else if (c === '*' && glob[i + 1] === '*') {
      re += '.*'
      i += 2
    } else if (c === '*') {
      re += '[^/]*'
      i += 1
    } else if (c === '{') {
      const end = glob.indexOf('}', i)
      re +=
        '(?:' +
        glob
          .slice(i + 1, end)
          .split(',')
          .join('|') +
        ')'
      i = end + 1
    } else {
      re += c.replace(/[.+?^$()|[\]\\]/g, '\\$&')
      i += 1
    }
  }
  return new RegExp('^' + re + '$')
}

const results = {}
for (const [name, spec] of Object.entries(PATTERNS)) {
  const matchers = spec.files.map(globToRe)
  const buckets = {}
  for (const f of files) {
    if (EXCLUDE.test(f) || (spec.exclude && spec.exclude.test(f))) continue
    if (!matchers.some((re) => re.test(f))) continue
    let text
    try {
      text = readFileSync(join(ROOT, f), 'utf8')
    } catch {
      continue
    }
    const n = (text.match(spec.re) ?? []).length
    if (n === 0) continue
    const dir = f.split('/').slice(0, 2).join('/')
    buckets[dir] = (buckets[dir] ?? 0) + n
  }
  results[name] = buckets
}

const baseline = existsSync(BASELINE) ? JSON.parse(readFileSync(BASELINE, 'utf8')) : {}

if (UPDATE) {
  writeFileSync(BASELINE, JSON.stringify(results, null, 2) + '\n')
  console.log(`baseline written: ${Object.keys(results).length} patterns`)
  process.exit(0)
}

let fail = false
for (const [name, buckets] of Object.entries(results)) {
  const base = baseline[name] ?? {}
  for (const [dir, n] of Object.entries(buckets)) {
    const prev = base[dir] ?? 0
    if (n > prev) {
      console.error(`✗ ${name} grew in ${dir}: ${prev} → ${n}`)
      fail = true
    }
  }
}
const total = Object.values(results).reduce((sum, buckets) => sum + Object.values(buckets).reduce((s, n) => s + n, 0), 0)
console.log(`leftovers: ${total} tracked markers across ${Object.keys(results).length} patterns`)
if (fail) {
  console.error('leftover debt grew — fix it or run `node tools/check-leftovers.mjs --update` to re-baseline deliberately')
  process.exit(1)
}
