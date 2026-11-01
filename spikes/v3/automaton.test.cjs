// Spike 0.2 — MamemAutomaton Spend-validate test.
const test = require('node:test')
const assert = require('node:assert')
const { RunarContract, MockProvider, LocalSigner } = require('runar-sdk')
const { Spend, LockingScript } = require('@bsv/sdk')
const { createHash } = require('node:crypto')

const artifact = JSON.parse(require('fs').readFileSync('MamemAutomaton.json', 'utf8'))
const signer = new LocalSigner('11'.repeat(32))
const h160 = (pkHex) => createHash('ripemd160').update(createHash('sha256').update(Buffer.from(pkHex, 'hex')).digest()).digest('hex')
const sha256d = (b) => createHash('sha256').update(createHash('sha256').update(b).digest()).digest('hex')
const le8 = (n) => { const b = Buffer.alloc(8); b.writeBigUInt64LE(BigInt(n)); return b }

// δ: q0 -a-> q1, q1 -b-> q2, q1 -a-> q3, q2 -c-> q3
const TRIPLES = [[0n, 97n, 1n], [1n, 98n, 2n], [1n, 97n, 3n], [2n, 99n, 3n]]
const DELTA_ROOT = sha256d(Buffer.concat(TRIPLES.flat().map(le8)))

async function setup () {
  const prov = new MockProvider('testnet')
  const pk = await signer.getPublicKey()
  prov.addUtxo('1Q1pE5vPGEEMqRcVRMbtBK842Y6Pzo6nK9', { txid: 'ab'.repeat(32), outputIndex: 0, satoshis: 1e6, script: '' })
  const c = new RunarContract(artifact, [0n, DELTA_ROOT, h160(pk), 0n])
  c.connect(prov, signer)
  return c
}
const args = TRIPLES.flat()

// build the step tx and evaluate its contract input against the contract's
// own current output (the parent being spent)
async function evalStep (c, callArgs) {
  const pk = await signer.getPublicKey()
  const parentLock = c.currentUtxo.script
  const { tx } = await c.call('step', callArgs)
  const in0 = tx.inputs[0]
  const spend = new Spend({
    sourceTXID: c.currentUtxo.txid,
    sourceOutputIndex: c.currentUtxo.outputIndex ?? 0,
    sourceSatoshis: c.currentUtxo.satoshis ?? 1,
    lockingScript: LockingScript.fromHex(parentLock),
    transactionVersion: tx.version,
    otherInputs: tx.inputs.slice(1).map(i => ({
      sourceTXID: i.sourceTXID ?? i.sourceTransaction?.id('hex') ?? '',
      sourceOutputIndex: i.sourceOutputIndex ?? 0,
      sequence: i.sequence ?? 0xffffffff
    })),
    inputIndex: 0,
    unlockingScript: in0.unlockingScript,
    outputs: tx.outputs,
    inputSequence: in0.sequence ?? 0xffffffff,
    lockTime: tx.lockTime ?? 0
  })
  return spend.validate()
}

test('automaton: valid transition q0 -a-> q1 evaluates true', async () => {
  const c = await setup()
  await c.deploy({ satoshis: 1 })
  const pk = await signer.getPublicKey()
  assert.ok(await evalStep(c, ['00'.repeat(72), pk, ...args, 0n, 97n, 1n]),
    'valid transition must evaluate true')
})

test('automaton: invalid triple evaluates false', async () => {
  const c = await setup()
  await c.deploy({ satoshis: 1 })
  const pk = await signer.getPublicKey()
  assert.ok(!(await evalStep(c, ['00'.repeat(72), pk, ...args, 0n, 98n, 1n])),
    'sym not in δ must evaluate false')
})

test('automaton: wrong table (deltaRoot mismatch) evaluates false', async () => {
  const c = await setup()
  await c.deploy({ satoshis: 1 })
  const pk = await signer.getPublicKey()
  const wrong = args.map((x, i) => i === 8 ? x + 9n : x)
  assert.ok(!(await evalStep(c, ['00'.repeat(72), pk, ...wrong, 0n, 97n, 1n])),
    'tampered table must evaluate false')
})

test('automaton: wrong signer evaluates false', async () => {
  const c = await setup()
  await c.deploy({ satoshis: 1 })
  const pk2 = '03' + '5a'.repeat(32)
  assert.ok(!(await evalStep(c, ['00'.repeat(72), pk2, ...args, 0n, 97n, 1n])),
    'wrong signer must evaluate false')
})
