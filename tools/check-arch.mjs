#!/usr/bin/env node
// Architecture ratchet — two halves:
//
//   A) zero-tolerance grep gates: structural invariants that must never be
//      bypassed outside an explicit allowlist (raw broadcaster construction,
//      raw key material in app code, scattered process.env reads, deep
//      package specifiers). Allowlisting a file is a deliberate, reviewed act.
//
//   B) metrics ratchet: consumes `depcruise --output-type json` plus git
//      ls-files LOC and compares per-package / per-file coupling, size and
//      instability against tools/.arch-baseline.json. Counts may only shrink;
//      `--update` re-baselines after a deliberate refactor. Mirrors
//      check-leftovers.mjs conventions (same glob engine, same gate style).
import { execSync } from 'node:child_process'
import { readFileSync, writeFileSync, existsSync } from 'node:fs'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..')
const BASELINE = join(ROOT, 'tools', '.arch-baseline.json')
const UPDATE = process.argv.includes('--update')

// ---------------------------------------------------------------------------
// file inventory (mirrors check-leftovers: git-tracked sources only)
// ---------------------------------------------------------------------------
const SRC_GLOB = globToRe('packages/*/src/**/*.{ts,mts,mjs}')
const SRC_EXCLUDE = /\.runar\.ts$|\.d\.ts$|\/vendor\/|\/test\//

const files = execSync('git ls-files', { cwd: ROOT, encoding: 'utf8' })
  .split('\n')
  .map((f) => f.trim())
  .filter(Boolean)
const srcFiles = files.filter((f) => SRC_GLOB.test(f) && !SRC_EXCLUDE.test(f))

function globToRe(glob) {
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

const pkgOf = (f) => (f.match(/^packages\/([^/]+)\//) ?? [])[1]
const cmpStr = (a, b) => (a < b ? -1 : a > b ? 1 : 0)

// ---------------------------------------------------------------------------
// A) zero-tolerance grep gates — a hit in any non-allowlisted file fails
// ---------------------------------------------------------------------------
// e2e and watcher are the harness tier (regtest/live drivers) — they own keys
// and broadcasters by design and are excluded wholesale from custody gates.
const GATED = /packages\/(?!e2e\/|watcher\/)/
const GATES = [
  {
    name: 'raw-broadcaster-construction',
    re: /new\s+(TeranodeBroadcaster|BsvBroadcaster|GorillaPoolBroadcaster|WhatsOnChainBroadcaster)\s*\(/g,
    scope: GATED,
    // composition roots only: mcp runtime (SpendGuard-wrapped), chain regtest
    // factory + testkit (harness support). Anything else bypasses SpendGuard.
    allow: ['packages/mcp/src/runtime.ts', 'packages/chain/src/regtest.ts', 'packages/chain/src/testkit.ts']
  },
  {
    name: 'raw-key-material',
    re: /(PrivateKey\.fromWif|PrivateKey\.fromHex|new\s+PrivateKey\s*\()/g,
    scope: GATED,
    // keystore bootstrap + composition roots. server.ts rotate keeps a legacy
    // WIF arg flagged for removal; toolbox re-wraps a PrivateKey for the SDK
    // key deriver. No NEW site may introduce key material into app code.
    allow: [
      'packages/mcp/src/init.ts',
      'packages/mcp/src/runtime.ts',
      'packages/mcp/src/pay.ts',
      'packages/mcp/src/server.ts',
      'packages/automation/src/toolbox.ts',
      'packages/chain/src/testkit.ts',
      // custody signer's own keygen — derives scalars into keys internally
      'packages/identity/src/custody.ts'
    ]
  },
  {
    name: 'env-read',
    re: /process\.env\.NEKTAR_/g,
    scope: GATED,
    // all NEKTAR_* reads go through @nektar/core nektarEnv() — injected
    // `env = process.env` params and env.X reads are the testable seam.
    // env.ts is the sanctioned surface itself.
    allow: ['packages/core/src/env.ts']
  },
  {
    name: 'env-write',
    re: /process\.env(?:\.\w+|\[[^\]]+\])\s*=[^=]/g,
    scope: GATED,
    allow: []
  },
  {
    name: 'deep-package-specifier',
    re: /from\s+['"]@nektar\/[a-z-]+\//g,
    scope: /./,
    // packages must be consumed via their index — @nektar/contracts/*.json is generated
    // artifacts addressed by file, not a package-internal deep import.
    allowSpecifier: /@nektar\/contracts\//
  }
]

let fail = false
for (const gate of GATES) {
  const hits = []
  for (const f of srcFiles) {
    if (!gate.scope.test(f)) continue
    if ((gate.allow ?? []).includes(f)) continue
    let text
    try {
      text = readFileSync(join(ROOT, f), 'utf8')
    } catch {
      continue
    }
    const n = (text.match(gate.re) ?? []).filter((m) => !(gate.allowSpecifier && gate.allowSpecifier.test(m))).length
    if (n > 0) hits.push(`${f} (${n})`)
  }
  if (hits.length > 0) {
    console.error(`✗ ${gate.name}: ${hits.length} file(s) outside the allowlist`)
    for (const h of hits) console.error(`    ${h}`)
    fail = true
  }
}

// ---------------------------------------------------------------------------
// B) metrics — depcruise JSON + LOC
// ---------------------------------------------------------------------------
const cruiseJson = JSON.parse(
  execSync('npx depcruise packages --config .dependency-cruiser.cjs --output-type json', {
    cwd: ROOT,
    encoding: 'utf8',
    maxBuffer: 64 * 1024 * 1024
  })
)
const modules = cruiseJson.modules ?? cruiseJson.output?.modules ?? []
const srcModules = modules.filter((m) => SRC_GLOB.test(m.source.replaceAll('\\', '/')))

const pkgLoc = {}
const fileLoc = {}
for (const f of srcFiles) {
  let n = 0
  try {
    n = (readFileSync(join(ROOT, f), 'utf8').match(/\n/g) ?? []).length + 1
  } catch {
    continue
  }
  fileLoc[f] = n
  const p = pkgOf(f)
  pkgLoc[p] = (pkgLoc[p] ?? 0) + n
}

// Cross-package edges CANNOT come from depcruise's resolved paths: workspace
// @nektar/x imports resolve through node_modules into dist/ and the exclude
// filter drops them from the graph entirely (verified: 0 @nektar edges in the
// cruise JSON). Measure them at the specifier level instead — which is also
// where the layering contract actually lives.
const IMPORT_RE = /(?:import|export)\s[^'"]*?from\s+['"]([^'"]+)['"]|import\s*\(\s*['"]([^'"]+)['"]|require\s*\(\s*['"]([^'"]+)['"]/g
const NEKTAR_SPEC_RE = /^@nektar\/([a-z-]+)$/

const pkgCa = {}
const pkgCe = {}
const pkgEdges = {} // pkg -> Set of target packages
const fileFanout = {}
const graph = {} // source -> [resolved] for SCC audit (ALL dep types incl. type-only)
const specEdges = {} // pkg -> Map(targetPkg -> [specifier,...])
for (const m of srcModules) {
  const src = m.source.replaceAll('\\', '/')
  fileFanout[src] = m.dependencies.length
  graph[src] = []
  for (const d of m.dependencies) {
    const r = (d.resolved ?? '').replaceAll('\\', '/')
    if (!r.startsWith('packages/')) continue
    graph[src].push(r)
  }
}
// specifier-level package edges: @nektar/x imports add to fanout (they are
// absent from the cruise graph) and build the real inter-package matrix
for (const f of srcFiles) {
  const srcPkg = pkgOf(f)
  let text
  try {
    text = readFileSync(join(ROOT, f), 'utf8')
  } catch {
    continue
  }
  const nektarPkgs = new Set()
  for (const mm of text.matchAll(IMPORT_RE)) {
    const spec = /** @type {string | undefined} */ (mm[1] ?? mm[2] ?? mm[3])
    const hit = spec !== undefined ? NEKTAR_SPEC_RE.exec(spec) : null
    if (hit === null) continue
    const dstPkg = hit[1]
    if (dstPkg === srcPkg) continue
    nektarPkgs.add(dstPkg)
    ;(specEdges[srcPkg] ??= new Map()).set(dstPkg, [...(specEdges[srcPkg].get(dstPkg) ?? []), f])
  }
  fileFanout[f] = (fileFanout[f] ?? 0) + nektarPkgs.size
  pkgEdges[srcPkg] = new Set([...(pkgEdges[srcPkg] ?? []), ...nektarPkgs])
}
for (const [p, targets] of Object.entries(pkgEdges)) {
  pkgCe[p] = targets.size
  for (const t of targets) pkgCa[t] = (pkgCa[t] ?? 0) + 1
}

// Hard layering contract — the sharp boundaries that must hold regardless of
// baseline drift. depcruise can't see these edges (workspace imports resolve
// into dist/ and drop out of its graph), so the contract is enforced here at
// specifier level. Softer boundaries are covered by the edge ratchet below:
// any NEW cross-package edge fails until admitted via --update.
const ROOTS = new Set(['core', 'crypto', 'log'])
const FORBIDDEN = [
  {
    name: 'nothing-imports-e2e',
    test: (src, dst) => src !== 'e2e' && dst === 'e2e',
    why: 'e2e is a test harness — shipping code must not depend on it'
  },
  {
    name: 'automation-not-mcp',
    test: (src, dst) => src === 'automation' && dst === 'mcp',
    why: 'automation reaches its host only through the AutomationHost interface'
  },
  {
    name: 'store-stays-leaf',
    test: (src, dst) => src === 'store' && !ROOTS.has(dst),
    why: 'store is leaf blob/pack persistence — only core-tier deps allowed'
  },
  {
    name: 'roots-stay-roots',
    test: (src, dst) => ROOTS.has(src) && !ROOTS.has(dst),
    why: 'core/crypto/log are the foundation — they may only import each other'
  },
  {
    name: 'chain-not-appward',
    test: (src, dst) => src === 'chain' && ['bstack', 'reasoning', 'automation', 'mcp', 'watcher'].includes(dst),
    why: 'chain is a lower-level lib — application packages sit above it'
  }
]
const SPEC_GATED = /packages\/(?!e2e\/|watcher\/)/ // harness tier may reach anywhere
for (const [p, targets] of Object.entries(specEdges)) {
  if (!SPEC_GATED.test(`packages/${p}/`)) continue
  for (const [t, filesHit] of targets) {
    for (const rule of FORBIDDEN) {
      if (!rule.test(p, t)) continue
      console.error(`✗ ${rule.name}: ${p} → ${t} (${rule.why})`)
      for (const f of filesHit.slice(0, 4)) console.error(`    ${f}`)
      fail = true
    }
  }
}

const metrics = { packages: {}, files: {}, typeOnlyCycles: [] }
for (const p of new Set([...Object.keys(pkgLoc), ...Object.keys(pkgEdges)])) {
  const ca = pkgCa[p] ?? 0
  const ce = pkgCe[p] ?? 0
  const pFiles = srcFiles.filter((f) => pkgOf(f) === p)
  metrics.packages[p] = {
    loc: pkgLoc[p] ?? 0,
    pkgFanout: ce,
    edges: [...(pkgEdges[p] ?? [])].sort(cmpStr),
    maxFileLoc: Math.max(0, ...pFiles.map((f) => fileLoc[f] ?? 0)),
    maxFileFanout: Math.max(0, ...pFiles.map((f) => fileFanout[f] ?? 0)),
    instability: ca + ce === 0 ? null : Number((ce / (ca + ce)).toFixed(3))
  }
}
// god-file tracking: only files over either threshold enter the ratchet —
// everything below is free to grow until it becomes structurally interesting
const LOC_CAP = 800
const FANOUT_CAP = 20
for (const f of srcFiles) {
  const loc = fileLoc[f] ?? 0
  const fan = fileFanout[f] ?? 0
  if (loc >= LOC_CAP || fan >= FANOUT_CAP) metrics.files[f] = { loc, fanout: fan }
}

// type-only-inclusive cycle audit (Tarjan over the full module graph —
// depcruise's no-circular rule exempts type-only edges; this sees them)
{
  const idx = {},
    low = {},
    onStack = new Set(),
    stack = []
  let counter = 0
  const sccs = []
  function strongconnect(v) {
    idx[v] = low[v] = counter++
    stack.push(v)
    onStack.add(v)
    for (const w of graph[v] ?? []) {
      if (!(w in idx)) {
        strongconnect(w)
        low[v] = Math.min(low[v], low[w])
      } else if (onStack.has(w)) {
        low[v] = Math.min(low[v], idx[w])
      }
    }
    if (low[v] === idx[v]) {
      const scc = []
      let w
      do {
        w = stack.pop()
        onStack.delete(w)
        scc.push(w)
      } while (w !== v)
      if (scc.length > 1) sccs.push(scc.sort(cmpStr))
    }
  }
  for (const v of Object.keys(graph)) if (!(v in idx)) strongconnect(v)
  metrics.typeOnlyCycles = sccs.map((s) => s.join('|')).sort(cmpStr)
}

// ---------------------------------------------------------------------------
// baseline compare — same shrink-only contract as check-leftovers
// ---------------------------------------------------------------------------
const baseline = existsSync(BASELINE) ? JSON.parse(readFileSync(BASELINE, 'utf8')) : { packages: {}, files: {}, typeOnlyCycles: [] }

if (UPDATE) {
  writeFileSync(BASELINE, JSON.stringify(metrics, null, 2) + '\n')
  // keep the baseline prettier-clean so --update never trips check:format
  execSync(`npx prettier --write "${BASELINE}"`, { cwd: ROOT, stdio: 'ignore' })
  console.log(
    `arch baseline written: ${Object.keys(metrics.packages).length} packages, ${Object.keys(metrics.files).length} tracked files, ${metrics.typeOnlyCycles.length} type-only cycles`
  )
  process.exit(0)
}

for (const [p, m] of Object.entries(metrics.packages)) {
  const b = baseline.packages?.[p]
  if (b === undefined) {
    console.error(`✗ new package ${p} — run --update to admit it deliberately`)
    fail = true
    continue
  }
  for (const k of ['loc', 'pkgFanout', 'maxFileLoc', 'maxFileFanout']) {
    if (m[k] > b[k]) {
      console.error(`✗ ${p}.${k} grew: ${b[k]} → ${m[k]}`)
      fail = true
    }
  }
  const newEdges = (m.edges ?? []).filter((e) => !(b.edges ?? []).includes(e))
  for (const e of newEdges) {
    console.error(`✗ new package edge ${p} → ${e} — admit it deliberately via --update`)
    fail = true
  }
  if (m.instability !== null && b.instability !== null && Math.abs(m.instability - b.instability) > 0.25) {
    console.error(`✗ ${p}.instability drifted: ${b.instability} → ${m.instability}`)
    fail = true
  }
}
for (const [f, m] of Object.entries(metrics.files)) {
  const b = baseline.files?.[f]
  if (b === undefined) {
    console.error(`✗ ${f} crossed a god-file threshold (loc≥${LOC_CAP} or fanout≥${FANOUT_CAP}): loc=${m.loc} fanout=${m.fanout}`)
    fail = true
    continue
  }
  if (m.loc > b.loc) {
    console.error(`✗ ${f} loc grew: ${b.loc} → ${m.loc}`)
    fail = true
  }
  if (m.fanout > b.fanout) {
    console.error(`✗ ${f} fanout grew: ${b.fanout} → ${m.fanout}`)
    fail = true
  }
}
const newCycles = metrics.typeOnlyCycles.filter((c) => !(baseline.typeOnlyCycles ?? []).includes(c))
if (newCycles.length > 0) {
  console.error(`✗ ${newCycles.length} new type-inclusive dependency cycle(s):`)
  for (const c of newCycles) console.error(`    ${c}`)
  fail = true
}

console.log(
  `arch: ${Object.keys(metrics.packages).length} packages, ${Object.keys(metrics.files).length} tracked files, ${metrics.typeOnlyCycles.length} type-only cycles, ${GATES.length} grep gates`
)
if (fail) {
  console.error('architecture ratchet failed — shrink it, or run `node tools/check-arch.mjs --update` to re-baseline deliberately')
  process.exit(1)
}
