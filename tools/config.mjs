#!/usr/bin/env node
// tools/config.mjs — non-interactive .env surface (Hermes `config` +
// OpenClaw non-TTY pattern). Warn, don't fail — info, not a gate.
//
//   node tools/config.mjs get NEKTAR_NETWORK      print one value
//   node tools/config.mjs get --all               print all (secrets masked)
//   node tools/config.mjs set NEKTAR_HTTP_PORT 8080
//   node tools/config.mjs unset NEKTAR_HTTP_PORT
//   node tools/config.mjs check                   validate + stale keys
//   node tools/config.mjs set NEKTAR_FOO x --force   bypass known-key guard
import { existsSync, readFileSync, writeFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = resolve(fileURLToPath(new URL('..', import.meta.url)))
const envPath = join(root, '.env')

// keys the runtime + tools actually read (from runtime.ts, server.ts, bin.js,
// serve.mjs, service-run.mjs) — anything else is a probable typo
const KNOWN = new Set([
  'NEKTAR_NETWORK',
  'NEKTAR_DATA_DIR',
  'NEKTAR_KEY_PASSPHRASE',
  'NEKTAR_KEY_PASSPHRASE_FILE',
  'NEKTAR_FUND_WIF',
  'NEKTAR_FUND_WIF_FILE',
  'NEKTAR_REGTEST_WIF',
  'NEKTAR_MAINNET_CONFIRM',
  'NEKTAR_VERIFY_BASE',
  'NEKTAR_HTTP_PORT',
  'NEKTAR_HTTP_HOST',
  'NEKTAR_HTTP_TOKEN',
  'NEKTAR_MODEL_ID',
  'NEKTAR_PAYMENTS',
  'NEKTAR_OPERATOR_WALLET',
  'NEKTAR_PACK_SHARE',
  'NEKTAR_RPC_URL',
  'NEKTAR_RPC_USER',
  'NEKTAR_RPC_PASS',
  'NEKTAR_PROPAGATION_URL',
  'NEKTAR_ASSET_URL',
  'NEKTAR_BROADCASTER_URL',
  'NEKTAR_BROADCASTER_API_KEY',
  'NEKTAR_WOC_URL',
  'NEKTAR_WOC_API_KEY',
  'NEKTAR_BTC_WOC_URL',
  'NEKTAR_CHAINTRACKS_URL',
  'NEKTAR_TEMPORAL_SOURCES',
  'NEKTAR_CLEARANCE_AUTHORITIES',
  'NEKTAR_BATCH',
  'NEKTAR_AUTOMINE',
  'NEKTAR_CONFIRM_MS',
  'NEKTAR_FINALITY_DEPTH',
  'NEKTAR_FEE_RATE_SAT_PER_KB',
  'NEKTAR_FEE_VAULT',
  'NEKTAR_MAX_FEE_SATS_PER_TX',
  'NEKTAR_MAX_SPEND_SATS_PER_DAY',
  'NEKTAR_MAX_LEVEL',
  'NEKTAR_MIN_BROADCAST_INTERVAL_MS',
  'NEKTAR_FUND_TXIDS',
  'NEKTAR_DEMO_UTXOS',
  'NEKTAR_INDEXER_DISCOVERY',
  'NEKTAR_WAIT_FUNDING',
  'NEKTAR_WAIT_FUNDING_MS',
  'NEKTAR_PROOF_SHARDING',
  'NEKTAR_STARK_PROVER',
  'NEKTAR_STARK_IMAGE',
  'NEKTAR_PAID_DISCLOSURE',
  'NEKTAR_ACCESS_LOG',
  'NEKTAR_HETZNER_TOKEN',
  'NEKTAR_DEMO_PORT',
  'NEKTAR_AUTORESPOND',
  'NEKTAR_AUTORESPOND_PRICE_SATS',
  'NEKTAR_AUTORESPOND_DOMAINS',
  'NEKTAR_AUTOMATION',
  'NEKTAR_AUTOMATION_INTERVAL_MS',
  'NEKTAR_HTTP_TOKEN_FILE',
  'NEKTAR_AI_BASE_URL',
  'NEKTAR_AI_MODEL',
  'NEKTAR_AI_PROVIDER',
  'NEKTAR_AI_EGRESS',
  'NEKTAR_AI_API_KEY',
  'NEKTAR_AI_API_KEY_FILE'
])
const SECRET = /WIF|PASS|KEY|TOKEN|SECRET/i
const NETWORKS = ['regtest', 'testnet', 'ttn', 'main']

const read = () => (existsSync(envPath) ? readFileSync(envPath, 'utf8') : '')
const parse = (txt) =>
  txt
    .split(/\r?\n/)
    .map((l) => l.match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/))
    .filter(Boolean)
    .map((m) => ({ key: m[1], val: m[2].trim() }))
const get = (key, env) => env.find((e) => e.key === key)?.val
const mask = (key, val) => (SECRET.test(key) && val ? '***' : val)

const [cmd, ...rest] = process.argv.slice(2)
const flag = (f) => rest.includes(f)
const positional = rest.filter((a) => !a.startsWith('--'))

if (!existsSync(envPath)) console.log(`(no .env yet — 'pnpm init:nektar' creates one)\n`)

switch (cmd) {
  case 'get': {
    if (flag('--all')) {
      for (const e of parse(read())) console.log(`${e.key}=${mask(e.key, e.val)}`)
      break
    }
    const key = positional[0]
    if (!key) {
      console.error('usage: config get KEY | --all')
      process.exit(2)
    }
    const v = get(key, parse(read()))
    console.log(v === undefined ? `(unset)` : mask(key, v))
    break
  }
  case 'set': {
    const [key, ...valParts] = positional
    const val = valParts.join(' ')
    if (!key || val === '') {
      console.error('usage: config set KEY VALUE')
      process.exit(2)
    }
    if (key.startsWith('NEKTAR_') && !KNOWN.has(key) && !flag('--force')) {
      console.error(`unknown key ${key} — possible typo. Re-run with --force to set anyway.`)
      process.exit(2)
    }
    const lines = read().split(/\r?\n/)
    const idx = lines.findIndex((l) => new RegExp(`^${key}=`).test(l))
    const line = `${key}=${val}`
    if (idx >= 0) {
      lines[idx] = line
    } else lines.push(line)
    writeFileSync(envPath, lines.join('\n').replace(/\n{3,}/g, '\n\n'))
    console.log(`${key}=${mask(key, val)}`)
    break
  }
  case 'unset': {
    const key = positional[0]
    if (!key) {
      console.error('usage: config unset KEY')
      process.exit(2)
    }
    const lines = read()
      .split(/\r?\n/)
      .filter((l) => !new RegExp(`^${key}=`).test(l))
    writeFileSync(envPath, lines.join('\n').replace(/\n{3,}/g, '\n\n'))
    console.log(`${key} removed`)
    break
  }
  case 'check':
  case 'validate': {
    const env = parse(read())
    const net = get('NEKTAR_NETWORK', env) ?? 'regtest'
    const warn = (m) => console.log(`  ! ${m}`)
    const ok = (m) => console.log(`  ok ${m}`)
    NETWORKS.includes(net) ? ok(`NEKTAR_NETWORK=${net}`) : warn(`NEKTAR_NETWORK=${net} — must be ${NETWORKS.join('|')}`)
    if (net === 'main') {
      get('NEKTAR_MAINNET_CONFIRM', env) === 'I_UNDERSTAND'
        ? ok('mainnet confirm')
        : warn('main needs NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND or the server refuses to start')
      get('NEKTAR_FUND_WIF', env) || get('NEKTAR_FUND_WIF_FILE', env) ? ok('fund key') : warn('main needs NEKTAR_FUND_WIF (or _FILE)')
      get('NEKTAR_KEY_PASSPHRASE', env) ? ok('key passphrase') : warn('main needs NEKTAR_KEY_PASSPHRASE')
    }
    if (net === 'ttn') get('NEKTAR_KEY_PASSPHRASE', env) ? ok('key passphrase') : warn('ttn: NEKTAR_KEY_PASSPHRASE recommended')
    if (net === 'regtest')
      (get('NEKTAR_FUND_WIF', env) ?? get('NEKTAR_REGTEST_WIF', env)) && get('NEKTAR_RPC_PASS', env)
        ? ok('regtest secrets')
        : warn('regtest needs NEKTAR_FUND_WIF/NEKTAR_REGTEST_WIF + NEKTAR_RPC_PASS')
    const port = get('NEKTAR_HTTP_PORT', env)
    if (port !== undefined) /^\d+$/.test(port) && Number(port) < 65536 ? ok(`HTTP :${port}`) : warn(`NEKTAR_HTTP_PORT=${port} is not a valid port`)
    else warn('no NEKTAR_HTTP_PORT — the instance serves MCP stdio only; agents cannot reach it over HTTP')
    const dir = get('NEKTAR_DATA_DIR', env)
    dir
      ? existsSync(resolve(dir))
        ? ok(`dataDir ${dir}`)
        : warn(`NEKTAR_DATA_DIR ${dir} does not exist — pnpm doctor:fix creates it`)
      : ok('dataDir default ./.nektar')
    for (const e of env) if (e.key.startsWith('NEKTAR_') && !KNOWN.has(e.key)) warn(`unknown key ${e.key} — typo? (config:nektar unset ${e.key})`)
    for (const e of env)
      if (SECRET.test(e.key) && e.val && !/^_FILE/i.test(e.key)) warn(`${e.key} holds a plaintext secret — consider a *_FILE variant or a secrets manager`)
    console.log('\ncheck complete — warnings only, nothing enforced')
    break
  }
  default:
    console.log('nektar config — .env surface\n')
    console.log('  get KEY | --all        read (secrets masked)')
    console.log('  set KEY VALUE          write (unknown NEKTAR_* keys need --force)')
    console.log('  unset KEY              remove')
    console.log('  check                  validate + stale-key report (warn only)')
    process.exit(cmd ? 2 : 0)
}
