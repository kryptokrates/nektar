import knex from 'knex'
import { Engine } from '@bsv/overlay'
import { KnexStorage } from '@bsv/overlay'
import allMigrations from '@bsv/overlay/storage/knex/all-migrations.ts'
import { Transaction, PrivateKey, MerklePath, Script } from '@bsv/sdk'
import { p2pkhLock } from './lib/anchor.mjs'

const HEIGHT = 100
const fundKey = PrivateKey.fromRandom()
const tx = new Transaction()
tx.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0, unlockingScript: Script.fromHex('51') })
tx.addOutput({ lockingScript: p2pkhLock(fundKey), satoshis: 10000 })
const txid = tx.id('hex')
tx.merklePath = new MerklePath(HEIGHT, [[{ offset: 0, hash: txid, txid: true }]])
const root = tx.merklePath.computeRoot(txid)
console.log('root', root)
const chainTracker = { currentHeight: async () => 1000, isValidRootForHeight: async (r, h) => r === root && h === HEIGHT }
const kx = knex({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
for (const m of allMigrations) await m.up(kx)
const engine = new Engine({ t: { identifyAdmissibleOutputs: async () => ({ outputsToAdmit: [0], coinsToRetain: [] }), getDocumentation: async () => '', getMetaData: async () => ({ name: 't', shortDescription: '' }) } }, {}, new KnexStorage(kx), chainTracker, undefined, undefined, undefined, { broadcast: async () => ({ status: 'success' }) })
console.log('verifying...')
console.log('beef verify:', await tx.verify(chainTracker))
console.log('submitting...')
const steak = await engine.submit({ beef: tx.toBEEF(), topics: ['t'] }, undefined, 'current-tx')
console.log('STEAK', JSON.stringify(steak).slice(0, 500))
process.exit(0)
