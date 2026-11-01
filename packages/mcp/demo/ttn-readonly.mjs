// Read-only TTN smoke: Arcade policy fee, WoC tip, header check, and a real
// merkle path for a recent coinbase txid verified against TTN headers.
// NEVER broadcasts — GETs only.
import { fetchArcFeeRate, WocTracker, merklePathMainnet } from '@mamem/chain'

const cfg = {
  arcUrl: 'https://arcade-v2-ttn-us-1.bsvblockchain.tech',
  wocUrl: 'https://api.woc-ttn.bsvblockchain.tech/v1/bsv/test'
}

const fee = await fetchArcFeeRate(cfg)
console.log(`Arcade TTN policy miningFee → ${fee} sat/kB`)

const tracker = new WocTracker(cfg)
const tip = await tracker.currentHeight()
console.log(`WoC TTN tip height: ${tip}`)
const h = await tracker.headerAtHeight(tip - 1)
console.log(`header @${tip - 1}: hash=${h?.hash?.slice(0, 20)}… merkleRoot=${h?.merkleRoot?.slice(0, 20)}… time=${new Date((h?.time ?? 0) * 1000).toISOString()}`)
console.log(`isValidRootForHeight(own root): ${await tracker.isValidRootForHeight(h.merkleRoot, tip - 1)}`)

// a recent coinbase txid: WoC /block/height → first txid in the block
const block = await (await fetch(`${cfg.wocUrl}/block/height/${tip - 1}`)).json()
const coinbaseTxid = block.tx?.[0]
console.log(`recent coinbase txid: ${coinbaseTxid}`)
const mp = coinbaseTxid ? await merklePathMainnet(cfg, coinbaseTxid) : null
if (mp) {
  console.log(`merklePath: height=${mp.merklePath.blockHeight} status=${mp.status} verify=${mp.merklePath.verify(coinbaseTxid, tracker) ? 'PASS' : 'FAIL'}`)
} else {
  console.log('merklePathMainnet: no path (tx may not be mined yet)')
}
