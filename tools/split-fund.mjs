import { Transaction, PrivateKey, P2PKH } from '@bsv/sdk'

const INDEX = 'https://test.bananablocks.com/api/v1'
const ARC = 'https://arcade-v2-testnet-us-1.bsvblockchain.tech'
const AGENTS =
  'mkGWdCoVMNTFMPUGXzTgKmrxCYQGYmPiyH,mtGqhiiZEK9TyhLmhFv1M9xQZoEwuxPbnH,mhuvW4NGyZBbUgbJcXBYi3Xt3Tgta59TDk,n4HaV6Ha3eudFTSRKu3fv1CYVPUWFD6BCH,mhCDX2yzzJNwVnqCgbLfVG9PLuKoiYca7j,n3yjQRAom44eiNbqCXkYGmxCYFVpVZ8Zqc,mmQ1kNVQj7UVYmmXz7KDK4qzs1cmyiyaof,mrMZWw9qXR3yNbPBSRRKpjtbVKgywfzsmY,msVabQTMdb5YowktR9kMyRtZJyDuxGyGv4,mthsxEBA8hgmsfXxxB9mXgVeRznvBc1BJV'.split(
    ','
  )
// committed WIFs were swept — source keys come from the environment now.
// NEKTAR_SPLIT_SOURCE_WIF_1 / _2 for the two outpoints below.
const wifEnv = (n) => {
  const w = process.env[`NEKTAR_SPLIT_SOURCE_WIF_${n}`]
  if (!w) throw new Error(`set NEKTAR_SPLIT_SOURCE_WIF_${n}`)
  return w
}
const SOURCES = [
  {
    txid: 'c0f9050565e06125be9cd222f8e2c0f2f0e59164a2ba1e6d06deeb842f3b9012',
    vout: 0,
    sats: 6503879,
    wif: wifEnv(1),
    changeAddr: 'mqjkNt5ScrzwRc1fKnNygUV4hcCmWF2hGW'
  },
  {
    txid: '40f77ac68fc043ecc8756184528224ec897c1e5a43bf4e769913d991409a2d99',
    vout: 0,
    sats: 4612917,
    wif: wifEnv(2)
  }
]
const PER = 1_000_000,
  FEE = 1_000

const tx = new Transaction()
for (const s of SOURCES) {
  const raw = await (await fetch(`${INDEX}/tx/${s.txid}/hex`)).text()
  tx.addInput({
    sourceTXID: s.txid,
    sourceOutputIndex: s.vout,
    sourceTransaction: Transaction.fromHex(raw),
    unlockingScriptTemplate: new P2PKH().unlock(PrivateKey.fromWif(s.wif))
  })
}
const p2 = new P2PKH()
for (const a of AGENTS) tx.addOutput({ lockingScript: p2.lock(a), satoshis: PER })
const total = SOURCES.reduce((t, s) => t + s.sats, 0)
tx.addOutput({ lockingScript: p2.lock(SOURCES[0].changeAddr), satoshis: total - AGENTS.length * PER - FEE })
await tx.fee()
await tx.sign()
// the BSV broadcaster is the only write path — Extended Format so the
// source parents travel with the spend
const r = await fetch(`${ARC}/tx`, { method: 'POST', headers: { 'content-type': 'application/octet-stream' }, body: Buffer.from(tx.toEF()) })
console.log('txid:', tx.id('hex'))
console.log('broadcast:', await r.text())
