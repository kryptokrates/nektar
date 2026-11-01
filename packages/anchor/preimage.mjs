import { Transaction } from '@bsv/sdk'
import { createHash } from 'node:crypto'
const dsha = b => { const h1 = createHash('sha256').update(b).digest(); return createHash('sha256').update(h1).digest('hex') }
const txHex = process.argv[2].trim()
const tx = Transaction.fromHex(txHex)
// real preimage components (BCH sighash ALL|FORKID)
const po = Buffer.concat(tx.inputs.map(i => Buffer.concat([Buffer.from(i.sourceTXID ?? i.sourceTransaction.id('hex'), 'hex').reverse(), (() => { const b = Buffer.alloc(4); b.writeUInt32LE(i.sourceOutputIndex); return b })()])))
const seq = Buffer.concat(tx.inputs.map(i => { const b = Buffer.alloc(4); b.writeUInt32LE(i.sequence ?? 0xffffffff); return b }))
const outs = Buffer.concat(tx.outputs.map(o => o.toBinary != null ? Buffer.from(o.toBinary()) : Buffer.concat([(() => { const b = Buffer.alloc(8); b.writeBigUInt64LE(BigInt(o.satoshis)); return b })(), Buffer.from([o.lockingScript.toBinary().length]), Buffer.from(o.lockingScript.toBinary()))])))
const hp = dsha(po), hs = dsha(seq), ho = dsha(outs)
console.log('real hashPrevouts', hp)
console.log('real hashSequence', hs)
console.log('real hashOutputs', ho)
const unlock = tx.inputs[0].unlockingScript.toHex()
console.log('hashPrevouts embedded?', unlock.includes(hp))
console.log('hashSequence embedded?', unlock.includes(hs))
console.log('hashOutputs embedded?', unlock.includes(ho))
// also check the funding input's sig covers... check in0's embedded fields individually:
for (const [name, h] of [['hp', hp], ['hs', hs], ['ho', ho]]) {
  console.log(name, 'in unlockScript:', unlock.includes(h))
}
