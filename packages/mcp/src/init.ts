// mamem-init — first-run wallet bootstrap for the MCP server operator.
//
//   node dist/src/init.js                     → generate a funding key, write
//                                               it to .env (never echoed),
//                                               print the address
//   node dist/src/init.js --network=regtest   → keep/generate the key, then
//   (or the legacy --regtest flag)              check reserve funding on the
//                                               local Teranode stack; prints
//                                               the exact funding command if
//                                               unfunded
//   --force                                    → rotate an existing key (old
//                                               funds stay with the old key)
//   MAMEM_ENV_FILE=/path/to/.env               → target a different env file
//                                               (testing, custom layouts)
//
// The key never leaves the gitignored .env; only the ADDRESS is printed.
// The AI user never sees any of this — the server is the wallet holder
// (operator-pays model).
import { PrivateKey } from '@bsv/sdk'
import { existsSync, readFileSync, writeFileSync, chmodSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

const ARG = (k: string) => process.argv.includes('--' + k)
const ARGV = (k: string): string | undefined => {
  const hit = process.argv.find(a => a.startsWith(`--${k}=`))
  return hit?.slice(k.length + 3)
}
// MAMEM_ENV_FILE overrides the target — lets a scratch env be used for
// testing or non-standard layouts without touching the operator's .env.
const ENV_PATH = process.env.MAMEM_ENV_FILE ?? fileURLToPath(new URL('../../../../.env', import.meta.url))
const QS_PATH = fileURLToPath(new URL('../../../../infra/teranode-quickstart/.env', import.meta.url))

// --network: only regtest is honest today — reject anything that would
// imply mainnet readiness.
const network = ARGV('network') ?? (ARG('regtest') ? 'regtest' : undefined)
if (network !== undefined && network !== 'regtest') {
  console.error(`mamem-init: unsupported --network "${network}" — only "regtest" is wired today`)
  process.exit(2)
}

function readEnv (p: string): Record<string, string> {
  const out: Record<string, string> = {}
  if (!existsSync(p)) return out
  for (const line of readFileSync(p, 'utf8').split(/\r?\n/)) {
    const m = line.trim().match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/)
    if (m) out[m[1]] = m[2].trim()
  }
  return out
}
function upsertEnv (p: string, key: string, value: string): void {
  const lines = existsSync(p) ? readFileSync(p, 'utf8').split(/\r?\n/) : []
  const re = new RegExp(`^${key}=`)
  const idx = lines.findIndex(l => re.test(l.trim()))
  if (idx >= 0) lines[idx] = `${key}=${value}`; else lines.push(`${key}=${value}`)
  writeFileSync(p, lines.filter((l, i) => l !== '' || i < lines.length - 1).join('\n'))
  try { chmodSync(p, 0o600) } catch { /* windows */ }
}

const env = readEnv(ENV_PATH)
const existing = process.env.MAMEM_REGTEST_WIF ?? env.MAMEM_REGTEST_WIF
let key: PrivateKey
let fresh = false
if (existing && !ARG('force')) {
  key = PrivateKey.fromWif(existing)
  const addr = key.toPublicKey().toAddress([0x6f])
  console.log(`mamem-init: a funding key already exists (${addr})`)
  if (network !== 'regtest') {
    console.log('  nothing to do — use --force to rotate (existing funds stay with the old key)')
    process.exit(0)
  }
  console.log('  key kept — checking regtest funding')
} else {
  fresh = true
  key = PrivateKey.fromRandom()
  upsertEnv(ENV_PATH, 'MAMEM_REGTEST_WIF', key.toWif())
  const addr = key.toPublicKey().toAddress([0x6f])
  console.log(`mamem-init: funding key generated`)
  console.log(`  address : ${addr}`)
  console.log(`  key     : written to ${ENV_PATH} (chmod 600 — never committed, never printed)`)
  if (existing) console.log('  WARNING : rotated — UTXOs held by the previous key are not spendable by this one')
}

if (network === 'regtest') {
  // split mature coinbases owned by the NEW key into spendable reserve
  // outputs — only meaningful right after mining fresh blocks to it, or
  // when the operator reuses a previously funded key.
  const qs = readEnv(QS_PATH)
  const cfg = {
    rpcUrl: process.env.MAMEM_RPC_URL ?? 'http://localhost:9292/',
    rpcUser: process.env.MAMEM_RPC_USER ?? qs.rpc_user ?? 'teranode',
    rpcPass: process.env.MAMEM_RPC_PASS ?? qs.rpc_pass ?? '',
    propagationUrl: process.env.MAMEM_PROPAGATION_URL ?? 'http://localhost:8833/tx',
    assetUrl: process.env.MAMEM_ASSET_URL ?? 'http://localhost:8090/api/v1'
  }
  // make the operator .env self-sufficient for the MCP server: map the
  // quickstart creds into MAMEM_RPC_* if they aren't set already.
  if (qs.rpc_user && env.MAMEM_RPC_USER === undefined) { upsertEnv(ENV_PATH, 'MAMEM_RPC_USER', qs.rpc_user); env.MAMEM_RPC_USER = qs.rpc_user }
  if (qs.rpc_pass && env.MAMEM_RPC_PASS === undefined) { upsertEnv(ENV_PATH, 'MAMEM_RPC_PASS', qs.rpc_pass); env.MAMEM_RPC_PASS = qs.rpc_pass }
  const { ReserveFunder, bootstrapReserve } = await import('@mamem/chain')
  const auth = 'Basic ' + Buffer.from(`${cfg.rpcUser}:${cfg.rpcPass}`).toString('base64')
  const rpc = async (method: string, ...params: unknown[]): Promise<any> => {
    const r = await fetch(cfg.rpcUrl, { method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: auth }, body: JSON.stringify({ jsonrpc: '1.0', id: 1, method, params }) })
    const j = await r.json()
    if (j.error) throw new Error(`rpc ${method}: ${JSON.stringify(j.error)}`)
    return j.result
  }
  try {
    const tip = (await rpc('getblockchaininfo')).blocks
    const stateFile = fileURLToPath(new URL('../../../../.state/init-reserve.json', import.meta.url))
    const treasuryFile = fileURLToPath(new URL('../../../../.state/init-treasury.json', import.meta.url))
    const reserve = new ReserveFunder(key, stateFile)
    if (!fresh) await reserve.scanCoinbases(cfg, rpc, Math.max(1, tip - 400), tip - 101)
    if (reserve.utxos.length > 0) {
      console.log(`  reserve : funded — ${reserve.utxos.length} mature coinbase output(s) found`)
    } else if (existsSync(treasuryFile)) {
      // resplit the persisted treasury only — maxCandidates:0 skips the
      // full-chain scan that is useless for a key that never mined.
      const tx = await bootstrapReserve(cfg, key, reserve, rpc, { treasuryFile, outputs: 16, maxCandidates: 0 })
      console.log(tx ? `  reserve : funded — split tx ${tx.id('hex').slice(0, 16)}… (+${reserve.utxos.length} outputs)` : '  reserve : treasury present but already spent — fund manually')
    } else {
      console.log('  reserve : unfunded — fund it, then rerun --regtest:')
      console.log(`            curl -u <rpc_user>:<rpc_pass> -d '{"method":"generatetoaddress","params":[101,"${key.toPublicKey().toAddress([0x6f])}"]}' ${cfg.rpcUrl}`)
    }
  } catch (e) {
    console.log(`  regtest : unreachable (${(e as Error).message.slice(0, 120)}) — start the stack with pnpm stack:up, then rerun --regtest`)
  }
}

console.log('\nnext steps:')
console.log('  plugin : pnpm mcp:install claude-desktop cursor   # any AI client via Docker MCP')
console.log('  serve  : MAMEM_NETWORK=regtest node packages/mcp/dist/src/bin.js')
console.log('  http   : MAMEM_HTTP_PORT=8080 …same            # REST /v1 + MCP /mcp + OpenAPI')
