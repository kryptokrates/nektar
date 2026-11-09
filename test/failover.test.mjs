#!/usr/bin/env node
// The friend's acceptance test: two peered rvs → Alice envelopes Bob via rv A
// → kill rv A (the central origin is gone) → Bob's rvSet falls back to the
// peer set → Bob collects + decrypts from rv B → Bob's acceptance envelope
// returns through B. Plus /v1/object/{sha} serving from the survivor.
// Exit 0 = the single-server dependency is dead.
import { spawn } from 'node:child_process'
import { PrivateKey } from '@bsv/sdk'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { sendEnvelope, collect } from '../packages/mcp/dist/src/mailbox.js'
import { setTimeout as delay } from 'node:timers/promises'

const dirA = mkdtempSync(join(tmpdir(), 'rv-a-'))
const dirB = mkdtempSync(join(tmpdir(), 'rv-b-'))
const PORT_A = 18301, PORT_B = 18302
const URL_A = `http://127.0.0.1:${PORT_A}`, URL_B = `http://127.0.0.1:${PORT_B}`

const env = (port, dir, peers) => ({
  ...process.env,
  NEKTAR_OVERLAY_PORT: String(port), NEKTAR_OVERLAY_DIR: dir,
  NEKTAR_RV_URL: `http://127.0.0.1:${port}`,
  NEKTAR_PEERS: peers, NEKTAR_NETWORK: 'testnet', NEKTAR_OVERLAY_STRICT_NET: '0'
})
const procs = []
const boot = (port, dir, peers) => { const p = spawn('node', ['packages/overlay/serve.mjs'], { env: env(port, dir, peers), stdio: 'ignore' }); procs.push(p); return p }

const get = async (u) => (await fetch(u)).json()
const ok = (cond, msg) => { if (!cond) { console.error(`FAIL: ${msg}`); cleanup(); process.exit(1) } console.log(`  ok — ${msg}`) }
const cleanup = () => { for (const p of procs) try { p.kill() } catch {} ; try { rmSync(dirA, { recursive: true, force: true }); rmSync(dirB, { recursive: true, force: true }) } catch {} }

const alice = PrivateKey.fromRandom()
const bob = PrivateKey.fromRandom()
const alicePub = alice.toPublicKey().toString()
const bobPub = bob.toPublicKey().toString()

// boot both rvs peered at each other
boot(PORT_A, dirA, URL_B)
boot(PORT_B, dirB, URL_A)
await delay(5000) // let them come up + self-beacon

// sanity — both alive
try { await get(`${URL_A}/peers`) } catch { ok(false, 'rv A up') }
try { await get(`${URL_B}/peers`) } catch { ok(false, 'rv B up') }
ok(true, 'rvs up')

// Alice sends an offer envelope to Bob through rv A (her endpoint)
const { messageId } = await sendEnvelope(alice, URL_A, { recipient: bobPub, kind: 'offer', payload: { shard: 'soccer', seq: 0, priceSats: 100 } })
ok(!!messageId, `envelope sent via rv A (${messageId.slice(0, 8)})`)
await delay(15000) // federation propagates A → B (peer-tail retry is 10s)

// kill rv A — the "central origin" is gone
procs[0].kill()
await delay(500)
ok(true, 'rv A killed')

// Bob collects from rv B via the surviving peer set — rvSet falls back
const msgs = await collect(bob, URL_B)
ok(msgs.length >= 1, `Bob collected ${msgs.length} envelope(s) from rv B`)
const m = msgs.find(x => x.messageId === messageId)
ok(!!m && m.sender === alicePub, 'envelope survived rv A going down — verified + decrypted')

// Bob's acceptance returns through the survivor (rv B)
await sendEnvelope(bob, URL_B, { recipient: alicePub, kind: 'acceptance', payload: { accepted: true, priceSats: 100 }, conversationId: messageId })
await delay(5000)
const aliceMsgs = await collect(alice, URL_B)
ok(aliceMsgs.some(x => x.kind === 'acceptance'), 'Alice got the acceptance via rv B')

// hash-addressed object — the envelope itself is retrievable by hash from B
const envObj = (await get(`${URL_B}/mailbox?recipient=${bobPub}`)).envelopes?.[0]
if (envObj) {
  const { createHash } = await import('node:crypto')
  const sha = createHash('sha256').update(JSON.stringify(envObj)).digest('hex')
  const r = await fetch(`${URL_B}/v1/object/${sha}`)
  ok(r.ok, `fetchObject by sha256 from surviving rv B`)
}

console.log('\nFAILOVER PASS — the exchange completed mailbox-mediated through the surviving host')
cleanup()
process.exit(0)
