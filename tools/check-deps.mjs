// pnpm check:deps — every bare import in packages/*/{src,test} must be a
// declared dependency of that package (deps, devDeps or peerDeps). Hoisting
// hides these locally; a clean checkout (Docker/CI) fails on them.
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { builtinModules } from 'node:module'
import { join } from 'node:path'

const root = new URL('../packages/', import.meta.url).pathname.replace(/^\/([A-Za-z]:)/, '$1')
const builtins = new Set(builtinModules)
const walk = dir => readdirSync(dir).flatMap(f => { const p = join(dir, f); return statSync(p).isDirectory() ? walk(p) : /\.(ts|mjs)$/.test(f) ? [p] : [] })
let missing = 0
for (const pkg of readdirSync(root)) {
  try { statSync(join(root, pkg, 'package.json')) } catch { continue }
  const pj = JSON.parse(readFileSync(join(root, pkg, 'package.json'), 'utf8'))
  const declared = new Set([pj.name, ...Object.keys({ ...pj.dependencies, ...pj.devDependencies, ...pj.peerDependencies })])
  const files = ['src', 'test', 'scripts', 'demo'].flatMap(d => { try { return walk(join(root, pkg, d)) } catch { return [] } })
  const used = new Map()
  for (const f of files) {
    // relative paths into node_modules only work by accident of hoisting
    for (const m of readFileSync(f, 'utf8').matchAll(/(?:from\s+|import\s*\(\s*)['"](\.[^'"]*node_modules[^'"]*)['"]/g)) { console.log(`✖ ${pj.name}: ${f.slice(root.length)} imports through node_modules (${m[1]}) — use the package's exports`); missing++ }
    for (const m of readFileSync(f, 'utf8').matchAll(/(?:from\s+|import\s*\(\s*|import\s+)['"]([^'"./][^'"]*)['"]/g)) {
      const spec = m[1]
      if (spec.startsWith('node:') || builtins.has(spec)) continue
      const name = spec.startsWith('@') ? spec.split('/').slice(0, 2).join('/') : spec.split('/')[0]
      if (!declared.has(name)) used.set(name, f.slice(root.length))
    }
  }
  for (const [name, f] of used) { console.log(`✖ ${pj.name}: imports '${name}' (e.g. ${f}) but does not declare it`); missing++ }
}
console.log(missing === 0 ? 'check:deps OK — every import is declared' : `check:deps FAIL: ${missing} undeclared`)
process.exit(missing === 0 ? 0 : 1)
