// port-runtime.test.ts — openRuntime is THE assembly: an injected ChainPort +
// coin book boot the whole spine with zero chain I/O. This is the memory tier
// of the one-startup-path check; toolbox-runtime.test.ts holds the stack half
// (real regtest custody through the same function).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { testWorld, memoryPort, fundsOf } from '@nektar/chain'
import { openRuntime } from '../src/runtime.js'

void test('openRuntime: injected ChainPort + funds boot the whole spine on memory', async (t) => {
  const w = await testWorld(t, 8)
  if (w === undefined) return
  const dd = mkdtempSync(join(tmpdir(), 'nektar-port-'))
  const rt = await openRuntime(
    {
      network: 'testnet', // the port's own label must win, not cfg.network
      dataDir: dd,
      confirmEveryMs: 100,
      regtest: { rpcUrl: '', rpcUser: '', rpcPass: '', propagationUrl: '', assetUrl: '', fundWif: w.funder.keyPair.toWif() }
    },
    { chain: memoryPort(w.chain), funds: fundsOf(w.funder) }
  )
  try {
    assert.equal(rt.network, 'regtest') // the injected port's label is the network
    assert.equal(rt.ops, undefined) // ops surface is a live-net custody thing
    await rt.mem.remember('port-test', 'hello world')
    const a = await rt.mem.anchorShard('port-test')
    assert.ok(a?.txid)
    // the broadcast went through the port — the tx is in the memory mempool
    assert.ok(await w.chain.fetchTx(a.txid))
    await w.chain.mine(1)
    // the events substrate observes via port.observe → pushes 'tx' →
    // deliverTxProof: the anchor's BUMP lands without any test-side poking
    const deadline = Date.now() + 5_000
    while (rt.mem.anchorTx(a.txid)?.merklePath === undefined && Date.now() < deadline) {
      await new Promise((r) => setTimeout(r, 50))
    }
    assert.ok(rt.mem.anchorTx(a.txid)?.merklePath, 'verified proof delivered through the injected port')
    assert.equal(rt.mem.unprovenAnchors().length, 0)
  } finally {
    await rt.close()
  }
})

void test('openRuntime: liveCheck wires only where the port proves unspent', async (t) => {
  const w = await testWorld(t, 8)
  if (w === undefined) return
  const cfg = (funder: typeof w.funder) => ({
    network: 'testnet' as const,
    dataDir: mkdtempSync(join(tmpdir(), 'nektar-port-')),
    confirmEveryMs: 100,
    regtest: { rpcUrl: '', rpcUser: '', rpcPass: '', propagationUrl: '', assetUrl: '', fundWif: funder.keyPair.toWif() }
  })
  // a port whose spendStatus can never answer 'unspent' — the live-index
  // shape: spendCheck still wires (a verified 'spent' burns), liveCheck
  // must not — futile retried probes inside the serialized take queue
  // under a degraded index were the regression
  const liveLike = { ...memoryPort(w.chain), provesUnspent: undefined }
  const funds = fundsOf(w.funder)
  const rt = await openRuntime(cfg(w.funder), { chain: liveLike, funds })
  try {
    assert.equal(funds.liveCheck, undefined, 'no unspent oracle where the port cannot prove it')
    assert.equal(typeof funds.spendCheck, 'function', 'spend burns still wire on every spendStatus port')
  } finally {
    await rt.close()
  }
  const w2 = await testWorld(t, 8)
  if (w2 === undefined) return
  const funds2 = fundsOf(w2.funder)
  const rt2 = await openRuntime(cfg(w2.funder), { chain: memoryPort(w2.chain), funds: funds2 })
  try {
    assert.equal(typeof funds2.liveCheck, 'function', 'a first-party utxo view wires the revive oracle')
    assert.equal(await funds2.liveCheck!('f'.repeat(64), 0), false, 'an unknown outpoint answers unknown, never live')
  } finally {
    await rt2.close()
  }
})

void test('openRuntime: injected funds without a chain port refuses to assemble', async () => {
  const f = await (await import('@nektar/chain')).testFunder(1)
  await assert.rejects(openRuntime({ network: 'regtest', dataDir: mkdtempSync(join(tmpdir(), 'nektar-port-')) }, { funds: fundsOf(f) }), /injected chain port/)
})
