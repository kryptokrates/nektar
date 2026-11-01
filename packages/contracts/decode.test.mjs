import { RunarContract, MockProvider, LocalSigner } from 'runar-sdk'
const artifact = JSON.parse((await import('node:fs')).readFileSync('MamemAnchor.json','utf8'))
const signer = new LocalSigner('1'.repeat(64)); const prov = new MockProvider('testnet')
const pk = await signer.getPublicKey(); prov.addUtxo(await signer.getAddress(),{txid:'ab'.repeat(32),outputIndex:0,satoshis:1e5,script:''})
const c = new RunarContract(artifact,[pk,7n,10n,'11'.repeat(32),'22'.repeat(32),'']);c.connect(prov,signer)
const s = c.getLockingScript()
// find OP_RETURN (6a) index and dump following bytes
const idx = s.lastIndexOf('6a')
console.log('tail hex:', s.slice(idx))
