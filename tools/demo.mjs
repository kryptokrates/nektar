#!/usr/bin/env node
// tools/demo.mjs — the magic moment in <60s: a real verified AI answer,
// offline, zero BSV, zero funding. Boots the instance on the MOCK network
// (anchors auto-mine in-process), seeds public memory, calls groundedAnswer,
// prints the receipt footer + badge — the whole product loop, nothing spent.
import { spawn } from 'node:child_process'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const PORT = Number(process.env.MAMEM_DEMO_PORT ?? 18090)
const base = `http://127.0.0.1:${PORT}`
const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const dir = mkdtempSync(join(tmpdir(), 'mamem-demo-'))

const post = async (tool, args) => {
  const r = await fetch(`${base}/v1/tools/${tool}`, {
    method: 'POST', headers: { 'content-type': 'application/json' },
    body: JSON.stringify(args), signal: AbortSignal.timeout(20_000)
  })
  const j = await r.json().catch(() => ({}))
  if (!r.ok) throw new Error(`${tool}: ${j.error ?? r.status}`)
  return j
}

console.log('mamem demo — mock network (offline, auto-mined), nothing is spent\n')
const child = spawn(process.execPath, [join(root, 'packages/mcp/dist/src/bin.js')], {
  env: { ...process.env, MAMEM_NETWORK: 'mock', MAMEM_HTTP_PORT: String(PORT), MAMEM_DATA_DIR: dir },
  stdio: ['ignore', 'pipe', 'inherit']
})
child.stdout.resume() // keep the child's logs out of the demo output
try {
  for (let i = 0; i < 80; i++) {
    try { if ((await fetch(`${base}/v1/health`, { signal: AbortSignal.timeout(500) })).ok) break } catch {}
    await new Promise(r => setTimeout(r, 250))
    if (i === 79) throw new Error('instance did not come up — is the build current? (pnpm -r build)')
  }
  await post('remember', { shard: 'guide', data: 'mamem anchors encrypted, provenance-signed agent memory on BSV' })
  await post('remember', { shard: 'guide', data: 'every grounded answer commits a receipt anyone can verify' })
  await post('anchor', { shard: 'guide' })
  await new Promise(r => setTimeout(r, 1500)) // mock BUMP attach tick is 1s

  const r = await post('groundedAnswer', { claims: [
    { text: 'mamem anchors encrypted, provenance-signed agent memory on BSV', cites: [{ shard: 'guide', seq: 0 }] },
    { text: 'and it reads minds', cites: [] } // ungrounded — watch it get stripped
  ] })

  console.log('  groundedAnswer →')
  console.log('    committed answer :', JSON.stringify(r.answer))
  console.log('    stripped (ungrounded):', (r.flagged ?? []).map(f => f.text ?? f.reason).join(' | ') || 'none')
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
