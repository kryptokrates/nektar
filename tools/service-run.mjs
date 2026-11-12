#!/usr/bin/env node
// tools/service-run.mjs — what the installed service actually starts: the
// nektar server plus, when NEKTAR_AUTORESPOND=1 (or --autorespond), the
// want-responder loop once the instance reports healthy.
import { spawn } from 'node:child_process'
import { existsSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = resolve(fileURLToPath(new URL('..', import.meta.url)))
// load .env like bin.js does — schtasks/launchd services inherit a bare env
const envPath = join(root, '.env')
if (existsSync(envPath)) {
  const { readFileSync } = await import('node:fs')
  for (const line of readFileSync(envPath, 'utf8').split(/\r?\n/)) {
    const m = line.trim().match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/)
    if (m && process.env[m[1]] === undefined) process.env[m[1]] = m[2].trim()
  }
}
const binJs = join(root, 'packages/mcp/dist/src/bin.js')
if (!existsSync(binJs)) {
  console.error('service-run: no build — run pnpm -r build first')
  process.exit(1)
}

const child = spawn(process.execPath, [binJs], { cwd: root, stdio: 'inherit' })
child.on('exit', (c) => process.exit(c ?? 0))
const stop = () => child.kill('SIGTERM')
process.on('SIGINT', stop)
process.on('SIGTERM', stop)

const autorespond =
  (process.argv.includes('--autorespond') || /^1|true|yes$/i.test(process.env.NEKTAR_AUTORESPOND ?? '')) &&
  !/^(1|true|yes)$/i.test(process.env.NEKTAR_AUTOMATION ?? '')
const port = process.env.NEKTAR_HTTP_PORT
if (autorespond && port) {
  // wait for health, then run the responder loop as a supervised child
  const ready = async () => {
    for (let i = 0; i < 80; i++) {
      try {
        if ((await fetch(`http://127.0.0.1:${port}/v1/health`, { signal: AbortSignal.timeout(400) })).ok) return true
      } catch {}
      await new Promise((r) => setTimeout(r, 300))
    }
    return false
  }
  const startLoop = () => {
    const loop = spawn(process.execPath, [join(root, 'tools/autorespond.mjs')], { cwd: root, stdio: 'inherit' })
    loop.on('exit', () => setTimeout(startLoop, 10_000)) // respawn on crash
    process.on('SIGINT', () => loop.kill('SIGTERM'))
    process.on('SIGTERM', () => loop.kill('SIGTERM'))
  }
  ready().then((ok) => {
    ok
      ? (console.log('service-run: instance healthy — starting autorespond'), startLoop())
      : console.error('service-run: instance never became healthy — autorespond skipped')
  })
} else if (autorespond) {
  console.error('service-run: NEKTAR_AUTORESPOND set but no NEKTAR_HTTP_PORT — autorespond cannot reach the instance')
}
