// dump lanes.sqlite trees/slots for a probe dataDir
import knex from 'knex'
import { mkdtempSync, mkdirSync, copyFileSync, existsSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { PrivateKey } from '@bsv/sdk'
import { ChaintracksTracker, WocTracker, BsvBroadcaster, fetchTxStatus, ReserveFunder } from '@nektar/chain'
import { NektarMemory } from './dist/src/memory.js'
import { callTool } from './dist/src/server.js'
import { openAutomation, ScriptedExecutor } from '@nektar/automation'

const ARC = 'https://arcade-v2-testnet-us-1.bsvblockchain.tech'
const WOC = 'https://api.whatsonchain.com/v1/bsv/test'
const WIF = 'cRJ9mNASGGJ4kTAf2HhN8CjPkbCwJ6xDiWp5DnvL9xveuM9PtuiF'

const dir = mkdtempSync(join(tmpdir(), 'tn-trees-'))
const dataDir = join(dir, 'ben')
mkdirSync(dataDir, { recursive: true })
const src = join(process.cwd(), '../../.state/testnet-agents/ben/reserve.json')
if (existsSync(src)) copyFileSync(src, join(dataDir, 'reserve.json'))
const mc = { broadcasterUrl: ARC, wocUrl: WOC }
const tracker = new ChaintracksTracker(mc, `${ARC}/chaintracks/v2`, new WocTracker(mc))
const bcast = new BsvBroadcaster(mc)
const chain = {
  broadcast: (tx) => bcast.broadcast(tx),
  currentHeight: () => tracker.currentHeight(),
  isValidRootForHeight: (r, h) => tracker.isValidRootForHeight(r, h),
  headerAtHeight: (h) => tracker.headerAtHeight(h),
  tipHeight: () => tracker.currentHeight()
}
const fundKey = PrivateKey.fromWif(WIF)
const reserve = new ReserveFunder(fundKey, join(dataDir, 'reserve.json'))
const mem = new NektarMemory(chain, reserve, fundKey, {
  network: 'testnet',
  modelId: 'm',
  feeRateSatPerKb: 1,
  txStatus: (txid) => fetchTxStatus(mc, txid, { wocFallback: true }),
  batchPolicy: { maxLeaves: 1 }
})
const rt = await openAutomation(mem, {
  network: 'testnet',
  localHosts: true,
  dataDir,
  marketHosts: ['http://127.0.0.1:18291', 'http://127.0.0.1:18292'],
  intervalMs: 0,
  call: (t, a) => callTool(mem, t, a),
  log: (l) => console.log('LOG', l),
  ai: new ScriptedExecutor(),
  publishFinger: true,
  opts: { offerTtlBlocks: 48, refundBlocks: 1 }
})
mem.automation = rt
for (let i = 0; i < 4; i++) await rt.cycle()

// inspect the lanes store directly
const kx = knex({ client: 'better-sqlite3', connection: { filename: join(dataDir, 'lanes.sqlite') }, useNullAsDefault: true })
const tables = await kx.raw("select name from sqlite_master where type='table'")
console.log('tables:', tables.map((t) => t.name).join(', '))
for (const t of tables.map((t) => t.name)) {
  try {
    console.log(t, JSON.stringify(await kx(t).select('*').limit(8), null, 0).slice(0, 2000))
  } catch (e) {
    console.log(t, 'ERR', e.message)
  }
}
await kx.destroy()
await rt.stop()
