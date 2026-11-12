// Measure REAL anchor tx sizes + fees on regtest at a configurable fee rate
// (default 100 sat/kB — the GorillaPool broadcaster policy rate). Read-only w.r.t.
// the brief: broadcasts regtest txs only, never mainnet.
//   node packages/e2e/dist/src/measure-costs.js
import { PrivateKey, Transaction } from '@bsv/sdk'
import { dsha256, utf8 } from '@nektar/core'
import { AnchorChain } from '@nektar/anchor'
import { ReserveFunder, TeranodeBroadcaster, bootstrapReserve, type RegtestConfig } from '@nektar/chain'
import { existsSync, readFileSync, mkdirSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import path from 'node:path'

function readEnv(p: string): Record<string, string> {
  if (!existsSync(p)) return {}
  return Object.fromEntries(
    readFileSync(p, 'utf8')
      .split(/\r?\n/)
      .map((l) => l.match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/))
      .filter(Boolean)
      .map((m) => [m![1], m![2].trim()])
  )
}
const root = fileURLToPath(new URL('../../../..', import.meta.url))
const localEnv = readEnv(path.join(root, '.env'))
const qsEnv = readEnv(path.join(root, 'infra/teranode-quickstart/.env'))
const WIF = process.env.NEKTAR_REGTEST_WIF ?? localEnv.NEKTAR_REGTEST_WIF
if (!WIF) throw new Error('NEKTAR_REGTEST_WIF missing (nektar/.env)')
const cfg: RegtestConfig = {
  rpcUrl: process.env.NEKTAR_RPC_URL ?? 'http://localhost:9292/',
  rpcUser: process.env.NEKTAR_RPC_USER ?? qsEnv.rpc_user ?? 'teranode',
  rpcPass: process.env.NEKTAR_RPC_PASS ?? qsEnv.rpc_pass ?? '',
  propagationUrl: process.env.NEKTAR_PROPAGATION_URL ?? 'http://localhost:8833/tx',
  assetUrl: process.env.NEKTAR_ASSET_URL ?? 'http://localhost:8090/api/v1'
}
const key = PrivateKey.fromWif(WIF)
const auth = 'Basic ' + Buffer.from(`${cfg.rpcUser}:${cfg.rpcPass}`).toString('base64')
const rpc = async (method: string, ...params: unknown[]): Promise<any> => {
  const r = await fetch(cfg.rpcUrl, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: auth },
    body: JSON.stringify({ jsonrpc: '1.0', id: 1, method, params })
  })
  const j = (await r.json()) as any
  if (j.error) throw new Error(`rpc ${method}: ${JSON.stringify(j.error)}`)
  return j.result
}
const broadcaster = new TeranodeBroadcaster(cfg.propagationUrl)
mkdirSync('.state', { recursive: true })
const reserve = new ReserveFunder(key, '.state/measure-reserve.json')
const s = await bootstrapReserve(cfg, key, reserve, rpc, { treasuryFile: '.state/treasury.json' })
if (s) await rpc('generatetoaddress', 1, key.toPublicKey().toAddress([0x6f]))
if (reserve.utxos.length === 0) throw new Error('reserve empty')

const RATE = Number(process.env.NEKTAR_FEE_RATE_SAT_PER_KB ?? 100)
const funder = {
  keyPair: key,
  get utxos() {
    return reserve.utxos
  },
  markSpent: (t: string, v: number) => reserve.markSpent(t, v),
  addOwned: (tx: Transaction, v: number) => reserve.addOwned(tx, v),
  takeReserve: async () => await reserve.takeReserve()
}
const chain = {
  broadcast: (tx: Transaction) => broadcaster.broadcast(tx),
  currentHeight: () => 0 as never,
  isValidRootForHeight: async () => true,
  headerAtHeight: async () => undefined as never,
  tipHeight: async () => 0
}

// known outpoint → satoshis, for fee computation (reserve + our own txs)
const sats = new Map<string, number>()
const record = (tx: Transaction) => tx.outputs.forEach((o, v) => sats.set(`${tx.id('hex')}:${v}`, o.satoshis ?? 0))
const seed = () => {
  for (const u of reserve.utxos) record(u.tx)
}
const feeOf = (tx: Transaction) => {
  let vin = 0
  for (const i of tx.inputs) {
    const id = i.sourceTXID ?? i.sourceTransaction?.id('hex')
    const v = id !== undefined ? sats.get(`${id}:${i.sourceOutputIndex}`) : undefined
    if (v === undefined) return undefined
    vin += v
  }
  return vin - tx.outputs.reduce((s, o) => s + (o.satoshis ?? 0), 0)
}
const report = (label: string, tx: Transaction) => {
  record(tx)
  console.log(`${label}: txid=${tx.id('hex')} bytes=${tx.toBinary().length} fee=${feeOf(tx)} sats`)
}

const ac = new AnchorChain('measure', PrivateKey.fromRandom(), funder as never, key, chain as never, { feeRateSatPerKb: RATE })
seed()
report('genesis', (await ac.anchor(5, dsha256(utf8('m1')))).tx)
seed()
report('append ', (await ac.anchor(8, dsha256(utf8('m2')))).tx)
seed()
report('rotate ', (await ac.rotate(PrivateKey.fromRandom())).tx)
console.log(`fee rate: ${RATE} sat/kB`)
