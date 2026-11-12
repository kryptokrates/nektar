import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, P2PKH, type MerklePath } from '@bsv/sdk'
import { buildBump, stackWorld, stackChain, StackFunder, StackChain } from '@nektar/chain'
import { NektarMemory, createHttpApi } from '../src/index.js'
import type { Server } from 'node:http'

const serve = async (t: {
  skip: (m?: string) => void
}): Promise<{ chain: StackChain; funder: StackFunder; mem: NektarMemory; srv: Server; base: string } | undefined> => {
  const w = await stackWorld(t, 10)
  if (w === undefined) return
  const { chain, funder } = w
  const mem = new NektarMemory(chain, funder, funder.keyPair)
  const srv = createHttpApi(mem).listen(0)
  await new Promise((r) => srv.once('listening', r))
  const base = `http://127.0.0.1:${(srv.address() as { port: number }).port}`
  return { chain, funder, mem, srv, base }
}

// a real spend mined on the chain — returns the txid and its true BUMP
const minedTx = async (chain: StackChain, funder: StackFunder): Promise<{ txid: string; mp: MerklePath }> => {
  const parent = funder.parents[0]
  const tx = new Transaction()
  tx.addInput({ sourceTransaction: parent, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(funder.keyPair) })
  tx.addOutput({ satoshis: (parent.outputs[0].satoshis ?? 0) - 500, lockingScript: new P2PKH().lock(funder.keyPair.toPublicKey().toAddress()) })
  await tx.fee()
  await tx.sign()
  assert.equal((await chain.broadcast(tx)).status, 'success')
  await chain.mine(1)
  const mp = (await chain.getTx(tx.id('hex')))!.merklePath
  assert.ok(mp, 'mined spend carries the node BUMP')
  return { txid: tx.id('hex'), mp }
}

const cb = (base: string, path: string, body: unknown, headers: Record<string, string> = {}): Promise<Response> =>
  fetch(`${base}${path}`, { method: 'POST', headers: { 'content-type': 'application/json', ...headers }, body: JSON.stringify(body) })

const bodyOf = (txid: string, mp?: { blockHeight: number; toHex: () => string }): object => ({
  timestamp: new Date().toISOString(),
  txid,
  txStatus: 'MINED',
  ...(mp ? { blockHeight: mp.blockHeight, merklePath: mp.toHex() } : {})
})

test('broadcaster callback: MINED with a valid BUMP delivers the proof once', async (t) => {
  const _w = await serve(t)
  if (_w === undefined) return
  const { chain, funder, mem, srv, base } = _w
  try {
    const { txid, mp } = await minedTx(chain, funder)
    let fires = 0
    const seen = new Set<string>()
    mem.onTxProof((t, _mp) => {
      fires++
      seen.add(t)
    })
    const r = await cb(base, '/v1/broadcaster-callback', bodyOf(txid, mp))
    assert.equal(r.status, 200)
    assert.equal(fires, 1)
    assert.ok(seen.has(txid))
  } finally {
    srv.close()
  }
})

test('broadcaster callback: BUMP for a different txid or an invalid root is ignored (still 200)', async (t) => {
  const _w = await serve(t)
  if (_w === undefined) return
  const { chain, funder, mem, srv, base } = _w
  try {
    const { mp } = await minedTx(chain, funder)
    let fires = 0
    mem.onTxProof(() => {
      fires++
    })
    // BUMP whose leaf is a different tx — does not contain the claimed txid
    const other = buildBump(['ef'.repeat(32), 'ab'.repeat(32)], 0, mp.blockHeight)
    let r = await cb(base, '/v1/broadcaster-callback', bodyOf('cd'.repeat(32), other))
    assert.equal(r.status, 200)
    // BUMP containing the claimed txid but rooted at a height the chain disagrees with
    const wrong = buildBump(['ef'.repeat(32), 'ab'.repeat(32)], 0, 777_777)
    r = await cb(base, '/v1/broadcaster-callback', bodyOf('ef'.repeat(32), wrong))
    assert.equal(r.status, 200)
    assert.equal(fires, 0, 'no proof delivered for unverifiable callbacks')
  } finally {
    srv.close()
  }
})

test('broadcaster callback: legacy /v1/arc-callback path still works', async (t) => {
  const _w = await serve(t)
  if (_w === undefined) return
  const { chain, funder, mem, srv, base } = _w
  try {
    const { txid, mp } = await minedTx(chain, funder)
    let fires = 0
    mem.onTxProof(() => {
      fires++
    })
    const r = await cb(base, '/v1/arc-callback', bodyOf(txid, mp))
    assert.equal(r.status, 200)
    assert.equal(fires, 1)
  } finally {
    srv.close()
  }
})

test('broadcaster callback: NEKTAR_CALLBACK_TOKEN gates the webhook (Authorization: Bearer)', async (t) => {
  const _w = await serve(t)
  if (_w === undefined) return
  const { chain, funder, mem, srv, base } = _w
  const prev = process.env.NEKTAR_CALLBACK_TOKEN
  process.env.NEKTAR_CALLBACK_TOKEN = 'tok-secret'
  try {
    const { txid, mp } = await minedTx(chain, funder)
    let fires = 0
    mem.onTxProof(() => {
      fires++
    })
    const noTok = await cb(base, '/v1/broadcaster-callback', bodyOf(txid, mp))
    assert.equal(noTok.status, 401, 'callback without the token is rejected')
    assert.equal(fires, 0)
    const ok = await cb(base, '/v1/broadcaster-callback', bodyOf(txid, mp), { authorization: 'Bearer tok-secret' })
    assert.equal(ok.status, 200)
    assert.equal(fires, 1)
  } finally {
    srv.close()
    if (prev === undefined) delete process.env.NEKTAR_CALLBACK_TOKEN
    else process.env.NEKTAR_CALLBACK_TOKEN = prev
  }
})
