#!/usr/bin/env node
// tools/automation.mjs — the operator's side of the autonomous marketplace.
// Works directly on the instance's automation.sqlite (local OS authority):
// policies and exception approvals are decided here, never by the agent.
//
//   node tools/automation.mjs policy show <file>       validate + print the hash
//   node tools/automation.mjs policy approve <file>    approve a policy (prints its hash)
//   node tools/automation.mjs policy revoke <hash>     stop all new work under it
//   node tools/automation.mjs policy list
//   node tools/automation.mjs approvals                pending human decisions
//   node tools/automation.mjs approve <approvalId>     approve exactly that action
//   node tools/automation.mjs deny <approvalId>
//   node tools/automation.mjs ops                      operations + states
//   node tools/automation.mjs status <operationId>     one operation + its event trail
//   node tools/automation.mjs budget <policyHash>
//
// Data dir: --data <dir> | NEKTAR_DATA_DIR | .env | ./.nektar
import { existsSync, readFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { createRequire } from 'node:module'
import { userInfo } from 'node:os'

const root = resolve(fileURLToPath(new URL('..', import.meta.url)))
const argv = process.argv.slice(2)
const flag = (k) => {
  const i = argv.indexOf(k)
  if (i < 0) return undefined
  const v = argv[i + 1]
  argv.splice(i, 2)
  return v
}
const env = existsSync(join(root, '.env'))
  ? Object.fromEntries(
      readFileSync(join(root, '.env'), 'utf8')
        .split(/\r?\n/)
        .map((l) => l.match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/))
        .filter(Boolean)
        .map((m) => [m[1], m[2].trim()])
    )
  : {}
const dataDir = resolve(flag('--data') ?? process.env.NEKTAR_DATA_DIR ?? env.NEKTAR_DATA_DIR ?? join(root, '.nektar'))
const json = argv.includes('--json') ? (argv.splice(argv.indexOf('--json'), 1), true) : false
const dist = join(root, 'packages/automation/dist/src')
if (!existsSync(join(dist, 'store.js'))) {
  console.error('automation: no build — run pnpm -r build first')
  process.exit(1)
}
const { AutomationStore } = await import(pathToFileURL(join(dist, 'store.js')).href)
const { policyHash, policyError } = await import(pathToFileURL(join(dist, 'policy.js')).href)
const knexFactory = createRequire(join(root, 'packages/mcp/package.json'))('knex')

const [cmd, a, b] = argv
const out = (v) => console.log(json ? JSON.stringify(v, null, 1) : typeof v === 'string' ? v : JSON.stringify(v, null, 1))
const die = (m, code = 1) => {
  console.error(`automation: ${m}`)
  process.exit(code)
}
const loadPolicy = (f) => {
  if (!f || !existsSync(f)) die(`policy file not found: ${f}`)
  const p = JSON.parse(readFileSync(f, 'utf8'))
  const e = policyError(p)
  if (e) die(`invalid policy — ${e}`)
  return p
}
if (cmd === 'policy' && a === 'show') {
  const p = loadPolicy(b)
  out({ policyHash: policyHash(p), policy: p })
  process.exit(0)
}
if (!cmd || cmd === 'help' || cmd === '--help') {
  out(
    readFileSync(fileURLToPath(import.meta.url), 'utf8')
      .split('\n')
      .filter((l) => l.startsWith('//'))
      .map((l) => l.slice(3))
      .join('\n')
  )
  process.exit(0)
}

const file = join(dataDir, 'automation.sqlite')
if (!existsSync(file) && !(cmd === 'policy' && a === 'approve'))
  die(`no automation state at ${file} — start the instance with NEKTAR_AUTOMATION=1 or approve a policy first`)
const kx = knexFactory({ client: 'sqlite3', connection: { filename: file }, useNullAsDefault: true })
const store = new AutomationStore(kx)
const who = `cli:${userInfo().username}`
try {
  await store.init()
  if (cmd === 'policy' && a === 'approve') {
    const p = loadPolicy(b)
    const hash = await store.approvePolicy(p, who)
    out(
      json
        ? { policyHash: hash }
        : `approved ${p.policyId} for ${p.network} as ${hash}\n  per purchase ≤ ${p.limits.perPurchaseSats} sats · per day ≤ ${p.limits.dailyPurchaseSats} · fees ≤ ${p.limits.perTxFeeSats}/tx, ${p.limits.dailyFeeSats}/day · locked ≤ ${p.limits.maxLockedSats} · run total ≤ ${p.limits.runTotalSats}`
    )
  } else if (cmd === 'policy' && a === 'revoke') {
    if (!b) die('policy hash required')
    await store.revokePolicy(b)
    out(json ? { revoked: b } : `revoked ${b} — no new work will start under it; existing obligations stay monitored`)
  } else if (cmd === 'policy' && a === 'list') out({ policies: await store.policies() })
  else if (cmd === 'approvals') {
    const list = await store.approvals('pending')
    if (json) out({ approvals: list })
    else if (!list.length) out('no pending approvals')
    else
      for (const x of list)
        out(
          `${x.approvalId}\n  operation ${x.operationId}\n  reason    ${x.reason}\n  exposure  ≤ ${x.maxExtraSats} sats\n  expires   ${new Date(x.expiresAt).toISOString()}\n  evidence  ${JSON.stringify(x.evidence)}`
        )
  } else if (cmd === 'approve' || cmd === 'deny') {
    if (!a) die('approvalId required')
    const r = await store.decideApproval(a, cmd === 'approve', who)
    out(json ? r : `${r.status} ${r.approvalId} (${r.reason}) — applies to that exact action only`)
  } else if (cmd === 'ops') {
    const ops = await store.operations()
    if (json) out({ operations: ops.map((o) => ({ operationId: o.operationId, role: o.role, kind: o.kind, state: o.state, lastError: o.lastError })) })
    else for (const o of ops) out(`${o.operationId}  ${o.role.padEnd(8)} ${o.kind.padEnd(9)} ${o.state}${o.lastError ? `  — ${o.lastError}` : ''}`)
  } else if (cmd === 'status') {
    const o = a && (await store.operation(a))
    if (!o) die('unknown operationId')
    const data = { ...o.data }
    if (o.role === 'seller' && data.offer) data.offer = { ...data.offer, wrapped: '[sealed]' }
    out({ operation: { ...o, data }, events: await store.events(a) })
  } else if (cmd === 'budget') {
    if (!a) die('policyHash required')
    out(await store.budgetStatus(a))
  } else die(`unknown command "${cmd}" — try: node tools/automation.mjs help`, 2)
} catch (e) {
  console.error(`automation: ${e.message}`)
  process.exitCode = 1
} finally {
  await kx.destroy()
}
