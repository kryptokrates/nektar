import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, writeFileSync, readFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { writeKeyFile, readKeyFile, isEncrypted, KeystoreError } from '../src/keystore.js'
import { configFromEnv } from '../src/runtime.js'
import { PrivateKey, P2PKH } from '@bsv/sdk'

test('keystore: round trip / wrong passphrase / tampered ct', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'ks-'))
  const f = path.join(dir, 'k.json')
  writeKeyFile(f, '{"wif":"L5"}', 'hunter2')
  assert.ok(isEncrypted(readFileSync(f, 'utf8')), 'file is encrypted on disk')
  assert.equal(readKeyFile(f, 'hunter2', true), '{"wif":"L5"}')
  assert.throws(() => readKeyFile(f, 'wrong', true), KeystoreError)
  const tampered = JSON.parse(readFileSync(f, 'utf8'))
  tampered.ct = '00' + tampered.ct.slice(2)
  const tf = path.join(dir, 't.json')
  writeFileSync(tf, JSON.stringify(tampered))
  assert.throws(() => readKeyFile(tf, 'hunter2', true), KeystoreError)
  // plaintext file refused when encryption is required
  const pf = path.join(dir, 'p.json')
  writeFileSync(pf, '{"a":1}')
  assert.throws(() => readKeyFile(pf, 'hunter2', true), /plaintext/)
})

const env = (e: Record<string, string>) => ({ NEKTAR_DATA_DIR: mkdtempSync(path.join(tmpdir(), 'rt-')), ...e }) as NodeJS.ProcessEnv

test('runtime main gate: confirm flag required', () => {
  assert.throws(() => configFromEnv(env({ NEKTAR_NETWORK: 'main', NEKTAR_KEY_PASSPHRASE: 'x' })), /I_UNDERSTAND/)
  const cfg = configFromEnv(env({ NEKTAR_NETWORK: 'main', NEKTAR_KEY_PASSPHRASE: 'x', NEKTAR_MAINNET_CONFIRM: 'I_UNDERSTAND' }))
  assert.equal(cfg.main?.maxFeePerTx, 1000)
  assert.equal(cfg.main?.maxSpendPerDay, 20_000)
  assert.equal(cfg.main?.minBroadcastIntervalMs, 250)
  assert.equal(cfg.confirmEveryMs, 60_000)
})

test('runtime ttn: no confirm flag required; testnet-encoded fee address', () => {
  const cfg = configFromEnv(env({ NEKTAR_NETWORK: 'ttn', NEKTAR_KEY_PASSPHRASE: 'x' }))
  assert.equal(cfg.main?.broadcasterUrl, 'https://arcade-v2-ttn-us-1.bsvblockchain.tech')
  assert.equal(cfg.main?.wocUrl, 'https://api.woc-ttn.bsvblockchain.tech/v1/bsv/test')
  assert.equal(cfg.main?.feeRateSatPerKb, 1)
  assert.equal(cfg.confirmEveryMs, 30_000)
  // ttn address encodes with testnet prefix (m/n…), mainnet with 1…
  const k = PrivateKey.fromRandom()
  assert.match(k.toPublicKey().toAddress([0x6f]), /^[mn]/)
  assert.match(k.toPublicKey().toAddress(), /^1/)
  // change scripts lock to the fee key's PKH regardless of address encoding
  const s1 = new P2PKH().lock(k.toPublicKey().toAddress())
  const s2 = new P2PKH().lock(k.toPublicKey().toAddress([0x6f]))
  assert.equal(s1.toHex(), s2.toHex(), 'PKH lock is encoding-independent')
})

test('runtime main: refuses without passphrase / with plaintext keys.json', async t => {
  const { openRuntime } = await import('../src/runtime.js')
  const base = { NEKTAR_NETWORK: 'main', NEKTAR_MAINNET_CONFIRM: 'I_UNDERSTAND' }
  // no passphrase
  const c1 = configFromEnv(env(base))
  await assert.rejects(openRuntime(c1), /NEKTAR_KEY_PASSPHRASE/)
  // plaintext keys.json refuses startup on main
  const c2 = configFromEnv(env({ ...base, NEKTAR_KEY_PASSPHRASE: 'pw' }))
  writeFileSync(path.join(c2.dataDir, 'keys.json'), JSON.stringify({ agentSeed: 'x', logKey: 'y', shardKeys: {} }))
  await assert.rejects(openRuntime(c2), /plaintext/)
})
