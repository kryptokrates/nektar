// nektar chat: a local model with verifiable memory.
//   node packages/mcp/demo/chat.mjs "What torque does drain port 4 need?"
// Model: any OpenAI-compatible endpoint. Default = Docker Model Runner
// (NEKTAR_LLM_URL=http://localhost:12434/engines/v1, NEKTAR_LLM_MODEL=ai/qwen3);
// Ollama: http://localhost:11434/v1, LM Studio: http://localhost:1234/v1.
// The nektar MCP server (official SDK, stdio) supplies tools + instructions;
// set NEKTAR_NETWORK=regtest etc. to use a persistent backend.
import { fileURLToPath } from 'node:url'
import { Client } from '@modelcontextprotocol/sdk/client/index.js'
import { StdioClientTransport } from '@modelcontextprotocol/sdk/client/stdio.js'

// NEKTAR_LLM_PROVIDER=openai → api.openai.com with OPENAI_API_KEY (env or the
// gitignored nektar/.env — read here so the key never appears on a command line)
import { readFileSync, existsSync } from 'node:fs'
const dotenv = (() => {
  const p = fileURLToPath(new URL('../../../.env', import.meta.url))
  return existsSync(p)
    ? Object.fromEntries(
        readFileSync(p, 'utf8')
          .split(/\r?\n/)
          .map((l) => l.match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/))
          .filter(Boolean)
          .map((m) => [m[1], m[2].trim()])
      )
    : {}
})()
const OPENAI = process.env.NEKTAR_LLM_PROVIDER === 'openai'
const LLM = process.env.NEKTAR_LLM_URL ?? (OPENAI ? 'https://api.openai.com/v1' : 'http://localhost:12434/engines/v1')
const MODEL = process.env.NEKTAR_LLM_MODEL ?? (OPENAI ? 'gpt-4.1-mini' : 'ai/qwen3')
const KEY = process.env.NEKTAR_LLM_KEY ?? (OPENAI ? (process.env.OPENAI_API_KEY ?? dotenv.OPENAI_API_KEY) : undefined)
if (OPENAI && !KEY) throw new Error('NEKTAR_LLM_PROVIDER=openai: set OPENAI_API_KEY (env or nektar/.env)')
const question =
  process.argv.slice(2).join(' ') ||
  'Remember on shard "manual" that drain port 4 needs 45Nm (source: manual rev1). Then tell me the torque for port 4, citing the record.'

// NEKTAR_CHAT_CLEARANCE=<level>: the HARNESS holds a wallet + a demo clearance
// authority (trusted by the spawned server) and transparently attaches identity
// + a Bulletproof clearance to recall — the model never sees keys or proofs.
const { ClientWallet, ClearanceAuthority } = await import('../dist/src/index.js')
const CLEAR = process.env.NEKTAR_CHAT_CLEARANCE ? BigInt(process.env.NEKTAR_CHAT_CLEARANCE) : undefined
const authority = CLEAR !== undefined ? new ClearanceAuthority() : undefined
const wallet = CLEAR !== undefined ? new ClientWallet() : undefined
const cred = CLEAR !== undefined ? authority.issue(wallet.pubkey, CLEAR) : undefined

const bin = fileURLToPath(new URL('../dist/src/bin.js', import.meta.url))
const client = new Client({ name: 'nektar-chat', version: '1.0.0' })
await client.connect(
  new StdioClientTransport({
    command: process.execPath,
    args: [bin],
    env: { ...process.env, NEKTAR_MODEL_ID: `${MODEL}@${new URL(LLM).host}`, ...(authority ? { NEKTAR_CLEARANCE_AUTHORITIES: authority.pubkey } : {}) },
    stderr: 'inherit'
  })
)
const rawCall = async (name, args) => {
  const r = await client.callTool({ name, arguments: args }, undefined, { timeout: 180_000 })
  return r.isError ? { error: r.content[0].text } : JSON.parse(r.content[0].text)
}
/** recall with harness clearance: retry withheld level>0 items one by one with identity + proof */
async function callWithClearance(name, args) {
  const out = await rawCall(name, args)
  if (name !== 'recall' || !wallet || out.error || !out.items) return out
  for (const [i, it] of out.items.entries()) {
    if (!/zkaccess required/.test(it.reason ?? '')) continue
    const r = await rawCall('recall', {
      shard: args.shard,
      seq: it.seq,
      identity: await wallet.identify(rawCall),
      zk: wallet.presentClearance(cred, args.shard, it.seq, CLEAR)
    })
    if (r.items?.[0]?.text !== undefined) {
      out.items[i] = { ...r.items[0], clearedBy: 'harness wallet (ZK clearance)' }
      console.log(`  ↳ harness: ZK clearance (level ≥ ${CLEAR}) attached for ${args.shard}/${it.seq} → ${r.items[0].zk}`)
    }
  }
  return out
}
// small local models do better with few tools: default to the core memory set
const wanted = new Set((process.env.NEKTAR_CHAT_TOOLS ?? 'remember,recall,prove,anchor').split(','))
const tools = (await client.listTools()).tools.filter((t) => wanted.has('all') || wanted.has(t.name))
const fnTools = tools.map((t) => ({ type: 'function', function: { name: t.name, description: t.description, parameters: t.inputSchema } }))
const messages = [
  { role: 'system', content: `${client.getInstructions()}\nUse the tools; never invent records. /no_think` },
  { role: 'user', content: question }
]
const known = new Set(tools.map((t) => t.name))
// streamed completion: headers arrive at once (slow CPU models no longer hit
// fetch's header timeout); content + tool_call deltas are assembled here
async function complete() {
  const res = await fetch(`${LLM}/chat/completions`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', ...(KEY ? { authorization: `Bearer ${KEY}` } : {}) },
    body: JSON.stringify({
      model: MODEL,
      messages,
      tools: fnTools,
      temperature: 0,
      stream: true,
      ...(OPENAI
        ? { max_completion_tokens: Number(process.env.NEKTAR_CHAT_MAX_TOKENS ?? 800) }
        : { max_tokens: Number(process.env.NEKTAR_CHAT_MAX_TOKENS ?? 512) })
    }),
    signal: AbortSignal.timeout(Number(process.env.NEKTAR_CHAT_TIMEOUT_MS ?? 900_000))
  })
  if (!res.ok) throw new Error(`model endpoint ${LLM}: HTTP ${res.status} ${(await res.text()).slice(0, 300)}`)
  const msg = { role: 'assistant', content: '', tool_calls: [] }
  let buf = ''
  const dec = new TextDecoder()
  for await (const chunk of res.body) {
    buf += dec.decode(chunk, { stream: true })
    let i
    while ((i = buf.indexOf('\n')) >= 0) {
      const line = buf.slice(0, i).trim()
      buf = buf.slice(i + 1)
      if (!line.startsWith('data:') || line === 'data: [DONE]') continue
      const d = JSON.parse(line.slice(5)).choices?.[0]?.delta ?? {}
      if (d.content) {
        msg.content += d.content
        process.stderr.write('.')
      }
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
  if (!msg.tool_calls?.length) {
    console.log(
      `\n${MODEL}: ${String(msg.content ?? '')
        .replace(/<think>[\s\S]*?<\/think>/g, '')
        .trim()}`
    )
    break
  }
  for (const tc of msg.tool_calls) {
    let out
    if (!known.has(tc.function.name))
      out = { error: `unknown tool ${tc.function.name}` } // small models hallucinate tool names
    else {
      let args = {}
      try {
        args = JSON.parse(tc.function.arguments || '{}')
      } catch {
        out = { error: 'arguments were not valid JSON' }
      }
      if (!out) out = (await callWithClearance(tc.function.name, args)) ?? { result: null, note: 'nothing pending (batch already anchored)' }
    }
    console.log(`→ ${tc.function.name}(${tc.function.arguments}) ${out.error ? `✖ ${out.error}` : '✔'}`)
    messages.push({ role: 'tool', tool_call_id: tc.id, content: JSON.stringify(out) })
  }
}
await client.close()
