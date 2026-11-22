// local-minihost — a standalone market host for local runs. Uses the real
// bstack wire implementation (testing.minihost is the same admit() codec the
// dockerized bbox-host serves) against a real witnessed mainnet tracker.
// Usage: node tools/local-minihost.mjs [port]
import { minihost } from '../packages/bstack/dist/src/testing.js'
import { ChaintracksTracker, IndexTracker } from '../packages/chain/dist/src/mainnet.js'
import { WitnessedTracker } from '../packages/chain/dist/src/headers.js'

const port = Number(process.argv[2] ?? 8470)
const tracker = new WitnessedTracker(new ChaintracksTracker(), new IndexTracker(), {
  onDisagree: (h, p, w) => console.error(`headers disagree at ${h} (${p} vs ${w})`)
})
const h = await minihost(['main'], { tracker, port })
console.log(`minihost up at ${h.url} (net=main)`)
