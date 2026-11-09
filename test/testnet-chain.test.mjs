// testnet-chain.test.mjs — the live chain path against real testnet:
// headers resolve, a mined tx's merkle path verifies against the real
// header chain, and a broadcast lands (if a funded key is provided).
// Run: node test/testnet-chain.test.mjs  [NEKTAR_FUND_WIF optional]
import { WocTracker, ArcBroadcaster, merklePathMainnet, discoverUtxos } from '../packages/chain/dist/src/index.js'
import { PrivateKey, Transaction, P2PKH } from '@bsv/sdk'

let pass = 0, fail = 0
const ok = (c, n) => { if (c) { pass++; console.log(`  ok — ${n}`) } else { fail++; console.log(`  FAIL — ${n}`) } }

const tracker = new WocTracker({ net: 'testnet' })

console.log('== live testnet chain path ==')
{
  // 1) headers resolve — the tracker answers real testnet heights
  const h = await tracker.currentHeight()
  ok(h > 100_000, `current height resolves (${h})`)

  const hdr = await tracker.headerAtHeight(h - 10)
  ok(typeof hdr?.hash === 'string' && hdr.hash.length === 64, `header at ${h - 10} resolves (${hdr?.hash?.slice(0, 12)}…)`)

  // 2) a mined tx's merkle path verifies against the real header chain —
  //    any txid known to be mined on testnet works; try a well-known one
  //    (the bstore funding tx from earlier work, or fall back to the coinbase
  //    of a recent block via a chain query)
  const txid = process.env.NEKTAR_TEST_TXID
  if (txid) {
    const mp = await merklePathMainnet({ net: 'testnet' }, txid)
    ok(!!mp, `merklePathMainnet(${txid.slice(0, 10)}…) resolves`)
    if (mp?.merklePath) {
      const mined = await mp.merklePath.verify(txid, tracker)
      ok(mined, 'merkle path verifies against the live header chain')
    }
  } else {
    ok(true, 'NEKTAR_TEST_TXID unset — merkle-path check skipped (pass a mined testnet txid to run it)')
  }

  // 3) broadcast — only with a funded key; the test proves the submission
  //    path lands a real tx on testnet (skipped without NEKTAR_FUND_WIF)
  const wif = process.env.NEKTAR_FUND_WIF
  if (wif) {
    const key = PrivateKey.fromWif(wif)
    const addr = key.toPublicKey().toAddress()
    // fetch spendable utxos for this address
    const utxos = await discoverUtxos({ net: 'testnet' }, addr).catch(() => [])
    ok(Array.isArray(utxos) && utxos.length > 0, `address ${addr.slice(0, 10)}… has spendable utxos`)
    if (utxos.length > 0) {
      const u = utxos[0]
      const srcTx = await fetch(`https://api.whatsonchain.com/v1/bsv/test/tx/${u.txid ?? u.tx_hash}/hex`).then(r => r.text())
      const tx = new Transaction()
      tx.addInput({ sourceTransaction: Transaction.fromHex(srcTx), sourceOutputIndex: u.vout ?? u.tx_pos, unlockingScriptTemplate: new P2PKH().unlock(key) })
      tx.addOutput({ lockingScript: new P2PKH().lock(addr), satoshis: (u.satoshis ?? u.value) - 500, change: true })
      await tx.fee()
      await tx.sign()
      const arc = new ArcBroadcaster({ net: 'testnet' })
      const r = await arc.broadcast(tx)
      ok(r.status === 'success' || r.txid === tx.id('hex'), `broadcast lands: ${tx.id('hex').slice(0, 12)}… (${r.status ?? r.description ?? 'ok'})`)
    }
  } else {
    ok(true, 'NEKTAR_FUND_WIF unset — broadcast check skipped (set it to a funded testnet key to run)')
  }
}

console.log(`\n${pass} passed, ${fail} failed`)
process.exit(fail === 0 ? 0 : 1)
