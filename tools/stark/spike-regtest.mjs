// S0.3: STARK membership against a REAL Teranode regtest anchor.
// remember 3 → anchor → BUMP → SPV prove (bundle) → STARK prove from the bundle
// → WASM verify → journal root == anchored R, size == anchored size.
import { readFileSync, existsSync, mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { randomBytes } from 'node:crypto'
import { openRuntime, configFromEnv } from '../../packages/mcp/dist/src/index.js'
import { proveMembershipStark, verifyZkm2 } from '../../packages/zk/dist/src/stark/index.js'
const env = (p) =>
  existsSync(p)
    ? Object.fromEntries(
        readFileSync(p, 'utf8')
          .split(/\r?\n/)
          .map((l) => l.match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/))
          .filter(Boolean)
          .map((m) => [m[1], m[2].trim()])
      )
    : {}
const local = env('.env')
const qs = env('infra/teranode-quickstart/.env')
const rt = await openRuntime(
  configFromEnv({
    NEKTAR_NETWORK: 'regtest',
    NEKTAR_DATA_DIR: mkdtempSync(join(tmpdir(), 'stark-')),
    NEKTAR_FUND_WIF: local.NEKTAR_REGTEST_WIF,
    NEKTAR_RPC_PASS: qs.rpc_pass,
    NEKTAR_RPC_USER: qs.rpc_user,
    NEKTAR_AUTOMINE: '1',
    NEKTAR_CONFIRM_MS: '1500'
  })
)
const S = `stark-${randomBytes(3).toString('hex')}`
for (const d of ['alpha fact', 'beta fact', 'gamma fact']) await rt.mem.remember(S, d, { source: 'spike' })
const a = await rt.mem.anchorShard(S)
console.log(`anchored ${a.txid}`)
let pr
for (let i = 0; i < 60; i++) {
  pr = await rt.mem.prove(S, 1)
  if (pr.result.ok) break
  await new Promise((r) => setTimeout(r, 2000))
}
console.log(`SPV prove: ${JSON.stringify(pr.result)}`)
const b = pr.bundle
const H = (h) => Uint8Array.from(Buffer.from(h, 'hex'))
const queryId = randomBytes(32)
const { bytes, ms } = await proveMembershipStark({ leaf: H(b.leaf), index: b.leafIndex, size: b.size, siblings: b.path.map(H), queryId })
const t = performance.now()
const v = verifyZkm2(bytes)
const vms = (performance.now() - t).toFixed(1)
const ok = v.ok && v.journal.root === b.anchor.R && v.journal.size === b.anchor.size && v.journal.queryId === queryId.toString('hex')
console.log(
  `STARK: prove ${ms} ms, proof ${bytes.length} B, verify ${vms} ms → ${v.ok ? 'valid' : v.reason}; root == anchored R (${b.anchor.R.slice(0, 16)}…): ${v.ok && v.journal.root === b.anchor.R}`
)
console.log(ok ? 'S0.3 PASS' : 'S0.3 FAIL')
await rt.close()
process.exit(ok ? 0 : 1)
