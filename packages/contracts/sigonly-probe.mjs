import { compile } from 'runar-compiler'
import fs from 'fs'
const src = `import { SmartContract, assert, checkSig, hash160, type PubKey, type Sig, type ByteString } from 'runar-lang'
export class SigOnly extends SmartContract {
  readonly ownerPkh: ByteString
  constructor (p: ByteString) { super(); this.ownerPkh = p }
  public spend (sig: Sig, pubKey: PubKey) {
    assert(hash160(pubKey) === this.ownerPkh)
    assert(checkSig(sig, pubKey))
  }
}`
const r = compile(src)
fs.writeFileSync('C:/tmp/sigonly.json', JSON.stringify(r.artifact))
console.log('script:', r.artifact.script, 'csep:', JSON.stringify(r.artifact.codeSeparatorIndices))
