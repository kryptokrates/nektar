import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createServer, type Server } from 'node:http'
import type { AddressInfo } from 'node:net'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { spawn } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { PrivateKey } from '@bsv/sdk'
import { MockChain, MockFunder } from '@nektar/chain'
import { NektarMemory, type NektarMemoryOptions } from '../src/memory.js'
import { callTool } from '../src/server.js'
import { signWant, verifyWant, verifyOffer, wantTaskDigest, type SignedWant } from '@nektar/overlay'
import { offerDisclosure, verifyOfferSig, type SaleOffer } from '@nektar/identity'

process.env.NEKTAR_DATA_DIR = mkdtempSync(join(tmpdir(), 'nektar-publish-'))

function setup (opts: NektarMemoryOptions = {}) {
  const chain = new MockChain(); chain.verifyScripts = true
  const funder = new MockFunder(); funder.topup(50)
  for (const t of funder.parents) chain.registerParent(t)
  const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 }, modelId: 'test-model-1', ...opts })
  return { chain, funder, mem }
}

/** stub rendezvous: records every POSTed artifact, returns the shape serve.mjs gives */
async function stubRv () {
  const posts: Array<{ path: string, body: any }> = []
  const srv = createServer((req, res) => {
    let b = ''
    req.on('data', c => b += c)
    req.on('end', () => {
      posts.push({ path: req.url ?? '', body: b ? JSON.parse(b) : {} })
      res.writeHead(200, { 'content-type': 'application/json' })
      res.end(JSON.stringify({ ok: true, pub: (JSON.parse(b || '{}').want ?? JSON.parse(b || '{}').offer ?? {}).pub ?? 'x' }))
    })
  })
  srv.listen(0)
  await new Promise(r => srv.once('listening', r))
  return { srv, posts, url: `http://127.0.0.1:${(srv.address() as AddressInfo).port}` }
}

test('publishWant: desc is signed as question; the artifact carries a stable wantId', async (t) => {
  const { mem } = setup()
  const rv = await stubRv()
  t.after(() => rv.srv.close())
  const r = await callTool(mem, 'publishWant', { domains: ['soccer'], desc: 'need results', maxSats: 400, rendezvousUrl: rv.url }) as any
  assert.equal(r.ok, true)
  const w = r.want
  assert.equal(w.question, 'need results')
  assert.equal(typeof w.wantId, 'string')
  assert.equal(w.revision, 1)
  assert.equal(w.status, 'open')
  assert.equal(w.taskHash, wantTaskDigest({ domains: ['soccer'], question: 'need results', maxSats: 400 }))
  const postedWant = rv.posts.find(p => p.path === '/want')!.body
  assert.ok(verifyWant(w, postedWant.sig), 'posted artifact verifies')
  // retry with the same wantId keeps identity
  const r2 = await callTool(mem, 'publishWant', { domains: ['soccer'], desc: 'need results', maxSats: 400, wantId: w.wantId, revision: 2, previousHash: 'f'.repeat(64), rendezvousUrl: rv.url }) as any
  assert.equal(r2.want.wantId, w.wantId)
  assert.equal(r2.want.revision, 2)
})

test('publishOffer: wrapped never reaches the rendezvous; quote signature survives', async (t) => {
  const { mem } = setup()
  const rv = await stubRv()
  t.after(() => rv.srv.close())
  const sellerKey = mem.operatorKey()
  const leaf = 'f'.repeat(64)
  const sale: SaleOffer = offerDisclosure({ sellerKey, value: new TextEncoder().encode('secret'), price: 500n, refundAfter: 1_000_000n, maxFee: 10n, leaf })
  assert.ok(sale.wrapped.length > 0)
  const r = await callTool(mem, 'publishOffer', { shard: 's', seq: 0, priceSats: 500, domains: ['soccer'], leaf, offer: JSON.parse(JSON.stringify(sale, (_k, v) => typeof v === 'bigint' ? Number(v) : v)), rendezvousUrl: rv.url }) as any
  assert.equal(r.ok, true)
  const posted = rv.posts.find(p => p.path === '/offer')!.body
  const raw = JSON.stringify(posted)
  assert.ok(!raw.includes(sale.wrapped), 'reveal credential never leaves the instance')
  assert.ok(posted.offer.offer !== undefined && !('wrapped' in posted.offer.offer), 'nested quote is the public shape')
  assert.ok(verifyOffer(posted.offer, posted.sig), 'outer signature verifies')
  assert.ok(verifyOfferSig({ ...posted.offer.offer, cfg: { ...posted.offer.offer.cfg, price: BigInt(posted.offer.offer.cfg.price), refundAfter: BigInt(posted.offer.offer.cfg.refundAfter), maxFee: BigInt(posted.offer.offer.cfg.maxFee) } }, '', leaf), 'quote offerSig still verifies')
  assert.equal(posted.offer.offer.cfg.price, 500)
  // price mismatch between outer listing and quote is refused before signing
  await assert.rejects(callTool(mem, 'publishOffer', { shard: 's', seq: 0, priceSats: 1, domains: ['soccer'], leaf, offer: JSON.parse(JSON.stringify(sale, (_k, v) => typeof v === 'bigint' ? Number(v) : v)), rendezvousUrl: rv.url }), /price/)
  // a doctored offerSig is refused
  const doc = JSON.parse(JSON.stringify(sale, (_k, v) => typeof v === 'bigint' ? Number(v) : v)); doc.offerSig = '30' + '00'.repeat(35)
  await assert.rejects(callTool(mem, 'publishOffer', { shard: 's', seq: 0, priceSats: 500, domains: ['soccer'], leaf, offer: doc, rendezvousUrl: rv.url }), /offerSig/)
})

test('respond: requires exact want identity and binds it into the offer', async (t) => {
  const { mem } = setup()
  const rv = await stubRv()
  t.after(() => rv.srv.close())
  const buyer = PrivateKey.fromRandom()
  const w: SignedWant = signWant(buyer, { domains: ['soccer'], question: 'who won', maxSats: 900, ts: Date.now() })
  // missing identity fields are refused
  await assert.rejects(callTool(mem, 'respond', { wantPub: w.want.pub, shard: 's', seq: 0, priceSats: 300, domains: ['soccer'], rendezvousUrl: rv.url }), /wantId/)
  await assert.rejects(callTool(mem, 'respond', { wantPub: w.want.pub, wantId: w.want.wantId, shard: 's', seq: 0, priceSats: 300, domains: ['soccer'], rendezvousUrl: rv.url }), /taskHash/)
  await assert.rejects(callTool(mem, 'respond', { wantPub: w.want.pub, wantId: w.want.wantId, taskHash: w.want.taskHash, shard: 's', seq: 0, priceSats: 300, domains: ['soccer'], rendezvousUrl: rv.url }), /wantRevision/)
  const r = await callTool(mem, 'respond', { wantPub: w.want.pub, wantId: w.want.wantId, taskHash: w.want.taskHash, wantRevision: w.want.revision, shard: 's', seq: 0, priceSats: 300, domains: ['soccer'], rendezvousUrl: rv.url }) as any
  assert.equal(r.ok, true)
  const posted = rv.posts.find(p => p.path === '/offer')!.body
  assert.equal(posted.offer.wantId, w.want.wantId)
  assert.equal(posted.offer.taskHash, w.want.taskHash)
  assert.equal(posted.offer.wantRevision, w.want.revision)
  assert.equal(posted.offer.buyerPub, w.want.pub)
  assert.equal(posted.offer.replyTo, w.want.pub)
  assert.ok(verifyOffer(posted.offer, posted.sig))
})

// ── autoresponder: multi-want dedup, fail-closed cursor, authenticated local calls ──
const AUTO = join(fileURLToPath(new URL('.', import.meta.url)), '..', '..', '..', '..', 'tools', 'autorespond.mjs')
const TOKEN = 'test-token-' + 'a'.repeat(8)

async function stubSeller (opts: { failWantIds?: string[] }) {
  const responds: any[] = []
  const srv = createServer((req, res) => {
    const send = (code: number, v: unknown) => { res.writeHead(code, { 'content-type': 'application/json' }); res.end(JSON.stringify(v)) }
    if (req.headers.authorization !== `Bearer ${TOKEN}`) return send(401, { error: 'unauthorized' })
    const url = new URL(req.url ?? '/', 'http://x')
    if (url.pathname === '/.well-known/nektar') return send(200, { publicShards: [{ shard: 's', domains: ['soccer'], offers: [{ kind: 'answer', priceSats: 100 }] }] })
    let b = ''
    req.on('data', c => b += c)
    req.on('end', () => {
      if (url.pathname === '/v1/tools/reputation') return send(200, { card: { pub: 'self' } })
      if (url.pathname === '/v1/tools/respond') {
        const j = JSON.parse(b || '{}')
        responds.push(j)
        if (opts.failWantIds?.includes(j.wantId)) return send(500, { error: 'boom' })
        return send(200, { ok: true, pub: 'seller' })
      }
      send(404, { error: 'nope' })
    })
  })
  srv.listen(0)
  await new Promise(r => srv.once('listening', r))
  return { srv, responds, port: (srv.address() as AddressInfo).port }
}

async function stubRvFeed (wants: SignedWant[]) {
  const events = wants.map((w, i) => ({ seq: i + 1, kind: 'want', pub: w.want.pub, domains: w.want.domains, ts: w.want.ts, payload: w }))
  const srv = createServer((req, res) => {
    const url = new URL(req.url ?? '/', 'http://x')
    if (url.pathname === '/events') {
      const since = Number(url.searchParams.get('since') ?? -1)
      const out = events.filter(e => e.seq > since)
      res.writeHead(200, { 'content-type': 'application/json' })
      res.end(JSON.stringify({ cursor: out.length ? out[out.length - 1]!.seq : since, events: out }))
      return
    }
    res.writeHead(404); res.end('{}')
  })
  srv.listen(0)
  await new Promise(r => srv.once('listening', r))
  return { srv, url: `http://127.0.0.1:${(srv.address() as AddressInfo).port}` }
}

function runAutorespond (env: Record<string, string>): Promise<{ code: number | null, out: string, err: string }> {
  return new Promise(resolve => {
    const p = spawn(process.execPath, [AUTO, '--once'], { env: { ...process.env, ...env } })
    let out = '', err = ''
    p.stdout.on('data', d => out += d); p.stderr.on('data', d => err += d)
    p.on('exit', code => resolve({ code, out, err }))
  })
}

test('autoresponder: two wants from one pub both answered; dedup keyed by wantId; failure keeps cursor', async (t) => {
  const buyer = PrivateKey.fromRandom()
  const w1 = signWant(buyer, { domains: ['soccer'], maxSats: 500, ts: Date.now() })
  const w2 = signWant(buyer, { domains: ['soccer'], maxSats: 500, ts: Date.now() + 1 })
  const seller = await stubSeller({})
  const rv = await stubRvFeed([w1, w2])
  const dir = mkdtempSync(join(tmpdir(), 'nektar-auto-'))
  t.after(() => { seller.srv.close(); rv.srv.close() })
  const env = { NEKTAR_OVERLAY_URL: rv.url, NEKTAR_HTTP_PORT: String(seller.port), NEKTAR_DATA_DIR: dir, NEKTAR_HTTP_TOKEN: TOKEN }
  const r1 = await runAutorespond(env)
  assert.equal(r1.code, 0, r1.err || r1.out)
  assert.equal(seller.responds.length, 2, 'both independent wants answered — dedup is not per-publisher')
  assert.deepEqual(seller.responds.map(r => r.wantId).sort(), [w1.want.wantId, w2.want.wantId].sort())
  assert.equal(seller.responds[0]!.taskHash, w1.want.taskHash)
  // second pass: dedup — nothing new
  seller.responds.length = 0
  const r2 = await runAutorespond(env)
  assert.equal(r2.code, 0)
  assert.equal(seller.responds.length, 0, 'dedup survived restart via state file')
})

test('autoresponder: failed respond does not advance the cursor', async (t) => {
  const buyer = PrivateKey.fromRandom()
  const w1 = signWant(buyer, { domains: ['soccer'], maxSats: 500, ts: Date.now() })
  const w2 = signWant(buyer, { domains: ['soccer'], maxSats: 500, ts: Date.now() + 1 })
  const seller = await stubSeller({ failWantIds: [w1.want.wantId] })
  const rv = await stubRvFeed([w1, w2])
  const dir = mkdtempSync(join(tmpdir(), 'nektar-auto-fail-'))
  t.after(() => { seller.srv.close(); rv.srv.close() })
  const env = { NEKTAR_OVERLAY_URL: rv.url, NEKTAR_HTTP_PORT: String(seller.port), NEKTAR_DATA_DIR: dir, NEKTAR_HTTP_TOKEN: TOKEN }
  const r1 = await runAutorespond(env)
  assert.equal(r1.code, 0)
  assert.equal(seller.responds.length, 1, 'the failing want was attempted')
  seller.responds.length = 0
  const r2 = await runAutorespond(env)
  assert.equal(r2.code, 0)
  assert.equal(seller.responds.length, 1, 'the failed want is retried — cursor did not skip it')
  assert.equal(seller.responds[0]!.wantId, w1.want.wantId)
  assert.ok(!seller.responds.some(r => r.wantId === w2.want.wantId), 'wants behind the failure were not skipped past it')
})
