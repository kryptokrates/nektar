#!/usr/bin/env node
import { appendFileSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { verifyTradeEvidence } from '../../../mcp/dist/src/automation/evidence.js'
const dir = process.argv[2]
if (!dir) {
  console.error('usage: pnpm scenarios:verify -- <evidence-dir>')
  process.exit(2)
}
const e = JSON.parse(readFileSync(join(dir, 's1-evidence.json'), 'utf8'))
const v = verifyTradeEvidence(e)
console.log(JSON.stringify(v, null, 1))
if (!v.ok) process.exit(1)
await certifyUnattended()
process.exit(0)

/** A11 — a green run certifies THIS commit + THIS config + THIS scenario suite
 *  for 7 days. The signing key is the operator's (NEKTAR_OPERATOR_KEY, WIF or
 *  hex) — it lives outside the agent keystore; the agent only ever sees the
 *  pub via NEKTAR_OPERATOR_PUB or <dataDir>/operator.pub. Skipping issuance
 *  when either is unset keeps this a no-op for evidence-only verify runs. */
async function certifyUnattended() {
  const keyEnv = process.env.NEKTAR_OPERATOR_KEY
  const dataDir = process.env.NEKTAR_DATA_DIR
  if (keyEnv === undefined || dataDir === undefined) {
    console.error('verify green — conformance.token not issued (NEKTAR_OPERATOR_KEY + NEKTAR_DATA_DIR certify unattended mode)')
    return
  }
  const { PrivateKey } = await import('@bsv/sdk')
  const conf = await import('../../../mcp/dist/src/automation/conformance.js')
  const key = /^[0-9a-fA-F]{64}$/.test(keyEnv.trim()) ? PrivateKey.fromHex(keyEnv.trim()) : PrivateKey.fromWif(keyEnv.trim())
  const pub = key.toPublicKey().toString().toLowerCase()
  mkdirSync(dataDir, { recursive: true })
  // the agent must trust the signer — record the pub in operator.pub when it
  // isn't already declared (NEKTAR_OPERATOR_PUB env would cover it too)
  const pubs = conf.operatorPubs(dataDir, process.env)
  if (!pubs.includes(pub)) appendFileSync(join(dataDir, 'operator.pub'), `${pub}\n`)
  const marketHosts = (process.env.NEKTAR_MARKET_HOSTS ?? '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
  const token = conf.issueToken(
    key,
    {
      gitCommit: conf.currentCommit(),
      configHash: conf.conformanceConfigHash({ network: process.env.NEKTAR_NETWORK ?? 'regtest', marketHosts }),
      suiteDigest: conf.suiteDigest(conf.defaultSuiteDir())
    },
    [...new Set([...pubs, pub])]
  )
  const file = join(dataDir, 'conformance.token')
  writeFileSync(file, JSON.stringify(token, null, 1))
  console.error(`conformance.token issued → ${file} (commit ${token.gitCommit.slice(0, 12)}, expires ${new Date(token.expiresAt).toISOString()})`)
}
