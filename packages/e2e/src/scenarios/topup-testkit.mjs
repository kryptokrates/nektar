#!/usr/bin/env node
// topup-testkit.mjs — move sats from the real-stack treasury tip into the
// testkit treasury tip. Both live under NEKTAR_REGTEST_WIF, so this is one
// tx: real-stack tip in → [topup out | change out], then both .state files
// are re-pointed at the new txid. Suite runs drain the testkit tail under
// the 1M split floor (ensureFunded fails fast by design); this is the
// instant recovery when the parked-sat sweep isn't needed yet.
//
//   node packages/e2e/src/scenarios/topup-testkit.mjs [sats=60000000]
//
// Requires NEKTAR_REGTEST_WIF + rpc user/pass (repo .env provides them).

import { PrivateKey, Transaction, P2PKH, SatoshisPerKilobyte } from '@bsv/sdk'
import { readFileSync, writeFileSync, existsSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { TeranodeBroadcaster } from '../../../chain/dist/src/regtest.js'

const root = resolve(fileURLToPath(new URL('../../../..', import.meta.url)))
const envFile = join(root, '.env')
/** @type {Record<string, string | undefined>} */
const env = existsSync(envFile)
  ? Object.fromEntries(
      readFileSync(envFile, 'utf8')
        .split(/\r?\n/)
        .map((l) => l.match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/))
        .filter(Boolean)
        .map((m) => [m[1], m[2].trim()])
    )
  : {}
const E = (k) => process.env[k] ?? env[k]
const key = PrivateKey.fromWif(E('NEKTAR_REGTEST_WIF'))
const addr = key.toPublicKey().toAddress([0x6f])
const auth = 'Basic ' + Buffer.from(`${E('NEKTAR_RPC_USER')}:${E('NEKTAR_RPC_PASS')}`).toString('base64')
const rpc = async (m, ...params) => {
  const r = await fetch(E('NEKTAR_RPC_URL') ?? 'http://localhost:9292/', {
    method: 'POST',
    headers: { 'content-type': 'application/json', authorization: auth },
    body: JSON.stringify({ jsonrpc: '1.0', id: 1, method: m, params })
  })
  const j = await r.json()
  if (j.error) throw new Error(`${m}: ${JSON.stringify(j.error)}`)
  return j.result
}
const TOPUP = Number(process.argv[2] ?? 60_000_000)
const load = (f) => JSON.parse(readFileSync(join(root, '.state', f), 'utf8'))
const rsFile = 'real-stack-treasury.json'
const tkFile = 'testkit-treasury-chain.json'
const rs = load(rsFile)
const tip = rs.treasury
const src = Transaction.fromHex(tip.txHex)
const inSats = src.outputs[tip.vout].satoshis
if (inSats < TOPUP + 20_000) throw new Error(`real-stack tip has only ${inSats} sats — cannot move ${TOPUP}`)

const tx = new Transaction()
tx.addInput({ sourceTransaction: src, sourceOutputIndex: tip.vout, unlockingScriptTemplate: new P2PKH().unlock(key) })
tx.addOutput({ lockingScript: new P2PKH().lock(addr), satoshis: TOPUP })
tx.addOutput({ lockingScript: new P2PKH().lock(addr), change: true })
await tx.fee(new SatoshisPerKilobyte(100))
await tx.sign()
const r = await new TeranodeBroadcaster(E('NEKTAR_PROPAGATION_URL') ?? 'http://localhost:8833/tx').broadcast(tx)
if (r.status !== 'success') throw new Error(`topup refused: ${r.description}`)
await rpc('generatetoaddress', 1, addr)
writeFileSync(join(root, '.state', tkFile), JSON.stringify({ namespace: 'testkit', treasury: { txid: tx.id('hex'), vout: 0, txHex: tx.toHex() } }, null, 1))
rs.treasury = { txid: tx.id('hex'), vout: 1, txHex: tx.toHex() }
writeFileSync(join(root, '.state', rsFile), JSON.stringify(rs, null, 1))
console.log(`topup ${TOPUP} sats → testkit tip ${tx.id('hex').slice(0, 16)}:0; real-stack change ${tx.outputs[1].satoshis} sats`)
