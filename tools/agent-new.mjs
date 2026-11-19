#!/usr/bin/env node
// tools/agent-new.mjs — one command to a trading agent. Creates a data dir,
// a fund key, a signed-off policy, and (with --up) a detached instance —
// then prints the MCP join card. The eleven manual steps the first live
// agent took collapse into this.
//
//   node tools/agent-new.mjs --testnet --name alice --domains sports,nutrition
//   node tools/agent-new.mjs --testnet --name bob --port 8100 --up
//   node tools/agent-new.mjs --main --name shop --fund-wif <wif>
//
// Flags:
//   --testnet|--main|--regtest   network (also selects the matching PROFILE)
//   --name <n>                   agent name → data dir .state/agent-<n>
//   --data <dir>                 explicit data dir (overrides --name)
//   --port <n>                   HTTP port (default 8080)
//   --fund-wif <wif>             bring-your-own key (else a fresh one is made)
//   --domains a,b                policy domains (default: general)
//   --max-sats <n>               per-purchase/daily cap in the policy (default 30000/60000)
//   --up                         spawn the instance detached + wait for health
//   --json                       print the join card as JSON
import { existsSync, mkdirSync, openSync, readFileSync, writeFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { createRequire } from 'node:module'
import { randomBytes } from 'node:crypto'
import { spawn } from 'node:child_process'

const root = resolve(fileURLToPath(new URL('..', import.meta.url)))
const argv = process.argv.slice(2)
const flag = (k) => {
  const i = argv.indexOf(k)
  if (i < 0) return undefined
  const v = argv[i + 1]
  argv.splice(i, 2)
  return v
}
const has = (k) => argv.includes(k)
const die = (m, code = 1) => {
  console.error(`agent-new: ${m}`)
  process.exit(code)
}
const req = createRequire(join(root, 'packages/mcp/package.json'))

const net = has('--testnet') ? 'testnet' : has('--main') ? 'main' : has('--regtest') ? 'regtest' : undefined
if (net === undefined) die('pick a network: --testnet | --main | --regtest')
const profile = net === 'testnet' ? 'testnet-agent' : net === 'main' ? 'mainnet-agent' : 'regtest-dev'
const name = flag('--name') ?? 'agent'
const dataDir = resolve(flag('--data') ?? join(root, '.state', `agent-${name}`))
const port = Number(flag('--port') ?? 8080)
if (!Number.isSafeInteger(port) || port < 1 || port > 65535) die(`bad --port ${flag('--port')}`)
const domains = (flag('--domains') ?? 'general')
  .split(',')
  .map((d) => d.trim().toLowerCase())
  .filter(Boolean)
const perPurchase = Number(flag('--max-sats') ?? 30_000)

const { PrivateKey } = req('@bsv/sdk')
const wifFlag = flag('--fund-wif')
const envFile = join(dataDir, 'agent.env')
const policyFile = join(dataDir, 'policy.json')
mkdirSync(dataDir, { recursive: true })

// fund key: flag > persisted env > fresh. Re-running agent-new on an
// existing dir keeps the same key — identity must not churn
let fundWif = wifFlag
if (fundWif === undefined && existsSync(envFile)) {
  const m = readFileSync(envFile, 'utf8').match(/^NEKTAR_FUND_WIF=(\S+)/m)
  if (m !== null) fundWif = m[1]
}
const key = fundWif !== undefined ? PrivateKey.fromWif(fundWif) : PrivateKey.fromRandom()
fundWif = key.toWif()
const agentPub = key.toPublicKey().toString()
const address = key.toAddress()
const token = randomBytes(32).toString('hex')
const operatorToken = randomBytes(32).toString('hex')

const policy = {
  v: 1,
  policyId: `${name}-${net}`,
  network: net,
  agentPub,
  roles: ['buyer', 'seller'],
  notBefore: 0,
  notAfter: 4102444800000,
  domains,
  limits: {
    perPurchaseSats: perPurchase,
    dailyPurchaseSats: perPurchase * 2,
    perTxFeeSats: 2000,
    dailyFeeSats: 20000,
    runTotalSats: perPurchase * 2,
    maxLockedSats: perPurchase * 2,
    maxUnconfirmedSats: perPurchase * 2,
    maxUnconfirmedCount: 20
  },
  scoring: {
    minimumScoreBps: 1000,
    weights: { relevance: 3000, provenance: 2500, history: 1500, price: 2000, latency: 1000 },
    collectMs: 8000
  },
  seller: { minPriceSats: 100, maxPriceSats: perPurchase, maxPreparationsPerDay: 20, sellableShards: domains },
  // the approvable surface — paid deals armed by the policy itself, no
  // NEKTAR_PAID_DISCLOSURE env needed
  disclosure: { allowPublic: false, paid: true },
  ai: { providers: ['scripted'], allowSensitiveEgress: false, maxOutputTokens: 256, timeoutMs: 20000 },
  sandbox: { mode: 'never' }
}

// the boot env — node --env-file reads this back; every line is a knob the
// operator can edit before the next start
const env = {
  NEKTAR_PROFILE: profile,
  NEKTAR_DATA_DIR: dataDir,
  NEKTAR_HTTP_PORT: String(port),
  NEKTAR_HTTP_TOKEN: token,
  NEKTAR_OPERATOR_TOKEN: operatorToken,
  NEKTAR_FUND_WIF: fundWif,
  // a fresh agent that can only move 20k/day stalls on its first upkeep day —
  // the cap is an operator knob, sized here for actual trading
  NEKTAR_MAX_SPEND_SATS_PER_DAY: '200000',
  // adopt funding sats the moment they land on the index — no restart to
  // notice a fresh UTXO
  NEKTAR_INDEXER_DISCOVERY: '1'
}
writeFileSync(
  envFile,
  Object.entries(env)
    .map(([k, v]) => `${k}=${v}`)
    .join('\n') + '\n'
)
writeFileSync(policyFile, JSON.stringify(policy, null, 1) + '\n')

// approve the policy directly into the data dir — no running instance needed
const dist = join(root, 'packages/automation/dist/src')
if (!existsSync(join(dist, 'store.js'))) die('no build — run pnpm -r build first')
const { AutomationStore } = await import(pathToFileURL(join(dist, 'store.js')).href)
const { policyError } = await import(pathToFileURL(join(dist, 'policy.js')).href)
const e = policyError(policy)
if (e !== undefined) die(`generated policy failed validation — ${e} (this is a bug)`)
const knexFactory = req('knex')
const kx = knexFactory({ client: 'sqlite3', connection: { filename: join(dataDir, 'automation.sqlite') }, useNullAsDefault: true })
let policyHash
try {
  const store = new AutomationStore(kx)
  await store.init()
  policyHash = await store.approvePolicy(policy, 'cli:agent-new')
} finally {
  await kx.destroy()
}

const bin = join(root, 'packages/mcp/dist/src/bin.js')
if (!existsSync(bin)) die('no build — run pnpm -r build first')
const startCmd = `node --env-file="${envFile}" "${bin}"`

let child
if (has('--up')) {
  const logFd = join(dataDir, 'agent.log')
  const fd = openSync(logFd, 'a')
  child = spawn(process.execPath, [`--env-file=${envFile}`, bin], {
    env: { ...process.env, ...env },
    detached: true,
    stdio: ['ignore', fd, fd]
  })
  child.unref()
  // health is the only readiness signal that counts
  const deadline = Date.now() + 45_000
  let healthy = false
  while (Date.now() < deadline) {
    const ok = await fetch(`http://127.0.0.1:${port}/v1/health`)
      .then((r) => r.ok)
      .catch(() => false)
    if (ok) {
      healthy = true
      break
    }
    await new Promise((r) => setTimeout(r, 500))
  }
  if (!healthy) console.error(`agent-new: started but /v1/health never answered — see ${logFd}`)
}

const card = {
  name,
  network: net,
  dataDir,
  port,
  fundingAddress: address,
  agentPub,
  policyHash,
  policyFile,
  envFile,
  mcp: `http://127.0.0.1:${port}/mcp`,
  bearer: token,
  operatorToken,
  agentCard: `http://127.0.0.1:${port}/.well-known/agent.json`,
  start: startCmd,
  ...(child !== undefined ? { pid: child.pid, log: join(dataDir, 'agent.log') } : {})
}
if (has('--json')) {
  console.log(JSON.stringify(card, null, 1))
} else {
  console.log(`agent ${name} ready — ${net}`)
  console.log(`  funding address  ${address}   ← send ${net} sats here, indexer adoption picks them up`)
  console.log(`  policy           ${policyHash.slice(0, 16)}… approved (${policyFile})`)
  console.log(`  MCP              ${card.mcp}`)
  console.log(`  bearer           ${token}`)
  console.log(`  agent card       ${card.agentCard}`)
  console.log(`  env              ${envFile}`)
  console.log(child !== undefined ? `  running          pid ${child.pid} — log ${card.log}` : `  start            ${startCmd}`)
}
