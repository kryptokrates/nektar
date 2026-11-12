import { compile } from 'runar-compiler'
import fs from 'node:fs'
for (const name of process.argv.slice(2)) {
  const r = compile(fs.readFileSync(name, 'utf8'))
  console.log(`=== ${name}: success=${r.success}`)
  if (!r.success) {
    console.log(JSON.stringify(r.diagnostics).slice(0, 3000))
    continue
  }
  const a = r.artifact
  console.log('abi methods:', JSON.stringify((a.abi?.methods ?? a.methods ?? []).map((m) => m.name)))
  const script = a.script ?? ''
  console.log('script bytes ~', Math.floor(script.length / 2))
}
