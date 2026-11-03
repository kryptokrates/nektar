// Live test-network pilot (BROADCASTS test coins): NEKTAR_NETWORK=testnet|ttn
//   NEKTAR_DATA_DIR=… NEKTAR_KEY_PASSPHRASE(_FILE)=… [NEKTAR_FUND_TXIDS=…] pnpm ttn:pilot
// genesis anchor → BUMP → SPV prove vs live headers → append → rotate →
// restart (no fork) → STARK membership vs the live anchor; explorer links + fees.
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { PrivateKey } from '@bsv/sdk'
import { configFromEnv, openRuntime } from '../dist/src/runtime.js'

let failures = 0
const check = (ok, name) => { console.log(`${ok ? '✔' : '✖'} ${name}`); if (!ok) failures++ }
const sleep = ms => new Promise(r => setTimeout(r, ms))

const env = {
  ...process.env,
  NEKTAR_NETWORK: process.env.NEKTAR_NETWORK === 'testnet' ? 'testnet' : 'ttn', // never main here
  NEKTAR_DATA_DIR: process.env.NEKTAR_DATA_DIR ?? mkdtempSync(path.join(tmpdir(), 'nektar-live-')),
  NEKTAR_BATCH: process.env.NEKTAR_BATCH ?? '8', // pilot closes batches explicitly
  NEKTAR_CONFIRM_MS: process.env.NEKTAR_CONFIRM_MS ?? '20000'
}
if (!env.NEKTAR_KEY_PASSPHRASE && !env.NEKTAR_KEY_PASSPHRASE_FILE) { console.error('NEKTAR_KEY_PASSPHRASE (or _FILE) required'); process.exit(1) }
const cfg = configFromEnv(env)
const link = txid => `${env.NEKTAR_NETWORK === 'testnet' ? 'https://test.whatsonchain.com' : 'https://woc-ttn.bsvblockchain.tech'}/tx/${txid}`
const shard = process.env.NEKTAR_PILOT_SHARD ?? `pilot-${Date.now().toString(36)}`

/** poll prove() until the anchor of `seq` carries a BUMP verified against live headers */
async function waitProven (rt, seq, maxMin = 25) {
  const deadline = Date.now() + maxMin * 60_000
  let r
  while (Date.now() < deadline) {
    r = (await rt.mem.prove(shard, seq)).result
    if (r.ok) return r
    process.stdout.write(`  … seq ${seq}: ${r.reason ?? 'pending'} (waiting for a block)\n`)
    await sleep(30_000)
  }
  return r
}

let rt = await openRuntime(cfg)
try {
  const w = await rt.ops.wallet()
  console.log(`network ${env.NEKTAR_NETWORK} · fee address ${w.address} · balance ${w.balanceSats} sats (${w.utxos} utxos) · caps ${JSON.stringify(w.caps)}`)
  if (w.balanceSats < 3000) { console.error(`fund ${w.address} (≥3000 sats)`); process.exit(2) }

  // 1. genesis anchor
  for (let i = 0; i < 3; i++) await rt.mem.remember(shard, `pilot record ${i} @ ${new Date().toISOString()}`, { source: 'pilot' })
  const a0 = await rt.mem.anchorShard(shard)
  check(!!a0?.txid, `genesis anchor broadcast n=${a0?.n}`)
  console.log(`  ${link(a0?.txid)}`)

  // 2. SPV proof against live headers
  const p0 = await waitProven(rt, 0)
  check(p0?.ok === true, `seq 0 proven: leaf → R → anchor → BUMP → live headers (${p0?.ok ? 'ok' : p0?.reason})`)

  // 3. append (spends the genesis head)
  for (let i = 3; i < 6; i++) await rt.mem.remember(shard, `pilot record ${i}`, { source: 'pilot' })
  const a1 = await rt.mem.anchorShard(shard)
  check(a1?.n === 1, `append anchor n=${a1?.n} spends the head`)
  console.log(`  ${link(a1?.txid)}`)

  // 4. owner rotation
  const rot = await rt.mem.rotateShard(shard, PrivateKey.fromRandom())
  check(!!rot?.txid, 'owner rotation broadcast')
  console.log(`  ${link(rot?.txid)}`)

  // 5. restart on the same data dir → continue the persisted head (no fork)
  await rt.close()
  await sleep(1000)
  rt = await openRuntime(cfg)
  for (let i = 6; i < 8; i++) await rt.mem.remember(shard, `pilot record ${i} after restart`, { source: 'pilot' })
  const a2 = await rt.mem.anchorShard(shard)
  check(a2?.n === 2, `after restart: anchor n=${a2?.n} spends the persisted (rotated) head`)
  console.log(`  ${link(a2?.txid)}`)

  // 6. STARK membership (Wright §13) against the live genesis anchor
  if (process.env.NEKTAR_PILOT_STARK !== '0') {
    const q = rt.mem.issueZkQuery('pilot-auditor')
    const { bytes, ms } = await rt.mem.proveMembership(shard, 1, q.queryId)
    const vm = await rt.mem.verifyMembership(shard, 1, bytes)
    check(vm.ok === true, `STARK membership (${bytes.length} B, ${ms} ms) verifies against the live SHA-256 anchor (${vm.reason ?? 'ok'})`)
  }

  // 7. later anchors: the broadcaster's 200/202 is the acceptance signal; the
  // BUMP is attached in the background once mined (NEKTAR_PILOT_WAIT_ALL=1 waits)
  if (process.env.NEKTAR_PILOT_WAIT_ALL === '1') {
    const p2 = await waitProven(rt, 6)
    check(p2?.ok === true, `post-restart seq 6 proven against live headers (${p2?.ok ? 'ok' : p2?.reason})`)
  } else {
    check(rt.ops.costs().some(c => c.txid === a2?.txid), 'post-restart anchor accepted by the broadcaster (200) — proof follows on mining')
  }

  const costs = rt.ops.costs()
  console.log('costs:')
  for (const c of costs) console.log(`  ${c.txid}  ${c.bytes} B  ${c.fee} sats  ${link(c.txid)}`)
  const w2 = await rt.ops.wallet()
  console.log(`spent today ${w2.spentTodaySats} sats · balance ${w2.balanceSats} sats`)
} finally { await rt.close().catch(() => {}) }
if (failures) { console.error(`${failures} check(s) failed`); process.exit(1) }
console.log('LIVE PILOT PASS')
