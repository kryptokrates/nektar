// Read-only testnet smoke: Arcade policy fee, bananablocks tip, header check,
// and a real merkle path for a txid passed as argv[2] verified against
// testnet headers. NEVER broadcasts — GETs only.
import { fetchBroadcasterFeeRate, IndexTracker, merklePathMainnet } from '@nektar/chain'

const cfg = {
  broadcasterUrl: 'https://arcade-v2-testnet-us-1.bsvblockchain.tech',
  indexerUrl: 'https://test.bananablocks.com/api/v1'
}

const fee = await fetchBroadcasterFeeRate(cfg)
console.log(`Arcade testnet policy miningFee → ${fee} sat/kB`)

const tracker = new IndexTracker(cfg)
const tip = await tracker.currentHeight()
console.log(`index testnet tip height: ${tip}`)
const h = await tracker.headerAtHeight(tip - 1)
console.log(`header @${tip - 1}: hash=${h?.hash.slice(0, 20)}… merkleRoot=${h?.merkleRoot.slice(0, 20)}… time=${new Date((h?.time ?? 0) * 1000).toISOString()}`)
console.log(`isValidRootForHeight(own root): ${await tracker.isValidRootForHeight(h?.merkleRoot, tip - 1)}`)

// bananablocks serves no block→txids list; pass a txid to prove the path
const txid = process.argv[2]
const mp = txid ? await merklePathMainnet(cfg, txid) : null
if (mp) {
  console.log(`merklePath: height=${mp.merklePath.blockHeight} status=${mp.status} verify=${(await mp.merklePath.verify(txid, tracker)) ? 'PASS' : 'FAIL'}`)
} else {
  console.log('no txid arg — skipping merkle-path check')
}
