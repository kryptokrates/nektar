#!/usr/bin/env node
// nektar verify-bundle — third-party auditor CLI.
// Verifies a ProofBundle or AuditBundle JSON against a header chain —
// no agent runtime needed. Usage:
//   node tools/verify-bundle.mjs <bundle.json> [--audit] [--headers headers.json] [--rpc <url>]
// headers.json: [{ "height": n, "hash": "<hex>", "merkleRoot": "<hex>", "time": n }]
// --rpc: a bitcoin-json-rpc node (getblockheader), e.g. http://localhost:9332
import fs from 'node:fs'
import { verifyFragment, verifyAuditBundle } from '../packages/verify/dist/index.js'

const args = process.argv.slice(2)
const file = args[0]
if (!file || args.includes('--help')) {
  console.log('usage: verify-bundle.mjs <bundle.json> [--audit] [--headers f.json] [--rpc url]')
  process.exit(2)
}
const audit = args.includes('--audit')
const hdrs = args.includes('--headers') ? args[args.indexOf('--headers') + 1] : undefined
const rpc = args.includes('--rpc') ? args[args.indexOf('--rpc') + 1] : undefined

// ── header source ────────────────────────────────────────────────────────
let tracker
if (hdrs) {
  const list = JSON.parse(fs.readFileSync(hdrs, 'utf8'))
  const byH = new Map(list.map((h) => [h.height, h]))
  const tip = Math.max(0, ...list.map((h) => h.height))
  tracker = {
    currentHeight: async () => tip,
    tipHeight: async () => tip,
    isValidRootForHeight: async (root, h) => byH.get(h)?.merkleRoot === root,
    headerAtHeight: async (h) => byH.get(h)
  }
} else if (rpc) {
  const call = (m, p) =>
    fetch(rpc, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ jsonrpc: '1.0', method: m, params: p }) })
      .then((r) => r.json())
      .then((j) => j.result)
  tracker = {
    currentHeight: async () => call('getblockcount', []),
    tipHeight: async () => call('getblockcount', []),
    isValidRootForHeight: async (root, h) => {
      const hh = await call('getblockhash', [h])
      const hdr = await call('getblockheader', [hh])
      return hdr?.merkleroot === root
    },
    headerAtHeight: async (h) => {
      const hh = await call('getblockhash', [h])
      return call('getblockheader', [hh])
    }
  }
} else {
  console.error('need --headers <file> or --rpc <url> for the header chain')
  process.exit(2)
}

const bundle = JSON.parse(fs.readFileSync(file, 'utf8'))

// ── verify ───────────────────────────────────────────────────────────────
const line = (ok, what, why = '') => console.log(`${ok ? 'PASS' : 'FAIL'}  ${what}${why ? ' — ' + why : ''}`)
let allOk = true
if (audit) {
  const r = await verifyAuditBundle(bundle, tracker, { isRevoked: bundle.isRevoked ? (p) => bundle.isRevoked.includes(p) : undefined })
  if (r.ok) line(true, 'audit bundle (receipt → fragments → signers → policy → anchors)')
  else
    for (const w of r.reasons) {
      line(false, 'audit bundle', w)
      allOk = false
    }
} else {
  const r = await verifyFragment(bundle, tracker)
  line(
    r.ok,
    `fragment ${bundle.shard ?? ''}/${bundle.leafIndex ?? ''} → anchor n=${bundle.n ?? ''}`,
    r.ok ? '' : `${r.reason}${r.detail ? ': ' + r.detail : ''}`
  )
  allOk = r.ok
}
console.log(allOk ? '\nVERDICT: VERIFIED (inclusion + anchor-chain integrity; truth of content is not established)' : '\nVERDICT: NOT VERIFIED')
process.exit(allOk ? 0 : 1)
