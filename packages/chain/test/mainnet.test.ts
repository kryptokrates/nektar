import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, P2PKH, UnlockingScript } from '@bsv/sdk'
import type { Broadcaster } from '../src/interfaces.js'
import { sha256 } from '@nektar/core'
import { BsvBroadcaster, fetchBroadcasterFeeRate, IndexTracker, tscToMerklePath, merklePathMainnet, discoverUtxos, probeTxStatus } from '../src/mainnet.js'
import { SpendGuard } from '../src/spendguard.js'
import { buildBump } from '../src/bump.js'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'

// -- recorded fixture helpers --------------------------------------------------
const stubFetch = (routes: Record<string, { status?: number; body?: any; raw?: string }>) => {
  const calls: string[] = []
  const f: typeof fetch = (input: any, init?: any) => {
    const url = String(input)
    calls.push(`${init?.method ?? 'GET'} ${url}`)
    for (const [k, v] of Object.entries(routes)) {
      if (url.includes(k)) {
        const status = v.status ?? 200
        return Promise.resolve(new Response(v.raw ?? JSON.stringify(v.body ?? {}), { status, headers: { 'content-type': 'application/json' } }))
      }
    }
    return Promise.resolve(new Response('not found', { status: 404 }))
  }
  ;(f as any).calls = calls
  return f
}
// same shape but captures request headers (callback-header assertions)
const stubFetchHdrs = (
  routes: Record<string, { status?: number; body?: any; raw?: string } | ((n: number) => { status?: number; body?: any; raw?: string })>
) => {
  const calls: Array<{ url: string; headers: Record<string, string> }> = []
  const f: typeof fetch = (input: any, init?: any) => {
    const url = String(input)
    calls.push({ url, headers: Object.fromEntries(Object.entries(init?.headers ?? {})) })
    for (const [k, v] of Object.entries(routes)) {
      if (url.includes(k)) {
        const r = typeof v === 'function' ? v(calls.filter((c) => c.url === url).length) : v
        return Promise.resolve(
          new Response(r.raw ?? JSON.stringify(r.body ?? {}), { status: r.status ?? 200, headers: { 'content-type': 'application/json' } })
        )
      }
    }
    return Promise.resolve(new Response('not found', { status: 404 }))
  }
  ;(f as any).calls = calls
  return f
}

const tx = () => {
  // minimal valid tx for broadcast tests
  const parent = new Transaction()
  parent.addOutput({ satoshis: 1000, lockingScript: new P2PKH().lock('1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa') })
  const t = new Transaction()
  t.addInput({ sourceTXID: parent.id('hex'), sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff })
  t.addOutput({ satoshis: 900, lockingScript: parent.outputs[0].lockingScript })
  return t
}

test('BsvBroadcaster: posts /tx with callback headers; 202 RECEIVED → success', async () => {
  const fetchImpl = stubFetchHdrs({ '/tx': { status: 202, body: { txid: 'aa', status: 202, txStatus: 'RECEIVED' } } })
  const b = new BsvBroadcaster({ fetchImpl, broadcasterUrl: 'http://bcast', callbackUrl: 'https://me.example', callbackToken: 'tok123' })
  const res = await b.broadcast(tx())
  assert.equal(res.status, 'success')
  const post = (fetchImpl as any).calls.find((c: any) => c.url === 'http://bcast/tx')
  assert.ok(post, 'posted to <base>/tx')
  assert.equal(post.headers['X-CallbackUrl'], 'https://me.example/v1/broadcaster-callback')
  assert.equal(post.headers['X-CallbackToken'], 'tok123')
  assert.equal(post.headers['X-FullStatusUpdates'], 'true')
})

test('BsvBroadcaster: 202 terminal status is an error; 400 carries the reason; NO WoC fallback', async () => {
  const fetchImpl = stubFetch({
    'bcast.example/tx': { status: 202, body: { txStatus: 'DOUBLE_SPEND_ATTEMPTED' } },
    'whatsonchain.example/tx/raw': { body: { txid: 'x' } }
  })
  const b = new BsvBroadcaster({ fetchImpl, broadcasterUrl: 'http://bcast.example', indexerUrl: 'http://whatsonchain.example' })
  const res = await b.broadcast(tx())
  assert.equal(res.status, 'error')
  assert.match(res.description!, /DOUBLE_SPEND_ATTEMPTED/)
  assert.ok(!(fetchImpl as any).calls.some((c: string) => c.includes('whatsonchain.example')), 'terminal status must NOT fall back to the index')

  const fetchImpl2 = stubFetch({
    'bcast.example/tx': {
      status: 400,
      body: { error: 'ERR_BAD_FEE', reason: 'fee too low', txid: 'aa', status: 400, title: 'bad', detail: 'fee below minimum', type: 'broadcaster' }
    },
    'whatsonchain.example/tx/raw': { body: { txid: 'x' } }
  })
  const b2 = new BsvBroadcaster({ fetchImpl: fetchImpl2, broadcasterUrl: 'http://bcast.example', indexerUrl: 'http://whatsonchain.example' })
  const res2 = await b2.broadcast(tx())
  assert.equal(res2.status, 'error')
  assert.match(res2.description!, /fee too low/)
  assert.ok(!(fetchImpl2 as any).calls.some((c: string) => c.includes('whatsonchain.example')), 'terminal 400 must NOT fall back to the index')
})

test('BsvBroadcaster: 503 twice then 202 → success, index never called; network error is an error, index never called', async () => {
  let n = 0
  const fetchImpl = stubFetchHdrs({
    'bcast.example/tx': () => (++n <= 2 ? { status: 503, body: { detail: 'busy' } } : { status: 202, body: { txStatus: 'RECEIVED' } }),
    'whatsonchain.example/tx/raw': { body: { txid: 'x' } }
  })
  const b = new BsvBroadcaster({ fetchImpl, broadcasterUrl: 'http://bcast.example', indexerUrl: 'http://whatsonchain.example' })
  const res = await b.broadcast(tx())
  assert.equal(res.status, 'success')
  assert.equal((fetchImpl as any).calls.filter((c: any) => c.url === 'http://bcast.example/tx').length, 3, 'two 503 retries then accepted')
  assert.ok(!(fetchImpl as any).calls.some((c: any) => c.url.includes('whatsonchain.example')), 'index never called')

  const fetchImpl2: typeof fetch = (input: any) => {
    if (String(input).includes('whatsonchain.example')) return Promise.resolve(new Response(JSON.stringify({ txid: 'x' }), { status: 200 }))
    throw new Error('ECONNREFUSED')
  }
  const b2 = new BsvBroadcaster({ fetchImpl: fetchImpl2, broadcasterUrl: 'http://bcast.example', indexerUrl: 'http://whatsonchain.example' })
  const res2 = await b2.broadcast(tx())
  assert.equal(res2.status, 'error', 'network error → error, the broadcaster is the only write path')
  const calls2: string[] = (fetchImpl2 as any).calls ?? []
  assert.ok(!calls2.some((c) => c.includes('whatsonchain.example')), 'index never called')
})

test('BsvBroadcaster: 5xx (non-503) is an error — the index has no write path', async () => {
  const fetchImpl = stubFetch({
    'bcast.example/tx': { status: 500, raw: 'oops' },
    'whatsonchain.example/tx/raw': { body: 'deadbeef', raw: '"deadbeef"' }
  })
  const b = new BsvBroadcaster({ fetchImpl, broadcasterUrl: 'http://bcast.example', indexerUrl: 'http://whatsonchain.example' })
  const res = await b.broadcast(tx())
  assert.equal(res.status, 'error')
  assert.ok(!(fetchImpl as any).calls.some((c: string) => c.includes('whatsonchain.example/tx/raw')), 'no index write path exists')
})

test('BsvBroadcaster fanout: every route gets the same bytes + token; first acceptance wins', async () => {
  const fetchImpl = stubFetchHdrs({
    'bcast.example/tx': { status: 500, raw: 'dead' },
    'arc2.example/tx': { status: 202, body: { txStatus: 'ACCEPTED' } }
  })
  const b = new BsvBroadcaster({
    fetchImpl,
    broadcasterUrl: 'http://bcast.example',
    broadcasterUrls: ['http://arc2.example'],
    callbackToken: 'tok'
  })
  const res = await b.broadcast(tx())
  assert.equal(res.status, 'success')
  const posts = (fetchImpl as any).calls.filter((c: any) => c.url.endsWith('/tx'))
  assert.equal(posts.length, 2, 'the same tx goes to every configured route')
  assert.ok(posts.some((c: any) => c.url === 'http://bcast.example/tx'))
  assert.ok(posts.some((c: any) => c.url === 'http://arc2.example/tx'))
  assert.ok(
    posts.every((c: any) => c.headers['X-CallbackToken'] === 'tok'),
    'one token tags every route — statuses merge'
  )
})

test('BsvBroadcaster fanout: a wire refusal anywhere outranks a bare transport failure', async () => {
  const fetchImpl: typeof fetch = (input: any) => {
    const url = String(input)
    if (url.includes('arc2.example')) return Promise.resolve(new Response(JSON.stringify({ txStatus: 'REJECTED', reason: 'double spend' }), { status: 202 }))
    throw new Error('ECONNREFUSED')
  }
  const b = new BsvBroadcaster({ fetchImpl, broadcasterUrl: 'http://bcast.example', broadcasterUrls: ['http://arc2.example'] })
  const res = await b.broadcast(tx())
  assert.equal(res.status, 'error')
  assert.match(res.description!, /REJECTED.*double spend/, 'the verdict explains the failure, not the dead route')
})

test('probeTxStatus: a secondary route answers when the primary errors; all-404 still lets the index say MINED', async () => {
  const fetchImpl = stubFetch({
    'bcast.example/tx': { status: 500, raw: 'dead' },
    'arc2.example/tx': { body: { txStatus: 'SEEN_ON_NETWORK' } }
  })
  assert.deepEqual(
    await probeTxStatus(
      { fetchImpl, broadcasterUrl: 'http://bcast.example', broadcasterUrls: ['http://arc2.example'], indexerUrl: 'http://whatsonchain.example' },
      't1'
    ),
    { status: 'SEEN_ON_NETWORK', answered: true }
  )
  // every route 404s (no stub) — 'not carried' answers the broadcaster leg,
  // but the index can still report the tx mined
  const fetchImpl2 = stubFetch({ 'whatsonchain.example': { body: { blockheight: 5 } } })
  assert.deepEqual(
    await probeTxStatus(
      { fetchImpl: fetchImpl2, broadcasterUrl: 'http://bcast.example', broadcasterUrls: ['http://arc2.example'], indexerUrl: 'http://whatsonchain.example' },
      't2'
    ),
    { status: 'MINED', answered: true }
  )
})

test('fetchBroadcasterFeeRate: /policy 100sat/1000B → 100 sat/kB', async () => {
  const fetchImpl = stubFetch({ '/policy': { body: { policy: { miningFee: { bytes: 1000, satoshis: 100 } } } } })
  assert.equal(await fetchBroadcasterFeeRate({ fetchImpl, broadcasterUrl: 'http://bcast' }), 100)
})

test('probeTxStatus: resolves early on first definitive status and aborts slow sibling', async () => {
  let slowAborted = false
  const fetchImpl: typeof fetch = (input: any, init?: RequestInit) => {
    const url = String(input)
    if (url.startsWith('http://fast')) return Promise.resolve(new Response(JSON.stringify({ txStatus: 'SEEN_ON_NETWORK' }), { status: 200 }))
    return new Promise((_resolve, reject) => {
      init?.signal?.addEventListener('abort', () => {
        slowAborted = true
        reject(new Error('aborted'))
      })
    })
  }
  const t0 = Date.now()
  const r = await probeTxStatus({ fetchImpl, broadcasterUrls: ['http://slow', 'http://fast'] }, 'aa'.repeat(32), { indexerFallback: false })
  const elapsed = Date.now() - t0
  assert.deepEqual(r, { status: 'SEEN_ON_NETWORK', answered: true })
  assert.ok(elapsed < 1_000, `expected early resolve, took ${elapsed}ms`)
  assert.equal(slowAborted, true, 'slow sibling fetch must be aborted once verdict lands')
})

test('IndexTracker: header cached; 429 retried', async () => {
  let headerCalls = 0
  const fetchImpl: typeof fetch = (input: any) => {
    const url = String(input)
    if (url.includes('/chain/info')) return Promise.resolve(new Response(JSON.stringify({ blocks: 850000 }), { status: 200 }))
    if (url.includes('/block/height/5')) {
      headerCalls++
      if (headerCalls === 1) return Promise.resolve(new Response('slow', { status: 429 }))
      return Promise.resolve(
        new Response(JSON.stringify({ hash: 'h5', previousblockhash: 'h4', merkleroot: 'mr', time: 1700000000, bits: '1d00ffff', nonce: 1 }), {
          status: 200
        })
      )
    }
    return Promise.resolve(new Response('nf', { status: 404 }))
  }
  const t = new IndexTracker({ fetchImpl, indexerUrl: 'http://whatsonchain' }, { requirePow: false }) // fixture headers carry no real PoW
  assert.equal(await t.currentHeight(), 850000)
  const h1 = await t.headerAtHeight(5)
  assert.equal(h1?.merkleRoot, 'mr')
  const h2 = await t.headerAtHeight(5)
  assert.equal(headerCalls, 2, 'one retry for 429, then cached — no third call')
  assert.deepEqual(h1, h2)
  assert.ok(await t.isValidRootForHeight('mr', 5))
  assert.ok(!(await t.isValidRootForHeight('nope', 5)))
})

test('tscToMerklePath: real tree roots verify for N=1..8', () => {
  const dsha = (b: Buffer) => Buffer.from(sha256(sha256(b)))
  const rootOf = (hashes: Buffer[]): Buffer => {
    if (hashes.length === 1) return hashes[0]!
    const next: Buffer[] = []
    for (let i = 0; i < hashes.length; i += 2) {
      const l = hashes[i]!,
        r = hashes[i + 1] ?? l
      next.push(dsha(Buffer.concat([l, r])))
    }
    return rootOf(next)
  }
  for (const n of [1, 2, 3, 5, 8]) {
    const internal = Array.from({ length: n }, (_, i) => dsha(Buffer.from(`tx${i}`))) // internal order
    const display = internal.map((h) => Buffer.from(h).reverse().toString('hex'))
    const rootDisplay = Buffer.from(rootOf(internal)).reverse().toString('hex')
    for (let i = 0; i < n; i++) {
      // TSC nodes are display-order hex siblings, '*' = odd self-pair
      const nodes: string[] = []
      let idx = i,
        level = internal.map((h) => Buffer.from(h))
      while (level.length > 1) {
        const sib = idx % 2 === 0 ? idx + 1 : idx - 1
        nodes.push(sib < level.length ? Buffer.from(level[sib]).reverse().toString('hex') : '*')
        const next: Buffer[] = []
        for (let k = 0; k < level.length; k += 2) next.push(dsha(Buffer.concat([level[k]!, level[k + 1] ?? level[k]!])))
        level = next
        idx = Math.floor(idx / 2)
      }
      const mp = tscToMerklePath({ index: i, txOrId: display[i]!, target: rootDisplay, nodes }, display[i], 100)
      assert.equal(mp.computeRoot(display[i]), rootDisplay, `N=${n} idx=${i}`)
    }
  }
})

test('merklePathMainnet (broadcaster BUMP) + discoverUtxos with recorded fixtures', async () => {
  const bumpHex = buildBump(['aa'.repeat(32), 'bb'.repeat(32)], 0, 850_000).toHex()
  // the fixture tx is built in-test so the index entry is self-consistent —
  // discover verifies hex → txid → P2PKH-to-address before trusting a row
  const addr = '1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa'
  const parent = new Transaction()
  parent.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0xffffffff, unlockingScript: new UnlockingScript(), sequence: 0xffffffff })
  parent.addOutput({ satoshis: 5000, lockingScript: new P2PKH().lock(addr) })
  const fetchImpl = stubFetch({
    'bcast.example/tx/aa': { body: { txStatus: 'IMMUTABLE', merklePath: bumpHex } },
    [`/address/${addr}/unspent`]: { body: [{ tx_hash: parent.id('hex'), tx_pos: 0, value: 5000 }] },
    '/tx/': { raw: parent.toHex() }
  })
  const mp = await merklePathMainnet({ fetchImpl, broadcasterUrl: 'http://bcast.example', indexerUrl: 'http://whatsonchain' }, 'aa'.repeat(32))
  assert.ok(mp?.merklePath, 'broadcaster BUMP hex parses')
  const utxos = await discoverUtxos({ fetchImpl, indexerUrl: 'http://whatsonchain' }, addr)
  assert.equal(utxos.length, 1)
  assert.equal(utxos[0].satoshis, 5000)
})

test('discoverUtxos: index entries are checked against their own bytes', async () => {
  const addr = '1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa'
  const other = '12cbQLTFMXRnSzktFkuoG3eHoMeFtpTu3S'
  const mk = (to: string, sats: number) => {
    const t = new Transaction()
    t.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0xffffffff, unlockingScript: new UnlockingScript(), sequence: 0xffffffff })
    t.addOutput({ satoshis: sats, lockingScript: new P2PKH().lock(to) })
    return t
  }
  const good = mk(addr, 7777)
  const wrongAddr = mk(other, 1000)
  const fakeTxid = 'cc'.repeat(32)
  const fetchImpl = stubFetch({
    [`/address/${addr}/unspent`]: {
      body: [
        { tx_hash: good.id('hex'), tx_pos: 0, value: 5000 }, // index lies about the amount — tx output wins
        { tx_hash: wrongAddr.id('hex'), tx_pos: 0, value: 1000 }, // pays someone else
        { tx_hash: fakeTxid, tx_pos: 0, value: 1000 }, // served hex hashes to a different txid
        { tx_hash: good.id('hex'), tx_pos: 5, value: 1000 } // vout does not exist
      ]
    },
    [`/tx/${good.id('hex')}`]: { raw: good.toHex() },
    [`/tx/${wrongAddr.id('hex')}`]: { raw: wrongAddr.toHex() },
    [`/tx/${fakeTxid}`]: { raw: good.toHex() }
  })
  const skips: string[] = []
  const utxos = await discoverUtxos({ fetchImpl, indexerUrl: 'http://whatsonchain' }, addr, (t, why) => skips.push(`${t.slice(0, 8)} ${why}`))
  assert.equal(utxos.length, 1)
  assert.equal(utxos[0].satoshis, 7777, 'satoshis come from the tx output, not the index value')
  assert.equal(skips.length, 3, 'wrong-address, wrong-txid, and missing-vout entries all skipped')
})

test('probeTxStatus: definitive answers vs "couldn\'t ask"', async () => {
  // broadcaster 404 = a real answer ("we do not carry this tx")
  const f404 = stubFetch({ 'bcast.example/tx/': { status: 404, body: { error: 'transaction not found' } } })
  assert.deepEqual(await probeTxStatus({ fetchImpl: f404, broadcasterUrl: 'http://bcast.example', indexerUrl: 'http://whatsonchain.example' }, 't1'), {
    status: '',
    answered: true
  })
  // every source rate-limited = nobody answered
  const f429 = stubFetch({
    'bcast.example/tx/': { status: 429, body: { error: 'slow down' } },
    'whatsonchain.example/tx/': { status: 429, body: {} }
  })
  assert.deepEqual(await probeTxStatus({ fetchImpl: f429, broadcasterUrl: 'http://bcast.example', indexerUrl: 'http://whatsonchain.example' }, 't1'), {
    status: '',
    answered: false
  })
  // broadcaster dead but WoC cross-check answers mined
  const fWoc = stubFetch({
    'bcast.example/tx/': { status: 500, body: {} },
    'whatsonchain.example/tx/': { body: { blockheight: 5 } }
  })
  assert.deepEqual(await probeTxStatus({ fetchImpl: fWoc, broadcasterUrl: 'http://bcast.example', indexerUrl: 'http://whatsonchain.example' }, 't1'), {
    status: 'MINED',
    answered: true
  })
  // a real status verdict answers at once
  const fSeen = stubFetch({ 'bcast.example/tx/': { body: { txStatus: 'SEEN_ON_NETWORK' } } })
  assert.deepEqual(await probeTxStatus({ fetchImpl: fSeen, broadcasterUrl: 'http://bcast.example', indexerUrl: 'http://whatsonchain.example' }, 't1'), {
    status: 'SEEN_ON_NETWORK',
    answered: true
  })
})

// -- SpendGuard ----------------------------------------------------------------
const okBroadcast: Broadcaster = { broadcast: () => Promise.resolve({ status: 'success', txid: 'x' }) }

test('SpendGuard: caps, interval, unknown input, ledger persistence', async () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'sg-'))
  const ledger = path.join(dir, 'ledger.json')
  // craft a tx spending parent:0 worth 10_000 sats with 9_000 out → 1000 fee
  const parent = new Transaction()
  parent.addOutput({ satoshis: 10_000, lockingScript: new P2PKH().lock('1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa') })
  const pid = parent.id('hex')
  const mkTx = () => {
    const t = new Transaction()
    t.addInput({ sourceTransaction: parent, sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff })
    t.addOutput({ satoshis: 9000, lockingScript: parent.outputs[0].lockingScript })
    return t
  }
  const resolve = (id: string) => (id === pid ? 10_000 : undefined)

  // unknown input → fail closed: no tracked value AND no attached source
  const noSourceTx = () => {
    const t = new Transaction()
    t.addInput({ sourceTXID: pid, sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff })
    t.addOutput({ satoshis: 9000, lockingScript: parent.outputs[0].lockingScript })
    return t
  }
  const g0 = new SpendGuard(okBroadcast, () => undefined, { ledgerFile: ledger, maxFeePerTx: 5000, maxPerDay: 100_000 })
  assert.match((await g0.broadcast(noSourceTx())).description!, /unknown input value/)

  // per-tx cap
  const g1 = new SpendGuard(okBroadcast, resolve, { ledgerFile: ledger, maxFeePerTx: 500, maxPerDay: 100_000 })
  assert.match((await g1.broadcast(mkTx())).description!, /fee 1000 sats > maxFeePerTx 500/)

  // ok under caps → recorded
  const g2 = new SpendGuard(okBroadcast, resolve, { ledgerFile: ledger, maxFeePerTx: 5000, maxPerDay: 1500, minIntervalMs: 0 })
  assert.equal((await g2.broadcast(mkTx())).status, 'success')
  // per-day cap hit on second tx (1500-1000=500 remaining < 1000)
  assert.match((await g2.broadcast(mkTx())).description!, /daily cap/)
  assert.equal(g2.costs().length, 1)
  // ledger persisted: new instance sees the spend
  const g3 = new SpendGuard(okBroadcast, resolve, { ledgerFile: ledger, maxFeePerTx: 5000, maxPerDay: 1500 })
  assert.equal(g3.spentToday(), 1000)
  assert.match((await g3.broadcast(mkTx())).description!, /daily cap/)

  // interval
  const g4 = new SpendGuard(okBroadcast, resolve, { ledgerFile: path.join(dir, 'l2.json'), minIntervalMs: 60_000, refuseOnInterval: true })
  assert.equal((await g4.broadcast(mkTx())).status, 'success')
  assert.match((await g4.broadcast(mkTx())).description!, /min interval/)
  // default: the interval delays instead of refusing
  const g5 = new SpendGuard(okBroadcast, resolve, { ledgerFile: path.join(dir, 'l3.json'), minIntervalMs: 300 })
  assert.equal((await g5.broadcast(mkTx())).status, 'success')
  const t0 = Date.now()
  assert.equal((await g5.broadcast(mkTx())).status, 'success')
  assert.ok(Date.now() - t0 >= 250, 'second broadcast waited out the interval')
})

test('SpendGuard: daily cap counts committed value, not just fees (escrow spends; refunds do not)', async () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'sg-'))
  const own = new P2PKH().lock('1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa')
  // own P2PKH utxo worth 5000
  const parent = new Transaction()
  parent.addOutput({ satoshis: 5000, lockingScript: own })
  const pid = parent.id('hex')
  const resolve = (id: string) => (id === pid ? 5000 : undefined)
  // escrow-like: own utxo → covenant output (foreign script) + change to self.
  // fee = 140, foreign = 210 → spend = 350 (a fee-only cap would miss the 210)
  const covenantScript = (await import('@bsv/sdk')).LockingScript.fromHex('76009c637576ab547a21' + 'ab'.repeat(40))
  const escrowTx = () => {
    const t = new Transaction()
    t.addInput({ sourceTransaction: parent, sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff })
    t.addOutput({ satoshis: 210, lockingScript: covenantScript })
    t.addOutput({ satoshis: 4650, lockingScript: own })
    return t
  }
  const g = new SpendGuard(okBroadcast, resolve, { ledgerFile: path.join(dir, 'l.json'), maxFeePerTx: 5000, maxPerDay: 400 })
  assert.equal((await g.broadcast(escrowTx())).status, 'success')
  assert.equal(g.spentToday(), 350, 'the covenant-committed value counts toward the daily cap')
  assert.match((await g.broadcast(escrowTx())).description!, /daily cap/, 'second escrow exceeds the cap')

  // a covenant-input tx (refund/claim/reveal) moves value BACK to self —
  // its spend is fee only, so a due refund still clears an exhausted cap
  const escParent = new Transaction()
  escParent.addOutput({ satoshis: 210, lockingScript: covenantScript })
  const refundTx = () => {
    const t = new Transaction()
    t.addInput({ sourceTransaction: escParent, sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff })
    t.addOutput({ satoshis: 170, lockingScript: own })
    return t
  }
  const g2 = new SpendGuard(okBroadcast, (id) => (id === escParent.id('hex') ? 210 : undefined), {
    ledgerFile: path.join(dir, 'l2.json'),
    maxPerDay: 50
  })
  assert.equal((await g2.broadcast(refundTx())).status, 'success', 'covenant-input spend counts fee only — refunds clear even a small cap')
  assert.equal(g2.spentToday(), 40)
})

test('SpendGuard: cost record shape carries spend — the ops costs() contract', async () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'sg-'))
  const own = new P2PKH().lock('1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa')
  const parent = new Transaction()
  parent.addOutput({ satoshis: 10_000, lockingScript: own })
  const pid = parent.id('hex')
  const t = new Transaction()
  t.addInput({ sourceTransaction: parent, sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff })
  t.addOutput({ satoshis: 9000, lockingScript: own })
  const g = new SpendGuard(okBroadcast, (id) => (id === pid ? 10_000 : undefined), { ledgerFile: path.join(dir, 'l.json') })
  assert.equal((await g.broadcast(t)).status, 'success')
  const [r] = g.costs()
  assert.match(r.txid, /^[0-9a-f]{64}$/)
  assert.equal(typeof r.bytes, 'number')
  assert.equal(typeof r.fee, 'number')
  assert.equal(
    typeof r.spend,
    'number',
    'spend — the daily-cap metric splitDailySpend sums; a dropped field degrades to NaN behind the `as` casts at the consumers'
  )
  assert.match(r.at, /^\d{4}-\d{2}-\d{2}T/)
})

test('importFundingTx: hex: raw tx imports; empty/malformed refs fail with clear errors', async () => {
  const { importFundingTx } = await import('../src/mainnet.js')
  // vout 0 pays addr/pkh, vout 1 pays someone else — only the matching one imports
  const addr = '1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa'
  const pkh = '62e907b15cbf27d5425399ebf6f0fb50ebb88f18'
  const parent = new Transaction()
  parent.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff })
  parent.addOutput({ satoshis: 300, lockingScript: new P2PKH().lock(addr) })
  parent.addOutput({ satoshis: 500, lockingScript: new P2PKH().lock('1BoatSLRHtKNngkdXEeobR76b53LETtpyT') })
  const hex = parent.toHex()
  const outs = await importFundingTx(undefined, 'hex:' + hex, pkh)
  assert.equal(outs.length, 1)
  assert.equal(outs[0].vout, 0)
  assert.equal(outs[0].satoshis, 300)
  // whitespace inside the wire is tolerated (files often carry a trailing \n)
  const outs2 = await importFundingTx(undefined, 'hex:  ' + hex.slice(0, 100) + '\n' + hex.slice(100) + '  ', pkh)
  assert.equal(outs2.length, 1)
  // empty / malformed refs → clear operator-facing errors, never an SDK reader crash
  await assert.rejects(() => importFundingTx(undefined, 'hex:', pkh), /empty transaction payload/)
  await assert.rejects(() => importFundingTx(undefined, 'hex:zz', pkh), /malformed transaction hex/)
  await assert.rejects(() => importFundingTx(undefined, 'hex:0', pkh), /malformed transaction hex/)
  await assert.rejects(() => importFundingTx(undefined, 'not-a-txid', pkh), /malformed txid/)
})

// G5: a second raw-tx source — index first, broadcaster rawTx fallback; the
// returned bytes must hash to the asked txid whatever the source
import { PrivateKey } from '@bsv/sdk'
const k = PrivateKey.fromRandom()
const withFetch = async (f: typeof fetch, fn: () => Promise<void>): Promise<void> => {
  const saved = globalThis.fetch
  ;(globalThis as { fetch: typeof fetch }).fetch = f
  try {
    await fn()
  } finally {
    ;(globalThis as { fetch: typeof fetch }).fetch = saved
  }
}

test('fetchRawTx: index hex serves, broadcaster rawTx backs it up', async () => {
  const mod = (await import('../src/mainnet.js')) as { fetchRawTx?: (c: unknown, t: string) => Promise<Transaction | undefined> }
  assert.ok(mod.fetchRawTx !== undefined, 'mainnet must export fetchRawTx')
  const tx = new Transaction()
  tx.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromASM('OP_1') })
  tx.addOutput({ satoshis: 1000, lockingScript: new P2PKH().lock(k.toPublicKey().toAddress()) })
  const txid = tx.id('hex')
  const cfg = { indexerUrl: 'http://whatsonchain', broadcasterUrl: 'http://arc' }
  await withFetch(
    stubFetch({
      [`http://whatsonchain/tx/${txid}/hex`]: { raw: tx.toHex() }
    }),
    async () => {
      const r = await mod.fetchRawTx!(cfg, txid)
      assert.equal(r?.id('hex'), txid)
    }
  )
  await withFetch(
    stubFetch({
      [`http://whatsonchain/tx/${txid}/hex`]: { status: 429, body: 'rate limited' },
      [`http://arc/tx/${txid}`]: { body: { rawTx: tx.toHex() } }
    }),
    async () => {
      const r = await mod.fetchRawTx!(cfg, txid)
      assert.equal(r?.id('hex'), txid, 'broadcaster rawTx is the fallback source')
    }
  )
})

test('fetchRawTx: EF-form rawTx parses; both sources down is undefined; wrong txid is refused', async () => {
  const mod = (await import('../src/mainnet.js')) as { fetchRawTx?: (c: unknown, t: string) => Promise<Transaction | undefined> }
  const srcTx = new Transaction()
  srcTx.addOutput({ satoshis: 2000, lockingScript: new P2PKH().lock(k.toPublicKey().toAddress()) })
  const tx = new Transaction()
  tx.addInput({
    sourceTXID: srcTx.id('hex'),
    sourceOutputIndex: 0,
    unlockingScript: UnlockingScript.fromASM('OP_1'),
    sourceTransaction: srcTx
  })
  tx.addOutput({ satoshis: 1000, lockingScript: new P2PKH().lock(k.toPublicKey().toAddress()) })
  const txid = tx.id('hex')
  const other = new Transaction()
  other.addInput({ sourceTXID: '11'.repeat(32), sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromASM('OP_1') })
  other.addOutput({ satoshis: 500, lockingScript: new P2PKH().lock(k.toPublicKey().toAddress()) })
  const cfg = { indexerUrl: 'http://whatsonchain', broadcasterUrl: 'http://arc' }
  await withFetch(
    stubFetch({
      [`http://whatsonchain/tx/${txid}/hex`]: { status: 404 },
      [`http://arc/tx/${txid}`]: { body: { rawTx: Buffer.from(tx.toEF()).toString('hex') } }
    }),
    async () => {
      const r = await mod.fetchRawTx!(cfg, txid)
      assert.equal(r?.id('hex'), txid, 'extended-format rawTx parses')
    }
  )
  await withFetch(stubFetch({}), async () => {
    assert.equal(await mod.fetchRawTx!(cfg, txid), undefined, 'no source answers → undefined')
  })
  await withFetch(stubFetch({ [`http://whatsonchain/tx/${txid}/hex`]: { raw: other.toHex() } }), async () => {
    assert.equal(await mod.fetchRawTx!(cfg, txid), undefined, 'bytes that hash to another txid are refused')
  })
})

test('BsvBroadcaster: transport throw retries through withRetry and succeeds on recovery', async () => {
  let attempts = 0
  const fetchImpl: typeof fetch = () => {
    attempts++
    if (attempts === 1) return Promise.reject(new Error('socket hang up'))
    return Promise.resolve(new Response(JSON.stringify({ txStatus: 'SEEN_ON_NETWORK' }), { status: 200, headers: { 'content-type': 'application/json' } }))
  }
  const b = new BsvBroadcaster({ broadcasterUrl: 'http://bcast.example', fetchImpl })
  const res = await b.broadcast(tx())
  assert.equal(res.status, 'success')
  assert.equal(attempts, 2, 'single jittered retry path handles transport throws')
})

test('BsvBroadcaster: circuit breaker skips unhealthy secondary after 3 consecutive failures', async () => {
  const hits: Record<string, number> = { good: 0, bad: 0 }
  const fetchImpl: typeof fetch = (input) => {
    const url = String(input)
    if (url.includes('good.example')) {
      hits.good++
      return Promise.resolve(new Response(JSON.stringify({ txStatus: 'SEEN_ON_NETWORK' }), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    hits.bad++
    return Promise.resolve(new Response('bad gateway', { status: 502 }))
  }
  const b = new BsvBroadcaster({
    broadcasterUrl: 'http://good.example',
    broadcasterUrls: ['http://bad.example'],
    fetchImpl
  })
  // First 3 broadcasts hit both routes; bad.example fails 3 times and trips its circuit
  for (let i = 0; i < 3; i++) {
    const r = await b.broadcast(tx())
    assert.equal(r.status, 'success')
  }
  const badHitsAfterTrip = hits.bad
  assert.ok(badHitsAfterTrip >= 3, 'unhealthy route tried at least 3 times before circuit opens')
  // 4th broadcast must bypass bad.example completely while good.example still serves
  const r4 = await b.broadcast(tx())
  assert.equal(r4.status, 'success')
  assert.equal(hits.bad, badHitsAfterTrip, 'open circuit must skip unhealthy secondary route')
  assert.equal(hits.good, 4)
})
