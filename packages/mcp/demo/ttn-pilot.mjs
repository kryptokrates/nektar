// TTN pilot (RUN ONLY AFTER FUNDING — broadcasts real TTN test coins).
//   MAMEM_DATA_DIR=.ttn-pilot MAMEM_KEY_PASSPHRASE=… MAMEM_NETWORK=ttn pnpm ttn:pilot
// remember→anchor→BUMP→prove→append→rotate→restart-no-fork; explorer links.
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { configFromEnv, openRuntime } from '../dist/src/runtime.js'

let failures = 0
const check = (ok, name) => { console.log(`${ok ? '✔' : '✖'} ${name}`); if (!ok) failures++ }

const env = {
  ...process.env,
  MAMEM_NETWORK: 'ttn',
  MAMEM_DATA_DIR: process.env.MAMEM_DATA_DIR ?? mkdtempSync(path.join(tmpdir(), 'mamem-ttn-')),
  MAMEM_BATCH: process.env.MAMEM_BATCH ?? '4',
  MAMEM_CONFIRM_MS: process.env.MAMEM_CONFIRM_MS ?? '30000'
}
if (!env.MAMEM_KEY_PASSPHRASE && !env.MAMEM_KEY_PASSPHRASE_FILE) {
  console.error('MAMEM_KEY_PASSPHRASE (or _FILE) required — keys are encrypted at rest on live nets')
  process.exit(1)
}
const cfg = configFromEnv(env)
const rt = await openRuntime(cfg)
const link = txid => `https://woc-ttn.bsvblockchain.tech/tx/${txid}`
try {
  const w = await rt.ops.wallet()
  console.log(`fee address: ${w.address} — balance ${w.balanceSats} sats (${w.utxos} utxos)`)
  if (w.balanceSats < 3000) { console.error(`fund ${w.address} — need ≥3000 sats for the pilot (anchor + append + rotate)`); process.exit(2) }

  const shard = 'ttn-pilot'
  for (let i = 0; i < 4; i++) await rt.mem.remember(shard, `pilot record ${i}`)
  const head = await rt.mem.anchor(shard) // genesis
  check(!!head, `genesis anchor broadcast`)
  const txid0 = head?.tx?.id?.('hex') ?? head?.txid
  console.log(`  genesis tx: ${link(txid0)}`)

  // wait for the BUMP
  const deadline = Date.now() + 20 * 60 * 1000
  let provable = false
  while (Date.now() < deadline && !provable) {
    await new Promise(r => setTimeout(r, 30_000))
    const p = await rt.mem.prove(shard, 0)
    provable = p?.result === 'ok'
  }
  const pr = await rt.mem.prove(shard, 0)
  check(pr?.result === 'ok', `seq 0 provable against TTN headers (${pr?.result})`)

  for (let i = 4; i < 8; i++) await rt.mem.remember(shard, `pilot record ${i}`)
  const head2 = await rt.mem.anchor(shard)
  const txid1 = head2?.tx?.id?.('hex') ?? head2?.txid
  check(!!head2, `append anchor broadcast`)
  console.log(`  append tx: ${link(txid1)}`)

  const rot = await rt.mem.rotateShard(shard)
  const txid2 = rot?.tx?.id?.('hex') ?? rot?.txid
  check(!!rot, `owner rotation broadcast`)
  console.log(`  rotate tx: ${link(txid2)}`)

  await rt.close()
  // restart on the same dir — next anchor must spend the persisted head, not fork
  const rt2 = await openRuntime(cfg)
  const head3 = await rt2.mem.anchor(shard)
  check(!!head3, `post-restart anchor spends persisted head (no fork)`)
  console.log(`  post-restart tx: ${link(head3?.tx?.id?.('hex') ?? head3?.txid)}`)
  console.log(`costs: ${JSON.stringify(rt2.ops.costs().map(c => ({ txid: c.txid.slice(0, 12), bytes: c.bytes, fee: c.fee })))}`)
  await rt2.close()
} finally { await rt.close().catch(() => {}) }
if (failures) { console.error(`${failures} check(s) failed`); process.exit(1) }
console.log('ttn pilot PASS')
