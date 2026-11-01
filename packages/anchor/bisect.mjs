// Bisect: plain P2PKH spend of a reserve coinbase output on regtest Teranode.
import { PrivateKey, Transaction, P2PKH, Utils } from '@bsv/sdk'
import { ReserveFunder, TeranodeBroadcaster } from '@mamem/chain'
import { readFileSync } from 'node:fs'

const qsEnv = Object.fromEntries(readFileSync('../../infra/teranode-quickstart/.env', 'utf8').split('\n').filter(l => l.includes('=')).map(l => l.split('=', 2).map(s => s.trim())))
const wifLine = readFileSync('../../.env','utf8').split(/\r?\n/).find(l=>l.startsWith('MAMEM_REGTEST_WIF='));
const key = PrivateKey.fromWif(process.env.MAMEM_REGTEST_WIF ?? wifLine.slice(18).trim())
const cfg = { rpcUrl: 'http://localhost:9292/', rpcUser: qsEnv.rpc_user ?? 'teranode', rpcPass: qsEnv.rpc_pass ?? '', propagationUrl: 'http://localhost:8833/tx', assetUrl: 'http://localhost:8090/api/v1' }
const auth = 'Basic ' + Buffer.from(`${cfg.rpcUser}:${cfg.rpcPass}`).toString('base64')
const rpc = async (m, ...p) => { const r = await fetch(cfg.rpcUrl, { method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: auth }, body: JSON.stringify({ jsonrpc: '1.0', id: 1, method: m, params: p }) }); const j = await r.json(); return j.result }
const reserve = new ReserveFunder(key, '../../.state.json')
await reserve.scanCoinbases(cfg, rpc, 1, 800)
const u = reserve.utxos.find(x => x.satoshis > 2000)
if (!u) throw new Error('no reserve utxo')
console.log('spending', u.tx.id('hex'), u.vout, u.satoshis)
const tx = new Transaction()
tx.addInput({ sourceTransaction: u.tx, sourceOutputIndex: u.vout, unlockingScriptTemplate: new P2PKH().unlock(key) })
tx.addOutput({ lockingScript: new P2PKH().lock(key.toPublicKey().toAddress([0x6f])), satoshis: u.satoshis - 10 })
await tx.sign()
const res = await new TeranodeBroadcaster(cfg.propagationUrl).broadcast(tx)
console.log('broadcast:', JSON.stringify(res).slice(0, 300))
