import knex from 'knex'
import { Engine, KnexStorage } from '@bsv/overlay'
import allMigrations from '@bsv/overlay/storage/knex/all-migrations.ts'
import { Transaction, PrivateKey, ProtoWallet, MerklePath, Script, PushDrop } from '@bsv/sdk'
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
for (let i = 0; i < 5; i++) P.addOutput({ lockingScript: p2pkhLock(fundKey), satoshis: 10000 })
const ptxid = P.id('hex')
P.merklePath = new MerklePath(HEIGHT, [[{ offset: 0, hash: ptxid, txid: true }]])
validRoots.add(`${HEIGHT}:${P.merklePath.computeRoot(ptxid)}`)

const wallet = new ProtoWallet(shardKey)
const g = new Transaction()
g.addInput({ sourceTransaction: P, sourceOutputIndex: 0, unlockingScriptTemplate: p2pkhUnlock(fundKey) })
const fields = anchorFields({ shard: SHARD, n: 0, size: 10, R: dsha256('b0'), Rp: chainRp(ZERO32, dsha256('b0'), 10) })
g.addOutput({ lockingScript: await anchorLockingScript(wallet, SHARD, fields), satoshis: 1 })
g.addOutput({ lockingScript: p2pkhLock(fundKey), satoshis: 9000, change: true })
await g.fee()
await g.sign()
console.log('verify:', await g.verify(chainTracker))
console.log('parse:', parseAnchorFields(g.outputs[0].lockingScript))
const ls = createLsNektar()
const tm = createTmNektar(ls)
const beef = g.toBEEF()
console.log('tm direct:', await tm.identifyAdmissibleOutputs(beef, []))
const kx = knex({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
for (const m of allMigrations) await m.up(kx)
const engine = new Engine({ tm_nektar: tm }, { ls_nektar: ls }, new KnexStorage(kx), chainTracker, undefined, undefined, undefined, { broadcast: async () => ({ status: 'success' }) })
const steak = await engine.submit({ beef, topics: ['tm_nektar'] }, undefined, 'current-tx')
console.log('STEAK', JSON.stringify(steak))
process.exit(0)
