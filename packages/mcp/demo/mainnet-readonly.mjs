// Read-only mainnet smoke: broadcaster policy fee rate, index tip + header check.
// NEVER broadcasts — this script only issues GETs.
import { fetchBroadcasterFeeRate, IndexTracker } from '@nektar/chain'

const fee = await fetchBroadcasterFeeRate()
console.log(`broadcaster policy miningFee → ${fee} sat/kB`)

const tracker = new IndexTracker()
const tip = await tracker.currentHeight()
console.log(`index tip height: ${tip}`)
const h = await tracker.headerAtHeight(tip - 1)
console.log(
  `header @${tip - 1}: hash=${h?.hash.slice(0, 20)}… merkleRoot=${h?.merkleRoot.slice(0, 20)}… time=${new Date((h?.time ?? 0) * 1000).toISOString()}`
)
console.log(`isValidRootForHeight(own root): ${await tracker.isValidRootForHeight(h?.merkleRoot, tip - 1)}`)
