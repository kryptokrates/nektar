import { Transaction, PrivateKey, P2PKH } from '@bsv/sdk'

const WOC = 'https://api.whatsonchain.com/v1/bsv/test'
const AGENTS = 'mkGWdCoVMNTFMPUGXzTgKmrxCYQGYmPiyH,mtGqhiiZEK9TyhLmhFv1M9xQZoEwuxPbnH,mhuvW4NGyZBbUgbJcXBYi3Xt3Tgta59TDk,n4HaV6Ha3eudFTSRKu3fv1CYVPUWFD6BCH,mhCDX2yzzJNwVnqCgbLfVG9PLuKoiYca7j,n3yjQRAom44eiNbqCXkYGmxCYFVpVZ8Zqc,mmQ1kNVQj7UVYmmXz7KDK4qzs1cmyiyaof,mrMZWw9qXR3yNbPBSRRKpjtbVKgywfzsmY,msVabQTMdb5YowktR9kMyRtZJyDuxGyGv4,mthsxEBA8hgmsfXxxB9mXgVeRznvBc1BJV'.split(',')
const SOURCES = [
  { txid: 'c0f9050565e06125be9cd222f8e2c0f2f0e59164a2ba1e6d06deeb842f3b9012', vout: 0, sats: 6503879, wif: 'L2T13mFKAppVNesNMNzVLP98jbbNPqkbvJokPd9EFsg6q52QE6c1', changeAddr: 'mqjkNt5ScrzwRc1fKnNygUV4hcCmWF2hGW' },
  { txid: '40f77ac68fc043ecc8756184528224ec897c1e5a43bf4e769913d991409a2d99', vout: 0, sats: 4612917, wif: 'L3SjXVjncPMrDTNH3669ASJPfUq2PNY3Z4xf2HW5BXwaybpTfadE' },
]
const PER = 1_000_000, FEE = 1_000

const tx = new Transaction()
for (const s of SOURCES) {
  const raw = await (await fetch(`${WOC}/tx/${s.txid}/hex`)).text()
  tx.addInput({ sourceTXID: s.txid, sourceOutputIndex: s.vout, sourceTransaction: Transaction.fromHex(raw), unlockingScriptTemplate: new P2PKH().unlock(PrivateKey.fromWif(s.wif)) })
}
const p2 = new P2PKH()
for (const a of AGENTS) tx.addOutput({ lockingScript: p2.lock(a), satoshis: PER })
const total = SOURCES.reduce((t, s) => t + s.sats, 0)
tx.addOutput({ lockingScript: p2.lock(SOURCES[0].changeAddr), satoshis: total - AGENTS.length * PER - FEE })
await tx.fee()
await tx.sign()
const hex = tx.toHex()
const r = await fetch(`${WOC}/tx/raw`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ txhex: hex }) })
console.log('txid:', tx.id('hex')); console.log('broadcast:', await r.text())
