import knex from 'knex'
import { Engine, KnexStorage } from '@bsv/overlay'
import allMigrations from '@bsv/overlay/storage/knex/all-migrations.ts'
import { Transaction, PrivateKey, ProtoWallet, MerklePath, Script } from '@bsv/sdk'
import { anchorFields, parseAnchorFields, anchorLockingScript, anchorUnlockTemplate, chainRp, dsha256, ZERO32, p2pkhLock, p2pkhUnlock } from './lib/anchor.mjs'
import { createTmNektar } from './overlay/tm_nektar.mjs'
import { createLsNektar } from './overlay/ls_nektar.mjs'

const HEIGHT = 100
const fundKey = PrivateKey.fromRandom()
const shardKey = PrivateKey.fromRandom()
const SHARD = 'shard-alpha'
const validRoots = new Set()
const chainTracker = { currentHeight: async () => 1000, isValidRootForHeight: async (r, h) => validRoots.has(`${h}:${r}`) }

const P = new Transaction()
P.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0, unlockingScript: Script.fromHex('51') })
for (let i = 0; i < 10; i++) P.addOutput({ lockingScript: p2pkhLock(fundKey), satoshis: 10000 })
const ptxid = P.id('hex')
P.merklePath = new MerklePath(HEIGHT, [[{ offset: 0, hash: ptxid, txid: true }]])
validRoots.add(`${HEIGHT}:${P.merklePath.computeRoot(ptxid)}`)

const wallet = new ProtoWallet(shardKey)
const ls = createLsNektar()
for (const m of ['outputAdmittedByTopic','outputSpent']) {
  const orig = ls[m]
  ls[m] = (...a) => { console.log('LS', m, JSON.stringify(a[0]).slice(0,300)); return orig(...a) }
}
const tm = createTmNektar(ls)
const kx = knex({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
for (const m of allMigrations) await m.up(kx)
const logger = { log:()=>{}, info:()=>{}, warn:()=>{}, error:(...a)=>console.log('ENG-ERR',...a), debug:()=>{}, time:()=>{}, timeEnd:()=>{} }
const engine = new Engine({ tm_nektar: tm }, { ls_nektar: ls }, new KnexStorage(kx), chainTracker, undefined, undefined, undefined, { broadcast: async () => ({ status: 'success' }) }, undefined, undefined, false, '', false, undefined, logger)

async function mkAnchor(n, prev) {
  const t = new Transaction()
  if (prev) t.addInput({ sourceTransaction: prev.tx, sourceOutputIndex: 0,
    unlockingScriptTemplate: anchorUnlockTemplate(wallet, SHARD, prev.tx.outputs[0].satoshis, prev.tx.outputs[0].lockingScript) })
  t.addInput({ sourceTransaction: P, sourceOutputIndex: n, unlockingScriptTemplate: p2pkhUnlock(fundKey) })
  const prevRp = prev ? prev.fields.Rp : ZERO32
  const R = dsha256('batch'+n)
  const fields = anchorFields({ shard: SHARD, n, size: 10+n, R, Rp: chainRp(prevRp, R, 10+n) })
  t.addOutput({ lockingScript: await anchorLockingScript(wallet, SHARD, fields), satoshis: 1 })
  t.addOutput({ lockingScript: p2pkhLock(fundKey), satoshis: 9000, change: true })
  await t.fee(); await t.sign()
  const f = parseAnchorFields(t.outputs[0].lockingScript)
  return { tx: t, fields: f }
}

const g = await mkAnchor(0, null)
console.log('g verify', await g.tx.verify(chainTracker))
console.log('rows0',(await kx('outputs').count('* as c'))[0].c); console.log('steak0', JSON.stringify(await engine.submit({beef:g.tx.toBEEF(),topics:['tm_nektar']},undefined,'current-tx')))
console.log('head after g', ls.getHead(SHARD)?.txid)
const a1 = await mkAnchor(1, { tx: g.tx, fields: parseAnchorFields(g.tx.outputs[0].lockingScript) })
console.log('a1 prev field check', a1.fields)
console.log('rows1',(await kx('outputs').count('* as c'))[0].c); console.log('steak1', JSON.stringify(await engine.submit({beef:a1.tx.toBEEF(),topics:['tm_nektar']},undefined,'current-tx')))
console.log('head after a1', JSON.stringify(ls.getHead(SHARD)))

// fix: recreate a1 correctly and continue scenario
const a1b = await mkAnchor(1, { tx: g.tx, fields: parseAnchorFields(g.tx.outputs[0].lockingScript) })
const s1 = await engine.submit({beef:a1b.tx.toBEEF(),topics:['tm_nektar']},undefined,'current-tx')
console.log('steak1b', JSON.stringify(s1), 'head', ls.getHead(SHARD)?.txid?.slice(0,8))
const bad = await mkAnchor(5, { tx: a1b.tx, fields: parseAnchorFields(a1b.tx.outputs[0].lockingScript) })
const s2 = await engine.submit({beef:bad.tx.toBEEF(),topics:['tm_nektar']},undefined,'current-tx')
console.log('rowsB',(await kx('outputs').count('* as c'))[0].c); console.log('steakBad', JSON.stringify(s2), 'head', JSON.stringify(ls.getHead(SHARD)))

const rows = await kx('outputs').select('*')
console.log('outputs rows:', rows.length, rows.map(r=>({txid:r.txid?.toString?.().slice(0,8), oi:r.outputIndex, spent:r.spent, topic:r.topic})))
const txrows = await kx('transactions').select('txid')
console.log('tx rows:', txrows.length)
const ans = await engine.lookup({service:'ls_nektar',query:{shard:'shard-alpha'}})
console.log('lookup:', JSON.stringify(ans).slice(0,300))
process.exit(0)
