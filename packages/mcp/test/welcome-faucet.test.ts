// J2/J1 friction fixes: the first-boot welcome seed (a fresh instance answers
// non-empty even with every seed host dark) and the self-serve testnet drip
// (funding stops being a conversation). Memory-tier — no live infra.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import type { AddressInfo } from 'node:net'
import { PrivateKey } from '@bsv/sdk'
import { testWorld } from '@nektar/chain'
import { NektarMemory, createHttpApi } from '../src/index.js'
import { seedWelcome } from '../src/welcome.js'
import { faucetDrip, faucetEnabled, FAUCET_DEFAULT_SATS } from '../src/faucet.js'

const tmp = () => mkdtempSync(join(tmpdir(), 'nektar-wf-'))
const testAddr = () => PrivateKey.fromRandom().toAddress([0x6f])

test('welcome: empty boot seeds the shard once; contented boot and repeat boots are untouched', async (t) => {
  const w = await testWorld(t, 4)
  if (w === undefined) return
  const { chain, funder } = w
  const dir = tmp()
  const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 } })
  // fresh memory: only system shards → the welcome lands
  const first = await seedWelcome({ mem, dataDir: dir })
  assert.ok(first.seeded >= 3, `expected records, got ${first.seeded}`)
  assert.ok(mem.shards().includes('welcome'))
  const recs = await mem.recall('welcome')
  assert.ok(
    recs.items.some((r) => /receipt/i.test(r.text ?? '')),
    'recall surfaces the welcome content'
  )
  // repeat boot: the marker caps the seed at one shot even though 'welcome'
  // is the only user shard
  assert.equal((await seedWelcome({ mem, dataDir: dir })).seeded, 0)
  // a boot that already holds user content is never touched (fresh dataDir,
  // fresh marker — the guard is the shard scan, not the marker alone)
  const dir2 = tmp()
  const mem2 = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 } })
  await mem2.remember('work', 'the fix was a missing index')
  assert.equal((await seedWelcome({ mem: mem2, dataDir: dir2 })).seeded, 0)
  assert.ok(!mem2.shards().includes('welcome'))
})

test('faucet: gate honours env + network; caps hold across restarts', async (t) => {
  const w = await testWorld(t, 6, 1_000_000)
  if (w === undefined) return
  const { chain, funder } = w
  const dir = tmp()
  const env: Record<string, string> = { NEKTAR_NETWORK: 'testnet' }
  assert.equal(faucetEnabled(env), false, 'unset flag = off')
  env.NEKTAR_FAUCET = '1'
  assert.equal(faucetEnabled(env), true)
  env.NEKTAR_NETWORK = 'main'
  assert.equal(faucetEnabled(env), false, 'main is a hard refusal')
  env.NEKTAR_NETWORK = 'testnet'

  const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 } })
  const addr = testAddr()
  const bad = await faucetDrip({ mem, address: 'not-an-address', dataDir: dir, env })
  assert.equal(bad.ok, false)
  if (bad.ok === false) assert.equal(bad.status, 400)

  const r1 = await faucetDrip({ mem, address: addr, dataDir: dir, env })
  assert.ok(r1.ok)
  if (r1.ok) assert.equal(r1.sats, FAUCET_DEFAULT_SATS)

  // same address: once ever, persisted — a second process (fresh module
  // state, same ledger file) still refuses
  const r2 = await faucetDrip({ mem, address: addr, dataDir: dir, env })
  assert.equal(r2.ok, false)
  if (r2.ok === false) assert.equal(r2.status, 429)

  // a second address drips — then a tiny daily cap refuses the third
  assert.ok((await faucetDrip({ mem, address: testAddr(), dataDir: dir, env })).ok)
  const capped = await faucetDrip({ mem, address: testAddr(), dataDir: dir, env: { ...env, NEKTAR_FAUCET_DAILY_SATS: '1' } })
  assert.equal(capped.ok, false)
  if (capped.ok === false) assert.match(capped.error, /daily/)
})

test('faucet route: disabled → 404 without spending; enabled → public drip', async (t) => {
  const w = await testWorld(t, 6, 1_000_000)
  if (w === undefined) return
  const { chain, funder } = w
  const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 } })
  const dir = tmp()
  process.env.NEKTAR_DATA_DIR = dir
  const srv = createHttpApi(mem).listen(0)
  await new Promise((r) => srv.once('listening', r))
  const base = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`
  const addr = testAddr()
  try {
    const off = await fetch(`${base}/v1/faucet`, { method: 'POST', body: JSON.stringify({ address: addr }) })
    assert.equal(off.status, 404, 'unset NEKTAR_FAUCET stays closed')
    process.env.NEKTAR_FAUCET = '1'
    process.env.NEKTAR_NETWORK = 'testnet'
    const on = await fetch(`${base}/v1/faucet`, { method: 'POST', body: JSON.stringify({ address: addr }) })
    const j = (await on.json()) as { txid?: string; sats?: number; error?: string }
    assert.equal(on.status, 200, `drip failed: ${j.error}`)
    assert.ok(j.txid)
    const again = await fetch(`${base}/v1/faucet`, { method: 'POST', body: JSON.stringify({ address: addr }) })
    assert.equal(again.status, 429)
    const bad = await fetch(`${base}/v1/faucet`, { method: 'POST', body: JSON.stringify({ address: 'x' }) })
    assert.equal(bad.status, 400)
  } finally {
    delete process.env.NEKTAR_FAUCET
    delete process.env.NEKTAR_DATA_DIR
    delete process.env.NEKTAR_NETWORK
    srv.close()
  }
})
