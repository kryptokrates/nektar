// AGT-002 integration: the real stdio JSON-RPC server, spawned as a process.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { createInterface } from 'node:readline'
import { fileURLToPath } from 'node:url'

test('mcp stdio server: initialize → tools/list → remember/anchor/prove/recall over JSON-RPC', async () => {
  const bin = fileURLToPath(new URL('../src/bin.js', import.meta.url))
  const srv = spawn(process.execPath, [bin], { stdio: ['pipe', 'pipe', 'inherit'], env: { ...process.env, MAMEM_MODEL_ID: 'stdio-test', MAMEM_DEMO_UTXOS: '20' } })
  const pending = new Map<number, (m: any) => void>()
  createInterface({ input: srv.stdout! }).on('line', l => { const m = JSON.parse(l); pending.get(m.id)?.(m); pending.delete(m.id) })
  let id = 0
  const rpc = (method: string, params: unknown) => new Promise<any>((resolve, reject) => {
    const n = ++id
    const t = setTimeout(() => reject(new Error(`${method} timed out`)), 8000)
    pending.set(n, m => { clearTimeout(t); m.error ? reject(new Error(m.error.message)) : resolve(m.result) })
    srv.stdin!.write(JSON.stringify({ jsonrpc: '2.0', id: n, method, params }) + '\n')
  })
  const call = async (name: string, args: unknown) => JSON.parse((await rpc('tools/call', { name, arguments: args })).content[0].text)
  try {
    assert.equal((await rpc('initialize', {})).serverInfo.name, 'mamem')
    const names = (await rpc('tools/list', {})).tools.map((t: { name: string }) => t.name)
    for (const n of ['remember', 'recall', 'prove', 'anchor', 'automaton', 'proveMembership']) assert.ok(names.includes(n), n)
    await call('remember', { shard: 'm', data: 'torque 45Nm' })
    await call('anchor', { shard: 'm' })
    assert.deepEqual((await call('prove', { shard: 'm', seq: 0 })).result, { ok: true })
    const r = await call('recall', { shard: 'm', seq: 0 })
    assert.equal(r.items[0].text, 'torque 45Nm')
    assert.equal(r.receipt.modelId, 'stdio-test')
    await assert.rejects(() => rpc('tools/call', { name: 'nope', arguments: {} }), /unknown tool nope/)
  } finally { srv.kill() }
})
