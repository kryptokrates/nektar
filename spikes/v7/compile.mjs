import { compile } from '../../packages/anchor/node_modules/runar-compiler/dist/index.js'
import fs from 'node:fs'
for (const name of ['probe', 'probe-sale']) {
  const r = compile(fs.readFileSync(`${name}.runar.ts`, 'utf8'))
  console.log(`=== ${name}: success=${r.success}`)
  if (!r.success) {
    console.log(JSON.stringify(r.diagnostics, null, 1).slice(0, 3000))
    continue
  }
  const a = r.artifact
  console.log('methods:', (a.abi?.methods ?? a.methods ?? []).map((m) => m.name ?? m).join(', '))
  const script = a.script ?? a.hex ?? ''
  console.log('script bytes ~', Math.floor(script.length / 2))
  fs.writeFileSync(
    `${name}.json`,
    JSON.stringify(a, (k, v) => (typeof v === 'bigint' ? v.toString() : v), 2)
  )
}
