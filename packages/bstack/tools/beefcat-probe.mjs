// beefcat interop probe: parses a beefcat .bcat stream (lightwebinc/beefcat,
// the Go reference carrier implementation) with OUR seams — @bsv/sdk
// AtomicBEEF parse, bcommon mineable/unlocking refusals, and the
// provenParent rule from bstack/src/carrier.ts — against headers fetched
// from a /v1/tip + /v1/root/<h> source (beefcat's devchain, or our own
// header-shim). Run from the repo root or packages/bstack:
//   node packages/bstack/tools/beefcat-probe.mjs <stream.bcat> <headers-url>
// Produces verdicts with our rules on their bytes: agreement on the shared
// mechanics (parse, anchored parent BUMP, unmineable, canonical unlocking,
// one-in/one-out) is the interop signal. The record layer is the documented
// boundary: their PushDrop record vs our FUNDING_TAG decode diverges by
// design, so this probe stops at the shared carrier/SPV mechanics and does
// not claim record-format equivalence. REFUSED-* naming follows
// beefcat/docs/verification.md.
import { Transaction } from '@bsv/sdk'
import { mineableRefusal, unlockingRefusal } from '@lightwebinc/bcommon'
import { readFileSync } from 'node:fs'

const file = process.argv.at(2); const base = process.argv.at(3)
if (file === undefined || base === undefined) {
  console.error('usage: node beefcat-probe.mjs <stream.bcat> <headers-url>')
  process.exit(2)
}

// ChainTracker over the shim contract devchain and our header-shim share
const tracker = {
  async isValidRootForHeight(root, height) {
    const r = await fetch(`${base}/v1/root/${height}`)
    if (!r.ok) return false
    const body = await r.text()
    try {
      return JSON.parse(body).merkleRoot === root
    } catch {
      return body.trim() === root
    }
  },
  async currentHeight() {
    const r = await fetch(`${base}/v1/tip`)
    if (!r.ok) throw new Error(`tip ${r.status}`)
    return (await r.json()).height
  }
}

const buf = readFileSync(file)
const PRE = Buffer.from('BEEFCAT\x01')
if (!buf.subarray(0, 8).equals(PRE)) {
  console.error('not a beefcat stream (bad preamble)')
  process.exit(2)
}

const check = async (name, carrier, i) => {
  const tag = `carrier ${i} ${name}`
  // the provenParent rule from carrier.ts — parent must carry its own BUMP
  // that verifies against headers WE chose (here: the devchain URL arg)
  for (const input of carrier.inputs) {
    const p = input.sourceTransaction
    if (p === undefined) return `${tag}: no-parent`
    if (p.merklePath === undefined) return `${tag}: parent-unanchored`
    if (!(await p.merklePath.verify(p.id('hex'), tracker))) return `${tag}: parent-bump-refused`
  }
  return undefined
}

let pos = 8
let i = 0
let ok = 0
for (;;) {
  if (pos + 4 > buf.length) break
  const len = buf.readUInt32BE(pos)
  pos += 4
  const beef = Array.from(buf.subarray(pos, pos + len))
  pos += len
  let carrier
  try {
    carrier = Transaction.fromAtomicBEEF(beef)
  } catch (e) {
    console.log(`carrier ${i}: REFUSED-DECODE (${e.message.slice(0, 60)})`)
    i++
    continue
  }
  const refused = await check('spv', carrier, i)
  if (refused !== undefined) {
    console.log(refused)
    i++
    continue
  }
  const m = mineableRefusal(carrier)
  if (m !== undefined) {
    console.log(`carrier ${i}: REFUSED-MINEABLE (${m})`)
    i++
    continue
  }
  const u = unlockingRefusal(carrier)
  if (u !== undefined) {
    console.log(`carrier ${i}: REFUSED-UNLOCKING (${u})`)
    i++
    continue
  }
  const parent = carrier.inputs[0].sourceTransaction
  console.log(
    `carrier ${i}: VERIFIED  txid=${carrier.id('hex').slice(0, 16)}… ` +
      `parent=${parent.id('hex').slice(0, 16)}…@${parent.merklePath.blockHeight} ` +
      `lockTime=${carrier.lockTime} inputs=${carrier.inputs.length} outputs=${carrier.outputs.length}`
  )
  ok++
  i++
}
console.log(`\n${ok}/${i} carriers verified by our seams against ${base}`)
process.exit(ok === i && i > 0 ? 0 : 1)
