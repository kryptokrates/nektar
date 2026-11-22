// regtest.test.ts — the tx/proof lookup the regtest runtime's chain.getTx
// delegates to. Regression: without it mem.fetchTx returned nothing on
// regtest and bstack funding trees could never be marked proven.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, MerklePath, PrivateKey, P2PKH } from '@bsv/sdk'
import { regtestTx, type RegtestConfig } from '../src/regtest.js'

const cfg: RegtestConfig = { rpcUrl: 'http://rpc', rpcUser: 'u', rpcPass: 'p', propagationUrl: 'http://p/tx', assetUrl: 'http://asset/api/v1' }

async function aTx(): Promise<Transaction> {
  const key = PrivateKey.fromRandom()
  const parent = new Transaction()
  parent.addOutput({ lockingScript: new P2PKH().lock(key.toPublicKey().toAddress([0x6f])), satoshis: 1000 })
  const tx = new Transaction()
  tx.addInput({ sourceTransaction: parent, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(key) })
  tx.addOutput({ lockingScript: new P2PKH().lock(key.toPublicKey().toAddress([0x6f])), satoshis: 900 })
  await tx.sign()
  return tx
}

// stub fetch: POSTs to cfg.rpcUrl answer JSON-RPC getrawtransaction; GETs on
// the asset base answer the routed binary payloads (404 on misses)
const stub =
  (rawHex: string | undefined, routes: Record<string, { status: number; body?: Uint8Array | number[] }> = {}): typeof fetch =>
  (input: unknown) => {
    const url = String(input)
    if (url === cfg.rpcUrl) {
      const error = rawHex === undefined ? { code: -5, message: 'No such mempool or blockchain transaction' } : null
      return Promise.resolve(new Response(JSON.stringify({ jsonrpc: '1.0', id: 1, result: rawHex ?? null, error }), { status: 200 }))
    }
    const hit = Object.keys(routes).find((k) => url === `${cfg.assetUrl}/${k}`)
    const r = hit !== undefined ? routes[hit] : { status: 404 }
    return Promise.resolve(new Response(new Uint8Array(r.body ?? []), { status: r.status }))
  }

test('regtestTx: mined tx returns tx with attached MerklePath', async () => {
  const tx = await aTx()
  const txid = tx.id('hex')
  const mp = new MerklePath(812, [
    [
      { offset: 0, hash: txid, txid: true },
      { offset: 1, hash: 'cc'.repeat(32) }
    ]
  ])
  const got = await regtestTx(cfg, txid, stub(tx.toHex(), { [`merkle_proof/${txid}`]: { status: 200, body: mp.toBinary() } }))
  assert.ok(got !== undefined)
  assert.equal(got.id('hex'), txid)
  assert.ok(got.merklePath !== undefined)
  assert.equal(got.merklePath.blockHeight, 812)
})

test('regtestTx: unsealed tx returns the tx without a path (retried later)', async () => {
  const tx = await aTx()
  const txid = tx.id('hex')
  const got = await regtestTx(cfg, txid, stub(tx.toHex(), { [`merkle_proof/${txid}`]: { status: 404 } }))
  assert.ok(got !== undefined)
  assert.equal(got.merklePath, undefined)
})

test('regtestTx: unknown tx is undefined', async () => {
  const got = await regtestTx(cfg, 'ab'.repeat(32), stub(undefined))
  assert.equal(got, undefined)
})

// permanent state isolation: a treasury file namespaced for one consumer
// refuses to be spent by another — the convergence incident (NEKTAR_TREASURY_FILE
// pointed at another consumer's chain) corrupted both files silently
test('bootstrapReserve: a treasury file namespaced for another consumer is refused', async () => {
  const { mkdtempSync, writeFileSync } = await import('node:fs')
  const { join } = await import('node:path')
  const { tmpdir } = await import('node:os')
  const { bootstrapReserve, ReserveFunder } = await import('../src/regtest.js')
  const dir = mkdtempSync(join(tmpdir(), 'nektar-ns-'))
  const file = join(dir, 'chain.json')
  writeFileSync(file, JSON.stringify({ namespace: 'testkit', treasury: { txid: 'ab'.repeat(32), vout: 0, txHex: 'aa' } }))
  const reserve = new ReserveFunder(PrivateKey.fromRandom(), join(dir, 'reserve.json'))
  await assert.rejects(
    bootstrapReserve(
      cfg,
      PrivateKey.fromRandom(),
      reserve,
      () => {
        throw new Error('rpc must not be reached')
      },
      { treasuryFile: file, namespace: 'real-stack' }
    ),
    /namespaced 'testkit'/
  )
})

// G1: skip is a per-call pass-over, not a spend mark — a rejected parent
// must never be funded from, but the hint writes nothing
test('ReserveFunder.takeReserve: skip passes a candidate over for this call only', async () => {
  const { mkdtempSync } = await import('node:fs')
  const { join } = await import('node:path')
  const { tmpdir } = await import('node:os')
  const { Transaction: SdkTx, P2PKH: SdkP2PKH } = await import('@bsv/sdk')
  const { ReserveFunder } = await import('../src/regtest.js')
  const dir = mkdtempSync(join(tmpdir(), 'nektar-skip-'))
  const r = new ReserveFunder(PrivateKey.fromRandom(), join(dir, 'reserve.json'))
  const mk = (s: number) => {
    const tx = new SdkTx()
    tx.addOutput({ lockingScript: new SdkP2PKH().lock(PrivateKey.fromRandom().toPublicKey().toHash()), satoshis: s })
    return tx
  }
  const a = mk(1000)
  const b = mk(2000)
  ;(r.utxos as Array<{ tx: typeof a; vout: number; satoshis: number }>).push({ tx: a, vout: 0, satoshis: 1000 }, { tx: b, vout: 0, satoshis: 2000 })
  const take = r.takeReserve.bind(r) as unknown as (
    sc?: unknown,
    min?: unknown,
    skip?: (tx: typeof a, vout: number) => Promise<boolean>
  ) => Promise<{ tx: typeof a } | null>
  const u = await take(undefined, undefined, (tx) => Promise.resolve(tx === a))
  assert.equal(u?.tx, b)
  assert.equal((await take())?.tx, a, 'a skipped candidate is handed out later, not consumed')
})
