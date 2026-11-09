#!/usr/bin/env node
// tools/init.mjs — first-run wizard. Walks the 5 steps that used to live
// across 3 docs: pick a network → where your agent lives → passphrase →
// rendezvous → write .env → print the exact next command.
//
//   node tools/init.mjs            interactive
//   node tools/init.mjs --yes      accept all defaults (mock, ./.nektar, no rv, stdio)
//   node tools/init.mjs --env ttn --dir .ttn --rv https://rv.nektar.run --port 8080
import { createInterface } from 'node:readline'
import { existsSync, mkdirSync, writeFileSync, appendFileSync } from 'node:fs'
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

console.log('\nnektar init — your agent, your keys, verifiable memory.\n')
console.log('This walks the four decisions a fresh install needs, writes .env,')
console.log('and prints the one command to run next. Ctrl+C to bail anytime.\n')

// ─── 1. network ─────────────────────────────────────────────────────────────
const networks = [
  { id: 'mock', label: 'mock — offline, zero setup, auto-mined (best first run)' },
  { id: 'ttn', label: 'ttn — Teranode Test Net, test coins, real market' },
  { id: 'main', label: 'main — real BSV. Read docs/OPERATIONS.md first' },
  { id: 'regtest', label: 'regtest — local Teranode stack (devs)' }
]
const net = arg('--env') ?? (await pick('1/4 — which network?', networks)).id
if (!networks.some(n => n.id === net)) { console.error(`unknown network "${net}"`); process.exit(2) }

// ─── 2. data dir ────────────────────────────────────────────────────────────
const defDir = net === 'mock' ? '.nektar' : `.nektar-${net}`
const dir = arg('--dir') ?? ((YES ? defDir : await ask(`2/4 — where does your agent live? (identity + memory)\n  This directory IS the agent — back it up.\n[${defDir}] > `)) || defDir)
const dataDir = resolve(dir)
mkdirSync(dataDir, { recursive: true })

// ─── 3. passphrase ──────────────────────────────────────────────────────────
let pass = arg('--passphrase')
if (pass === undefined && !YES) {
  pass = await askSecret(`3/4 — key passphrase (encrypts your identity at rest; unrecoverable)\n  ${net === 'main' ? 'REQUIRED on main.' : 'empty = skip (mock is fine without)'}\n> `)
} else pass ??= ''

// ─── 4. rendezvous + port ───────────────────────────────────────────────────
let rv = arg('--rv')
if (rv === undefined && !YES && net !== 'mock') {
  rv = (await pick('4/4 — join the public discovery index?', [
    { id: 'https://rv.nektar.run', label: 'https://rv.nektar.run — public agents can find your offers' },
    { id: '', label: 'none — local only' }
  ])).id
} else rv ??= (net === 'mock' ? '' : '')
const port = arg('--port')

// ─── write .env ─────────────────────────────────────────────────────────────
const envPath = join(root, '.env')
const lines = [
  `# nektar init — ${new Date().toISOString().slice(0, 10)} (${net})`,
  `NEKTAR_NETWORK=${net}`,
  `NEKTAR_DATA_DIR=${dataDir}`
]
if (pass) lines.push(`NEKTAR_KEY_PASSPHRASE=${pass}`)
if (rv) lines.push(`NEKTAR_OVERLAY_URL=${rv}`)
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
  const managed = ['NEKTAR_NETWORK', 'NEKTAR_DATA_DIR', 'NEKTAR_KEY_PASSPHRASE', 'NEKTAR_OVERLAY_URL', 'NEKTAR_HTTP_PORT', 'NEKTAR_VERIFY_BASE', 'NEKTAR_MODEL_ID']
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
  console.log('  → then discover → peerCall → your first paid call on the public market')
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
rl.close()
