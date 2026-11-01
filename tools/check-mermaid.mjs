// pnpm check:mermaid — renders every ```mermaid block in the given markdown
// files with the official mermaid-cli image; any parse/render error fails.
import { readFileSync, writeFileSync, mkdirSync, rmSync, readdirSync } from 'node:fs'
import { execFileSync } from 'node:child_process'
import { join, resolve } from 'node:path'

const files = process.argv.slice(2)
const out = resolve('.state/mermaid'); rmSync(out, { recursive: true, force: true }); mkdirSync(out, { recursive: true })
let n = 0
for (const f of files) {
  for (const m of readFileSync(f, 'utf8').matchAll(/```mermaid\n([\s\S]*?)```/g)) writeFileSync(join(out, `d${String(++n).padStart(2, '0')}.mmd`), m[1])
}
let failed = 0
for (const d of readdirSync(out).filter(x => x.endsWith('.mmd'))) {
  try {
    execFileSync('docker', ['run', '--rm', '-u', 'root', '-v', `${out}:/data`, 'minlag/mermaid-cli:11.4.2', '-i', `/data/${d}`, '-o', `/data/${d}.svg`, '-q'],
      { stdio: 'pipe', timeout: 90_000, env: { ...process.env, MSYS_NO_PATHCONV: '1' } })
    console.log(`✔ ${d}`)
  } catch (e) { failed++; console.log(`✖ ${d}: ${String(e.stderr ?? e).split('\n').filter(Boolean).slice(0, 4).join(' | ')}`) }
}
console.log(failed === 0 ? `check:mermaid OK — ${n} diagrams rendered` : `check:mermaid FAIL: ${failed}/${n}`)
process.exit(failed === 0 ? 0 : 1)
