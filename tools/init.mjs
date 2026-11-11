#!/usr/bin/env node
// tools/init.mjs — first-run wizard: pick a network → where your agent
// lives → passphrase → write .env → print the exact next command.
//
//   node tools/init.mjs            interactive
//   node tools/init.mjs --yes      accept all defaults (mock, ./.nektar, stdio)
//   node tools/init.mjs --env ttn --dir .ttn --port 8080
import { createInterface } from 'node:readline'
import { existsSync, mkdirSync, writeFileSync, readFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = resolve(fileURLToPath(new URL('..', import.meta.url)))
const arg = f => { const i = process.argv.indexOf(f); return i >= 0 ? process.argv[i + 1] : undefined }
const YES = process.argv.includes('--yes') || !process.stdin.isTTY // piped/non-interactive → defaults

// ─── prompt helpers ─────────────────────────────────────────────────────────
const rl = createInterface({ input: process.stdin, output: process.stdout })
const ask = q => new Promise(res => rl.question(q, a => res(a.trim())))
const askSecret = q => new Promise(res => {
  const out = rl.output
  const orig = out.write.bind(out)
  out.write = (chunk, ...rest) => (typeof chunk === 'string' && chunk.includes('\n') ? orig(chunk, ...rest) : true)
  rl.question(q, a => { out.write = orig; res(a.trim()) })
})
const pick = async (q, opts) => {
  if (YES) return opts[0]
  const line = await ask(`${q}\n${opts.map((o, i) => `  ${i + 1}) ${o.label}`).join('\n')}\n> `)
  const n = Number(line)
  return (n >= 1 && n <= opts.length) ? opts[n - 1] : (opts.find(o => o.label.toLowerCase().startsWith(line.toLowerCase())) ?? opts[0])
}

// ─── existing config → reconfigure defaults (Hermes pattern: current values
//     ride as prompt defaults so a re-run only changes what you touch) ───────
const envPath = join(root, '.env')
const readEnv = p => existsSync(p)
  ? Object.fromEntries(readFileSync(p, 'utf8').split(/\r?\n/).map(l => l.match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/)).filter(Boolean).map(m => [m[1], m[2].trim()]))
  : {}
const prev = readEnv(envPath)
const reconfigure = Boolean(prev.NEKTAR_NETWORK)

console.log('\nnektar init — your agent, your keys, verifiable memory.\n')
console.log(reconfigure
  ? 'Existing config found — press Enter to keep each current value.\n'
  : 'This walks the four decisions a fresh install needs, writes .env,\nand prints the one command to run next. Ctrl+C to bail anytime.\n')

// ─── 1. network ─────────────────────────────────────────────────────────────
const networks = [
  { id: 'mock', label: 'mock — offline, zero setup, auto-mined (best first run)' },
  { id: 'ttn', label: 'ttn — Teranode Test Net, test coins, real market' },
  { id: 'main', label: 'main — real BSV. Read docs/OPERATIONS.md first' },
  { id: 'regtest', label: 'regtest — local Teranode stack (devs)' }
]
const net = arg('--env') ?? (prev.NEKTAR_NETWORK && YES ? prev.NEKTAR_NETWORK : (await pick('1/4 — which network?', prev.NEKTAR_NETWORK ? [...networks.filter(n => n.id === prev.NEKTAR_NETWORK), ...networks.filter(n => n.id !== prev.NEKTAR_NETWORK)] : networks)).id)
if (!networks.some(n => n.id === net)) { console.error(`unknown network "${net}"`); process.exit(2) }

// ─── 2. data dir ────────────────────────────────────────────────────────────
const defDir = prev.NEKTAR_DATA_DIR ?? (net === 'mock' ? '.nektar' : `.nektar-${net}`)
const dir = arg('--dir') ?? ((YES ? defDir : await ask(`2/4 — where does your agent live? (identity + memory)\n  This directory IS the agent — back it up.\n[${defDir}] > `)) || defDir)
const dataDir = resolve(dir)
mkdirSync(dataDir, { recursive: true })

// ─── 3. passphrase ──────────────────────────────────────────────────────────
let pass = arg('--passphrase')
if (pass === undefined && !YES) {
  pass = await askSecret(`3/4 — key passphrase (encrypts your identity at rest; unrecoverable)\n  ${net === 'main' ? 'REQUIRED on main.' : 'empty = skip (mock is fine without)'}${prev.NEKTAR_KEY_PASSPHRASE ? ' — Enter keeps the current one' : ''}\n> `)
  if (!pass && prev.NEKTAR_KEY_PASSPHRASE) pass = prev.NEKTAR_KEY_PASSPHRASE
} else pass ??= prev.NEKTAR_KEY_PASSPHRASE ?? ''

// ─── 4. port ────────────────────────────────────────────────────────────
const port = arg('--port') ?? prev.NEKTAR_HTTP_PORT

// ─── write .env ─────────────────────────────────────────────────────────────
const lines = [
  `# nektar init — ${new Date().toISOString().slice(0, 10)} (${net})`,
  `NEKTAR_NETWORK=${net}`,
  `NEKTAR_DATA_DIR=${dataDir}`
]
if (pass) lines.push(`NEKTAR_KEY_PASSPHRASE=${pass}`)
if (port) lines.push(`NEKTAR_HTTP_PORT=${port}`)
if (net === 'ttn') lines.push(`NEKTAR_VERIFY_BASE=https://nektar.run`)
const body = lines.join('\n') + '\n'
if (existsSync(envPath)) {
  const stamp = new Date().toISOString().replace(/[:.]/g, '-')
  const bak = `${envPath}.bak-${stamp}`
  const old = (await import('node:fs')).readFileSync(envPath, 'utf8')
  writeFileSync(bak, old)
  console.log(`(existing .env backed up → ${bak})`)
  // drop only the vars init manages — bin.js takes the first occurrence, so
  // a re-init must replace those; operator vars (WIFs, tokens) stay
  const managed = ['NEKTAR_NETWORK', 'NEKTAR_DATA_DIR', 'NEKTAR_KEY_PASSPHRASE', 'NEKTAR_HTTP_PORT', 'NEKTAR_VERIFY_BASE', 'NEKTAR_MODEL_ID']
  const kept = old.split(/\r?\n/).filter(l => !managed.some(k => new RegExp(`^\\s*${k}\\s*=`).test(l))).join('\n').replace(/\n{3,}/g, '\n\n').trimEnd()
  writeFileSync(envPath, (kept ? kept + '\n\n' : '') + body)
} else writeFileSync(envPath, body)
console.log(`\nwrote ${envPath}`)

// ─── next steps ─────────────────────────────────────────────────────────────
console.log('\nNext:')
if (net === 'mock') {
  console.log('  pnpm demo          — first verified receipt in <60s, nothing spent')
  console.log('  pnpm setup:claude  — wire Claude Code (or: pnpm setup:codex)')
} else if (net === 'ttn') {
  console.log(`  node packages/mcp/dist/src/bin.js            # starts on ttn (port: ${port ?? 'stdio only'})`)
  console.log('  → call fundingStatus, send TTN coins to the address, then fundingImport <txid>')
  console.log('  → then want → deals → your first purchase on the public market')
  console.log('  pnpm setup:claude                            # wire your agent client')
} else if (net === 'main') {
  console.log('  MAINNET spends real BSV. Required before the server will start:')
  console.log('    NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND')
  console.log('    NEKTAR_FUND_WIF=<funded key>   (docker: mcp secret set …)')
  console.log('  docs/OPERATIONS.md is the runbook.')
} else {
  console.log('  regtest needs the local Teranode stack: docs/OPERATIONS.md § regtest')
}
console.log(`\nYour agent lives in ${dataDir} — that directory + its passphrase`)
console.log('is the identity. Back it up; there is no recovery.\n')

// ─── alive-check (OpenClaw: verify before you trust it) — boot the instance
//     on a temp port with the config just written, poll health, report ───────
const binJs = join(root, 'packages/mcp/dist/src/bin.js')
if (existsSync(binJs) && net !== 'main') {
  const alivePort = 18190 + Math.floor(Math.random() * 800)
  const { spawn } = await import('node:child_process')
  const child = spawn(process.execPath, [binJs], {
    env: { ...process.env, NEKTAR_NETWORK: net, NEKTAR_DATA_DIR: dataDir, NEKTAR_KEY_PASSPHRASE: pass || undefined, NEKTAR_HTTP_PORT: String(alivePort) },
    stdio: ['ignore', 'pipe', 'pipe']
  })
  child.stderr.resume(); child.stdout.resume()
  let alive = false
  for (let i = 0; i < 60; i++) {
    try { if ((await fetch(`http://127.0.0.1:${alivePort}/v1/health`, { signal: AbortSignal.timeout(400) })).ok) { alive = true; break } } catch {}
    if (child.exitCode !== null) break
    await new Promise(r => setTimeout(r, 300))
  }
  child.kill()
  console.log(alive
    ? `alive-check: instance booted on ${net} with your config — your agent is alive.\n`
    : `alive-check: instance did NOT come up — run doctor for diagnostics.\n`)
} else if (!existsSync(binJs)) {
  console.log('(skipping alive-check — run pnpm -r build first)\n')
}

// ─── client detection (Hermes: find existing agent setups) ──────────────────
const { homedir } = await import('node:os')
const clients = [
  { dir: join(homedir(), '.claude'), label: 'Claude Code', script: 'tools/setup-claude.mjs' },
  { dir: join(homedir(), '.codex'), label: 'Codex CLI', script: 'tools/setup-codex.mjs' }
].filter(c => existsSync(c.dir))
if (clients.length) {
  console.log(`found agent clients: ${clients.map(c => c.label).join(', ')}\n`)
  for (const c of clients) {
    if (!YES) {
      const a = await ask(`wire ${c.label} into nektar now? [Y/n] > `)
      if (a === '' || /^y/i.test(a)) {
        const { spawnSync } = await import('node:child_process')
        spawnSync(process.execPath, [join(root, c.script)], { stdio: 'inherit' })
      }
    } else console.log(`  → wire it later: node ${c.script}`)
  }
  console.log('')
}
rl.close()
