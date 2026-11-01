// Scripted MCP client demo: spawns the mamem stdio server and drives it the
// way an agent would. With MAMEM_LLM_URL set (any OpenAI-compatible endpoint,
// e.g. a local Qwen via llama.cpp/vLLM/Ollama at http://localhost:11434/v1),
// a model picks the tool calls instead of the script (MAMEM_LLM_MODEL names it).
import { spawn } from 'node:child_process'
import { createInterface } from 'node:readline'
import { fileURLToPath } from 'node:url'

const bin = fileURLToPath(new URL('../dist/src/bin.js', import.meta.url))
const srv = spawn(process.execPath, [bin], { stdio: ['pipe', 'pipe', 'inherit'], env: { ...process.env, MAMEM_MODEL_ID: process.env.MAMEM_LLM_MODEL ?? 'scripted-demo' } })
const pending = new Map()
createInterface({ input: srv.stdout }).on('line', l => { const m = JSON.parse(l); pending.get(m.id)?.(m); pending.delete(m.id) })
let id = 0
const rpc = (method, params) => new Promise((resolve, reject) => {
  const n = ++id
  const t = setTimeout(() => reject(new Error(`${method} timed out`)), 15_000)
  pending.set(n, m => { clearTimeout(t); m.error ? reject(new Error(m.error.message)) : resolve(m.result) })
  srv.stdin.write(JSON.stringify({ jsonrpc: '2.0', id: n, method, params }) + '\n')
})
const call = async (name, args) => JSON.parse((await rpc('tools/call', { name, arguments: args })).content[0].text)

await rpc('initialize', {})
const { tools } = await rpc('tools/list', {})
console.log(`tools: ${tools.map(t => t.name).join(', ')}`)

if (process.env.MAMEM_LLM_URL) {
  // model-driven loop (≤ 6 tool rounds)
  const messages = [
    { role: 'system', content: 'You are an agent with verifiable memory. Store the facts, anchor them, then prove one and answer citing it.' },
    { role: 'user', content: 'Remember on shard "manual": "drain port 4 torque 45Nm" and "pump P2 inspected 2026-10-30". Then anchor, prove seq 0, and tell me the torque.' }
  ]
  const fnTools = tools.map(t => ({ type: 'function', function: { name: t.name, description: t.description, parameters: t.inputSchema } }))
  for (let round = 0; round < 6; round++) {
    const res = await fetch(`${process.env.MAMEM_LLM_URL}/chat/completions`, {
      method: 'POST', headers: { 'content-type': 'application/json', ...(process.env.MAMEM_LLM_KEY ? { authorization: `Bearer ${process.env.MAMEM_LLM_KEY}` } : {}) },
      body: JSON.stringify({ model: process.env.MAMEM_LLM_MODEL ?? 'qwen', messages, tools: fnTools }),
      signal: AbortSignal.timeout(60_000)
    })
    if (!res.ok) throw new Error(`LLM HTTP ${res.status}: ${await res.text()}`)
    const msg = (await res.json()).choices[0].message
    messages.push(msg)
    if (!msg.tool_calls?.length) { console.log(`model: ${msg.content}`); break }
    for (const tc of msg.tool_calls) {
      const out = await call(tc.function.name, JSON.parse(tc.function.arguments || '{}')).catch(e => ({ error: e.message }))
      console.log(`→ ${tc.function.name} ${tc.function.arguments}`)
      messages.push({ role: 'tool', tool_call_id: tc.id, content: JSON.stringify(out) })
    }
  }
} else {
  await call('remember', { shard: 'manual', data: 'drain port 4 torque 45Nm', source: 'manual-rev1' })
  await call('remember', { shard: 'manual', data: 'pump P2 inspected 2026-10-30', source: 'log' })
  const a = await call('anchor', { shard: 'manual' })
  console.log(`anchor txid=${a?.txid ?? '(batch already closed)'}`)
  const p = await call('prove', { shard: 'manual', seq: 0 })
  console.log(`prove seq0: ${JSON.stringify(p.result)}`)
  const r = await call('recall', { shard: 'manual', seq: 0 })
  console.log(`recall: "${r.items?.[0]?.text ?? ''}" check=${r.receipt?.checkResult} model=${r.receipt?.modelId}`)
  console.log(`truth note: ${r.receipt?.truthNote}`)
  const d = await call('automaton', { op: 'deploy', id: 'doc', def: { Q: ['draft', 'published'], Sigma: ['publish'], delta: [[0, 0, 1]], q0: 0, F: [1], O: [] } })
  const s = await call('automaton', { op: 'step', id: 'doc', sym: 0, to: 1, outHash: 'ab'.repeat(32) })
  console.log(`automaton: deploy ${d.txid.slice(0, 12)}… → step q=${s.q} (${s.statement})`)
}
srv.kill()
