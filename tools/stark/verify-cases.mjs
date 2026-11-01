// emit verifier inputs: ok, tampered seal byte, wrong image id (one JSON per line)
import { readFileSync } from 'node:fs'
const p = JSON.parse(readFileSync(process.argv[2], 'utf8'))
const flip = (h, at) => h.slice(0, at) + (h[at] === '0' ? '1' : '0') + h.slice(at + 1)
const cases = {
  ok: { receipt: p.receipt, imageId: p.imageId },
  tampered: { receipt: flip(p.receipt, Math.floor(p.receipt.length / 2)), imageId: p.imageId },
  wrongImage: { receipt: p.receipt, imageId: flip(p.imageId, 0) }
}
console.log(JSON.stringify(cases[process.argv[3]]))
