#!/usr/bin/env node
// tools/init.mjs — first-run wizard: pick a network → where your agent
// lives → passphrase → write .env → print the exact next command.
//
//   node tools/init.mjs            interactive (@clack/prompts wizard)
//   node tools/init.mjs --yes      accept all defaults (regtest, ./.nektar, stdio)
//   node tools/init.mjs --env testnet --dir .testnet --port 8080
import { existsSync, mkdirSync, writeFileSync, readFileSync, chmodSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { randomBytes } from 'node:crypto'
import { intro, outro, select, text, password, confirm, spinner, note, isCancel, cancel } from '@clack/prompts'

const root = resolve(fileURLToPath(new URL('..', import.meta.url)))
const arg = (f) => {
  const i = process.argv.indexOf(f)
  return i >= 0 ? process.argv[i + 1] : undefined
}
const YES = process.argv.includes('--yes') || !process.stdin.isTTY // piped/non-interactive → defaults

/** every prompt resolves to clack's cancel symbol on Ctrl+C — bail cleanly */
const req = (v) => {
  if (isCancel(v)) {
    cancel('bailed — nothing written')
    process.exit(0)
  }
  return v
}

// ─── existing config → reconfigure defaults (Hermes pattern: current values
//     ride as prompt defaults so a re-run only changes what you touch) ───────
const envPath = join(root, '.env')
const { parseDotEnv } = await import('./dotenv.mjs')
const readEnv = (p) => (existsSync(p) ? parseDotEnv(readFileSync(p, 'utf8')) : {})
const prev = readEnv(envPath)
const reconfigure = Boolean(prev.NEKTAR_NETWORK)

intro('nektar init — your agent, your keys, verifiable memory')
note(
  reconfigure ? 'Existing config found — defaults are your current values.' : 'Four decisions, then .env + the one command to run next. Ctrl+C to bail anytime.'
)

// ─── 1. network ─────────────────────────────────────────────────────────────
const networks = [
  { value: 'regtest', label: 'regtest — local Teranode stack, zero real cost (best first run)' },
  { value: 'testnet', label: 'testnet — public test coins, real market' },
  { value: 'main', label: 'main — real BSV. Read docs/OPERATIONS.md first' }
]
const net =
  arg('--env') ??
  (YES
    ? (prev.NEKTAR_NETWORK ?? 'regtest')
    : req(
        await select({
          message: '1/4 — which network?',
          options: networks,
          initialValue: networks.some((n) => n.value === prev.NEKTAR_NETWORK) ? prev.NEKTAR_NETWORK : 'regtest'
        })
      ))
if (!networks.some((n) => n.value === net)) {
  console.error(`unknown network "${net}"`)
  process.exit(2)
}

// ─── 2. data dir ────────────────────────────────────────────────────────────
const defDir = prev.NEKTAR_DATA_DIR ?? '.nektar'
const dir =
  arg('--dir') ??
  ((YES
    ? defDir
    : req(
        await text({
          message: '2/4 — where does your agent live? (identity + memory — this directory IS the agent, back it up)',
          placeholder: defDir,
          defaultValue: defDir
        })
      )) ||
    defDir)
const dataDir = resolve(dir)
mkdirSync(dataDir, { recursive: true })

// ─── 3. passphrase ──────────────────────────────────────────────────────────
// encryption is mandatory on main only — testnet/regtest boot with mode-0600
// plaintext keys when no passphrase is set (set one and they encrypt the same)
const mustEnc = net === 'main'
// a regtest-era data dir carries plaintext key files — main refuses them,
// so say so here instead of letting the first boot die on KeystoreError
for (const f of ['keys.json', 'fee.key.json']) {
  const p = join(dataDir, f)
  if (mustEnc && existsSync(p) && !readFileSync(p, 'utf8').includes('"kdf"'))
    note(`${p} is plaintext — main requires encrypted keys at rest; use a fresh --dir or remove it`, 'heads-up')
}
let pass = arg('--passphrase')
if (pass === undefined && !YES) {
  pass = req(
    await password({
      message: `3/4 — key passphrase (encrypts your identity at rest; unrecoverable)${mustEnc ? ' — REQUIRED, empty generates one into .env' : ' — empty = skip (plaintext keys; set one to encrypt)'}${prev.NEKTAR_KEY_PASSPHRASE ? ' — empty keeps the current one' : ''}`
    })
  )
  if (!pass && prev.NEKTAR_KEY_PASSPHRASE) pass = prev.NEKTAR_KEY_PASSPHRASE
} else pass ??= prev.NEKTAR_KEY_PASSPHRASE ?? ''
// main won't boot without one — generate rather than write a .env the
// runtime will refuse (same shape as `mcp:install --ghcr`)
const generatedPass = mustEnc && !pass
if (generatedPass) pass = randomBytes(24).toString('hex')

// ─── 4. port ────────────────────────────────────────────────────────────
const port = arg('--port') ?? prev.NEKTAR_HTTP_PORT

// ─── write .env ─────────────────────────────────────────────────────────────
const lines = [`# nektar init — ${new Date().toISOString().slice(0, 10)} (${net})`, `NEKTAR_NETWORK=${net}`, `NEKTAR_DATA_DIR=${dataDir}`]
if (pass) lines.push(`NEKTAR_KEY_PASSPHRASE=${pass}`)
if (port) lines.push(`NEKTAR_HTTP_PORT=${port}`)
if (net === 'testnet') lines.push(`NEKTAR_VERIFY_BASE=https://nektar.run`)
const body = lines.join('\n') + '\n'
if (existsSync(envPath)) {
  const stamp = new Date().toISOString().replace(/[:.]/g, '-')
  const bak = `${envPath}.bak-${stamp}`
  const old = (await import('node:fs')).readFileSync(envPath, 'utf8')
  writeFileSync(bak, old)
  note(`existing .env backed up → ${bak}`)
  // drop only the vars init manages — bin.js takes the first occurrence, so
  // a re-init must replace those; operator vars (WIFs, tokens) stay
  const managed = ['NEKTAR_NETWORK', 'NEKTAR_DATA_DIR', 'NEKTAR_KEY_PASSPHRASE', 'NEKTAR_HTTP_PORT', 'NEKTAR_VERIFY_BASE', 'NEKTAR_MODEL_ID']
  const kept = old
    .split(/\r?\n/)
    .filter((l) => !l.trim().startsWith('# nektar init') && !managed.some((k) => new RegExp(`^\\s*${k}\\s*=`).test(l)))
    .join('\n')
    .replace(/\n{3,}/g, '\n\n')
    .trimEnd()
  writeFileSync(envPath, (kept ? kept + '\n\n' : '') + body)
} else writeFileSync(envPath, body)
// 0600 — this file holds the passphrase that decrypts the 0600 keys; a
// default-umask (0644) .env leaks the one secret those files exist to protect
try {
  chmodSync(envPath, 0o600)
} catch {
  /* Windows has no POSIX modes — the ACL inherits and NTFS keeps it private */
}
note(`wrote ${envPath}` + (generatedPass ? '\ngenerated NEKTAR_KEY_PASSPHRASE — .env + the data dir ARE the agent; back up both' : ''))

// ─── next steps ─────────────────────────────────────────────────────────────
const hasRestateBin = existsSync(join(root, 'packages/mcp/restate/restate-server'))
console.log('\nNext:')
if (!hasRestateBin && !process.env.NEKTAR_RESTATE_URL && !prev.NEKTAR_RESTATE_URL)
  console.log('  pnpm restate:up    — install the local Restate executor sidecar (once, required at boot)')
if (net === 'regtest') {
  console.log('  pnpm stack:up      — local Teranode stack (regtest coins, free)')
  console.log('  pnpm demo          — first verified receipt in <60s on the stack')
  console.log('  pnpm setup:claude  — wire Claude Code (or: pnpm setup:codex)')
} else if (net === 'testnet') {
  console.log(`  node packages/mcp/dist/src/bin.js            # starts on testnet (port: ${port ?? 'stdio only'})`)
  console.log('  → call recall {} for the seeded welcome shard; call doctor for your fee address & testnet faucet curl')
  console.log('  → for trading: node packages/mcp/dist/src/bin.js policy approve --preset buyer')
  console.log('  pnpm setup:claude                            # wire your agent client')
} else if (net === 'main') {
  console.log('  MAINNET spends real BSV. Required before the server will start:')
  console.log('    NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND')
  console.log('    NEKTAR_FUND_WIF=<funded key>   (docker: mcp secret set …)')
  console.log('  docs/OPERATIONS.md is the runbook.')
}
console.log(`\nYour agent lives in ${dataDir} — that directory + its passphrase`)
console.log('is the identity. Back it up; there is no recovery.\n')

// ─── alive-check (OpenClaw: verify before you trust it) — boot the instance
//     on a temp port with the config just written, poll health, report ───────
const binJs = join(root, 'packages/mcp/dist/src/bin.js')
if (existsSync(binJs) && net !== 'main') {
  const alivePort = 18190 + Math.floor(Math.random() * 800)
  const { spawn } = await import('node:child_process')
  const s = spinner()
  s.start(`alive-check — booting on ${net}`)
  const child = spawn(process.execPath, [binJs], {
    env: { ...process.env, NEKTAR_NETWORK: net, NEKTAR_DATA_DIR: dataDir, NEKTAR_KEY_PASSPHRASE: pass || undefined, NEKTAR_HTTP_PORT: String(alivePort) },
    stdio: ['ignore', 'pipe', 'pipe']
  })
  const errLines = []
  child.stderr.setEncoding('utf8')
  child.stderr.on('data', (chunk) => {
    for (const l of chunk.split(/\r?\n/)) if (l.trim()) errLines.push(l.trim())
    if (errLines.length > 20) errLines.splice(0, errLines.length - 20)
  })
  child.stdout.resume()
  let alive = false
  for (let i = 0; i < 60; i++) {
    try {
      if ((await fetch(`http://127.0.0.1:${alivePort}/v1/health`, { signal: AbortSignal.timeout(400) })).ok) {
        alive = true
        break
      }
    } catch {}
    if (child.exitCode !== null) break
    await new Promise((r) => setTimeout(r, 300))
  }
  child.kill()
  if (alive) s.stop('alive-check: instance booted — your agent is alive')
  else {
    const tail = errLines.slice(-4).join('\n  ')
    s.error(`alive-check: instance did NOT come up${tail ? `\n  ${tail}` : ''}`)
    if (/restate|RESTATE_REQUIRED/i.test(tail)) note('run once: pnpm restate:up (installs the local Restate sidecar), or set NEKTAR_RESTATE_URL', 'next step')
    else note('run: node tools/doctor.mjs', 'diagnostics')
  }
} else if (!existsSync(binJs)) {
  note('skipping alive-check — run pnpm -r build first')
}

// ─── client detection (Hermes: find existing agent setups) ──────────────────
const { homedir } = await import('node:os')
const clients = [
  { dir: join(homedir(), '.claude'), label: 'Claude Code', script: 'tools/setup-claude.mjs' },
  { dir: join(homedir(), '.codex'), label: 'Codex CLI', script: 'tools/setup-codex.mjs' }
].filter((c) => existsSync(c.dir))
if (clients.length) {
  console.log(`found agent clients: ${clients.map((c) => c.label).join(', ')}\n`)
  for (const c of clients) {
    if (!YES) {
      const a = req(await confirm({ message: `wire ${c.label} into nektar now?`, initialValue: true }))
      if (a) {
        const { spawnSync } = await import('node:child_process')
        spawnSync(process.execPath, [join(root, c.script)], { stdio: 'inherit' })
      }
    } else console.log(`  → wire it later: node ${c.script}`)
  }
  console.log('')
}
outro('done')
