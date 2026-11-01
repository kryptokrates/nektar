// mamem chat: a local model with verifiable memory.
//   node packages/mcp/demo/chat.mjs "What torque does drain port 4 need?"
// Model: any OpenAI-compatible endpoint. Default = Docker Model Runner
// (MAMEM_LLM_URL=http://localhost:12434/engines/v1, MAMEM_LLM_MODEL=ai/qwen3);
// Ollama: http://localhost:11434/v1, LM Studio: http://localhost:1234/v1.
// The mamem MCP server (official SDK, stdio) supplies tools + instructions;
// set MAMEM_NETWORK=regtest etc. to use a persistent backend.
import { fileURLToPath } from 'node:url'
import { Client } from '@modelcontextprotocol/sdk/client/index.js'
import { StdioClientTransport } from '@modelcontextprotocol/sdk/client/stdio.js'

const LLM = process.env.MAMEM_LLM_URL ?? 'http://localhost:12434/engines/v1'
const MODEL = process.env.MAMEM_LLM_MODEL ?? 'ai/qwen3'
const question = process.argv.slice(2).join(' ') || 'Remember on shard "manual" that drain port 4 needs 45Nm (source: manual rev1). Then tell me the torque for port 4, citing the record.'

const bin = fileURLToPath(new URL('../dist/src/bin.js', import.meta.url))
const client = new Client({ name: 'mamem-chat', version: '1.0.0' })
await client.connect(new StdioClientTransport({ command: process.execPath, args: [bin], env: { ...process.env, MAMEM_MODEL_ID: `${MODEL}@${new URL(LLM).host}` }, stderr: 'inherit' }))
// small local models do better with few tools: default to the core memory set
const wanted = new Set((process.env.MAMEM_CHAT_TOOLS ?? 'remember,recall,prove,anchor').split(','))
const tools = (await client.listTools()).tools.filter(t => wanted.has('all') || wanted.has(t.name))
const fnTools = tools.map(t => ({ type: 'function', function: { name: t.name, description: t.description, parameters: t.inputSchema } }))
const messages = [
  { role: 'system', content: `${client.getInstructions()}\nUse the tools; never invent records. /no_think` },
  { role: 'user', content: question }
]
const known = new Set(tools.map(t => t.name))
// streamed completion: headers arrive at once (slow CPU models no longer hit
// fetch's header timeout); content + tool_call deltas are assembled here
async function complete () {
  const res = await fetch(`${LLM}/chat/completions`, {
    method: 'POST', headers: { 'content-type': 'application/json', ...(process.env.MAMEM_LLM_KEY ? { authorization: `Bearer ${process.env.MAMEM_LLM_KEY}` } : {}) },
    body: JSON.stringify({ model: MODEL, messages, tools: fnTools, temperature: 0, stream: true, max_tokens: Number(process.env.MAMEM_CHAT_MAX_TOKENS ?? 512) }),
    signal: AbortSignal.timeout(Number(process.env.MAMEM_CHAT_TIMEOUT_MS ?? 900_000))
  })
  if (!res.ok) throw new Error(`model endpoint ${LLM}: HTTP ${res.status} ${await res.text()}`)
  const msg = { role: 'assistant', content: '', tool_calls: [] }
  let buf = ''
  const dec = new TextDecoder()
  for await (const chunk of res.body) {
    buf += dec.decode(chunk, { stream: true })
    let i
    while ((i = buf.indexOf('\n')) >= 0) {
      const line = buf.slice(0, i).trim(); buf = buf.slice(i + 1)
      if (!line.startsWith('data:') || line === 'data: [DONE]') continue
      const d = JSON.parse(line.slice(5)).choices?.[0]?.delta ?? {}
      if (d.content) { msg.content += d.content; process.stderr.write('.') }
      for (const tc of d.tool_calls ?? []) {
        const t = (msg.tool_calls[tc.index ?? 0] ??= { id: tc.id ?? `call_${tc.index ?? 0}`, type: 'function', function: { name: '', arguments: '' } })
        if (tc.id) t.id = tc.id
        if (tc.function?.name) t.function.name += tc.function.name
        if (tc.function?.arguments) t.function.arguments += tc.function.arguments
      }
    }
  }
  if (!msg.tool_calls.length) delete msg.tool_calls
  return msg
}
for (let round = 0; round < 8; round++) {
  const msg = await complete()
  messages.push(msg)
  if (!msg.tool_calls?.length) { console.log(`\n${MODEL}: ${String(msg.content ?? '').replace(/<think>[\s\S]*?<\/think>/g, '').trim()}`); break }
  for (const tc of msg.tool_calls) {
    let out
    if (!known.has(tc.function.name)) out = { error: `unknown tool ${tc.function.name}` } // small models hallucinate tool names
    else {
      let args = {}
      try { args = JSON.parse(tc.function.arguments || '{}') } catch { out = { error: 'arguments were not valid JSON' } }
      if (!out) { const r = await client.callTool({ name: tc.function.name, arguments: args }); out = r.isError ? { error: r.content[0].text } : JSON.parse(r.content[0].text) }
    }
    console.log(`→ ${tc.function.name}(${tc.function.arguments}) ${out.error ? `✖ ${out.error}` : '✔'}`)
    messages.push({ role: 'tool', tool_call_id: tc.id, content: JSON.stringify(out) })
  }
}
await client.close()
