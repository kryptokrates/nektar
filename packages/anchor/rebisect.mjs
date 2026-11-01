import { Transaction } from '@bsv/sdk'
import { TeranodeBroadcaster } from '@mamem/chain'
import { readFileSync, existsSync } from 'node:fs'
const f = process.argv[2]
const hex = existsSync(f) ? readFileSync(f, 'utf8').trim() : f
const tx = Transaction.fromHex(hex)
const res = await new TeranodeBroadcaster('http://localhost:8833/tx').broadcast(tx)
console.log('rebroadcast', tx.id('hex'), '→', JSON.stringify(res).slice(0, 300))
