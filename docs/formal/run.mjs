// pnpm formal — model-check docs/formal/AORG.tla with TLC inside Docker
// (no local Java needed). Pass: the implementation model has no violation AND
// the mutant (sealed vertices editable) is caught violating AppendOnly.
import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, writeFileSync } from 'node:fs'
import { createHash } from 'node:crypto'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const jar = join(here, '.tools', 'tla2tools.jar')
const JAR_URL = 'https://github.com/tlaplus/tlaplus/releases/download/v1.7.4/tla2tools.jar'
const JAR_SHA256 = '936a262061c914694dfd669a543be24573c45d5aa0ff20a8b96b23d01e050e88'

if (!existsSync(jar)) {
  mkdirSync(dirname(jar), { recursive: true })
  const res = await fetch(JAR_URL, { signal: AbortSignal.timeout(60_000) })
  if (!res.ok) throw new Error(`download tla2tools.jar: HTTP ${res.status}`)
  writeFileSync(jar, Buffer.from(await res.arrayBuffer()))
}
const sha = createHash('sha256').update((await import('node:fs')).readFileSync(jar)).digest('hex')
if (sha !== JAR_SHA256) throw new Error(`tla2tools.jar sha256 mismatch: ${sha}`)

function tlc (cfg) {
  try {
    return { ok: true, out: execFileSync('docker', ['run', '--rm', '-v', `${here}:/spec`, '-w', '/spec', 'eclipse-temurin:21-jre',
      'java', '-cp', '.tools/tla2tools.jar', 'tlc2.TLC', '-workers', '2', '-deadlock', '-config', cfg, 'AORG.tla'],
    { encoding: 'utf8', timeout: 110_000, env: { ...process.env, MSYS_NO_PATHCONV: '1' } }) }
  } catch (e) { return { ok: false, out: String(e.stdout ?? '') + String(e.stderr ?? '') } }
}

const impl = tlc('AORG.cfg')
const states = /(\d+) distinct states found/.exec(impl.out)?.[1]
console.log(`AORG.cfg: ${impl.ok && /No error has been found/.test(impl.out) ? 'PASS' : 'FAIL'} (${states ?? '?'} distinct states)`)
const mut = tlc('AORG_mutant.cfg')
const caught = !mut.ok && /Invariant AppendOnly is violated/.test(mut.out)
console.log(`AORG_mutant.cfg: ${caught ? 'PASS (AppendOnly violation found, as required)' : 'FAIL (mutant not caught)'}`)
if (!(impl.ok && caught)) { console.log(impl.out.slice(-1500)); console.log(mut.out.slice(-1500)); process.exit(1) }
