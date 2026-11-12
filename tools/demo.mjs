#!/usr/bin/env node
// tools/demo.mjs — the magic moment in <60s: a real verified AI answer on the
// local Teranode regtest stack. Boots the instance on NEKTAR_NETWORK=regtest,
// seeds public memory, anchors + mines a real block for the BUMP, then calls
// groundedAnswer and prints the receipt footer + badge.
import { spawn } from 'node:child_process'
import { mkdtempSync, rmSync, existsSync, readFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const PORT = Number(process.env.NEKTAR_DEMO_PORT ?? 18090)
const base = `http://127.0.0.1:${PORT}`
const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const dir = mkdtempSync(join(tmpdir(), 'nektar-demo-'))

// repo .env carries NEKTAR_RPC_*/NEKTAR_REGTEST_WIF — merge it under the real env
for (const file of [join(root, '.env'), join(root, 'infra/teranode-quickstart/.env')]) {
  if (!existsSync(file)) continue
  for (const line of readFileSync(file, 'utf8').split(/\r?\n/)) {
    const m = /^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)\s*$/.exec(line)
    if (m !== null && process.env[m[1]] === undefined) process.env[m[1]] = m[2].replace(/^["']|["']$/g, '')
  }
}
const rpc = {
  url: process.env.NEKTAR_RPC_URL ?? 'http://localhost:9292/',
  user: process.env.NEKTAR_RPC_USER ?? 'teranode',
  pass: process.env.NEKTAR_RPC_PASS
}
if (rpc.pass === undefined || rpc.pass === '') {
  console.error('demo needs the local Teranode regtest stack (NEKTAR_RPC_PASS unset). Run pnpm stack:up first.')
  process.exit(1)
}
const mine = async (n = 1) => {
  const r = await fetch(rpc.url, {
    method: 'POST',
    headers: { 'content-type': 'application/json', Authorization: 'Basic ' + Buffer.from(`${rpc.user}:${rpc.pass}`).toString('base64') },
    body: JSON.stringify({ jsonrpc: '1.0', id: 1, method: 'generatetoaddress', params: [n, 'mrCD89nMtdkezoVJtDkkoVbXeHv9vV5Yrg'] }),
    signal: AbortSignal.timeout(15_000)
  })
  const j = await r.json()
  if (j.error) throw new Error(`mine: ${JSON.stringify(j.error)}`)
}

const post = async (tool, args) => {
  const r = await fetch(`${base}/v1/tools/${tool}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(args),
    signal: AbortSignal.timeout(20_000)
  })
  const j = await r.json().catch(() => ({}))
  if (!r.ok) throw new Error(`${tool}: ${j.error ?? r.status}`)
  return j
}

console.log('nektar demo — regtest stack (real node, real BUMPs), regtest coins only\n')
const child = spawn(process.execPath, [join(root, 'packages/mcp/dist/src/bin.js')], {
  env: { ...process.env, NEKTAR_NETWORK: 'regtest', NEKTAR_HTTP_PORT: String(PORT), NEKTAR_DATA_DIR: dir },
  stdio: ['ignore', 'pipe', 'inherit']
})
child.stdout.resume() // keep the child's logs out of the demo output
try {
  for (let i = 0; i < 80; i++) {
    try {
      if ((await fetch(`${base}/v1/health`, { signal: AbortSignal.timeout(500) })).ok) break
    } catch {}
    await new Promise((r) => setTimeout(r, 250))
    if (i === 79) throw new Error('instance did not come up — is the build current? (pnpm -r build)')
  }
  await post('remember', { shard: 'guide', data: 'nektar anchors encrypted, provenance-signed agent memory on BSV' })
  await post('remember', { shard: 'guide', data: 'every grounded answer commits a receipt anyone can verify' })
  await post('anchor', { shard: 'guide' })
  await mine(1) // seal the anchor so its BUMP exists
  await new Promise((r) => setTimeout(r, 500)) // confirmer attach tick

  const r = await post('groundedAnswer', {
    claims: [
      { text: 'nektar anchors encrypted, provenance-signed agent memory on BSV', cites: [{ shard: 'guide', seq: 0 }] },
      { text: 'and it reads minds', cites: [] } // ungrounded — watch it get stripped
    ]
  })

  console.log('  groundedAnswer →')
  console.log('    committed answer :', JSON.stringify(r.answer))
  console.log('    stripped (ungrounded):', (r.flagged ?? []).map((f) => f.text ?? f.reason).join(' | ') || 'none')
  console.log()
  console.log('  share the receipt anywhere:')
  console.log('   ', r.share?.markdown ?? '(share block missing)')
  console.log()
  console.log(`    receipt: ${r.share?.token ?? r.receiptToken}`)
  console.log(`    verify:  ${r.verifyUrl}`)
  console.log()
  console.log('  next: pnpm setup:claude (or setup:codex) wires the hooks;')
  console.log('        exportShard "guide" → packUrl hands this memory to another operator.')
  console.log('        proofs show committed + anchored + derivable — never "true".\n')
} finally {
  child.kill('SIGTERM')
  rmSync(dir, { recursive: true, force: true })
}
