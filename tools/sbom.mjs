// pnpm sbom — CycloneDX 1.5 JSON SBOM from `pnpm list` (every resolved
// package, with version + purl + license where pnpm reports it). SEC-006.
import { execFileSync } from 'node:child_process'
import { writeFileSync, readFileSync } from 'node:fs'
import { createHash } from 'node:crypto'

const raw = execFileSync(process.platform === 'win32' ? 'pnpm.cmd' : 'pnpm', ['list', '-r', '--depth', 'Infinity', '--json', '--prod'], { encoding: 'utf8', maxBuffer: 1 << 28, shell: process.platform === 'win32' })
const projects = JSON.parse(raw)
const comps = new Map()
const walk = deps => {
  for (const [name, d] of Object.entries(deps ?? {})) {
    if (d.version?.startsWith('link:')) { walk(d.dependencies); continue }
    const key = `${name}@${d.version}`
    if (!comps.has(key)) {
      comps.set(key, {
        type: 'library', name, version: d.version,
        purl: `pkg:npm/${name.replace('@', '%40')}@${d.version}`,
        ...(d.license ? { licenses: [{ license: { id: d.license } }] } : {})
      })
      walk(d.dependencies)
    }
  }
}
for (const p of projects) walk(p.dependencies)
const lock = readFileSync(new URL('../pnpm-lock.yaml', import.meta.url))
const bom = {
  bomFormat: 'CycloneDX', specVersion: '1.5', version: 1,
  metadata: {
    timestamp: new Date().toISOString(),
    component: { type: 'application', name: 'mamem', version: JSON.parse(readFileSync(new URL('../package.json', import.meta.url), 'utf8')).version ?? '0.0.0' },
    properties: [{ name: 'pnpm-lock.sha256', value: createHash('sha256').update(lock).digest('hex') }]
  },
  components: [...comps.values()].sort((a, b) => (a.name + a.version).localeCompare(b.name + b.version))
}
writeFileSync(new URL('../sbom.cdx.json', import.meta.url), JSON.stringify(bom, null, 1))
console.log(`sbom.cdx.json: ${bom.components.length} components (lock ${bom.metadata.properties[0].value.slice(0, 12)}…)`)
