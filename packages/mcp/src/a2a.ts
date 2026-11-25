// a2a.ts — A2A-compliant Agent Card (/.well-known/agent.json) + lightweight
// JSON-RPC task endpoint (POST /a2a) so A2A multi-agent orchestrators discover
// and invoke nektar memory/market skills while preserving every legacy field.
import { randomUUID } from 'node:crypto'
import type { IncomingMessage, ServerResponse } from 'node:http'
import { nektarEnv, type EnvSource } from '@nektar/core'
import { marketHosts } from './hosts.js'
import type { NektarMemory } from './memory.js'
import { safeHost } from './pages.js'
import { callTool, visibleTools, type ToolCtx } from './server.js'

export function buildAgentCard(mem: NektarMemory, req: IncomingMessage, env: EnvSource = process.env): Record<string, unknown> {
  const explicit = nektarEnv('NEKTAR_VERIFY_BASE', env)
  const proto = req.headers['x-forwarded-proto'] === 'https' ? 'https' : 'http'
  const base = (explicit ?? `${proto}://${safeHost(req.headers.host)}`).replace(/\/+$/, '')
  const authRequired = nektarEnv('NEKTAR_HTTP_TOKEN', env) !== undefined
  const marketOn = mem.automation != null

  const skills: Array<{ id: string; name: string; description: string; tags: string[] }> = [
    {
      id: 'recall',
      name: 'Verified Memory Recall',
      description: 'Search and retrieve verifiable, provenance-signed memory fragments across readable shards.',
      tags: ['memory', 'recall', 'verified']
    },
    {
      id: 'remember',
      name: 'Remember with Provenance',
      description: 'Persist durable facts into an append-only, BSV-anchored memory shard.',
      tags: ['memory', 'write', 'provenance']
    },
    {
      id: 'groundedAnswer',
      name: 'Grounded Answer + Receipt',
      description: 'Verify claims against cited fragments and issue a checkable nektarr1.* receipt.',
      tags: ['grounding', 'receipts', 'verification']
    },
    {
      id: 'verifyReceipt',
      name: 'Receipt Verification',
      description: 'Verify an existing nektarr1.* receipt token against log and anchor proofs.',
      tags: ['verification', 'audit']
    }
  ]
  if (marketOn) {
    skills.push({
      id: 'market',
      name: 'Knowledge Market (Want / Sell / Deals)',
      description: 'Buy or sell verifiable knowledge shards under operator-approved spending policies.',
      tags: ['market', 'bsv', 'escrow']
    })
  }

  return {
    name: 'nektar',
    version: '0.3.2',
    protocolVersion: '0.2.0',
    description: 'Verifiable, encrypted agent memory anchored on BSV — recall, proofs, L0 LLM proxy, and an autonomous sats marketplace over MCP & A2A',
    url: `${base}/a2a`,
    network: nektarEnv('NEKTAR_NETWORK', env) ?? 'regtest',
    defaultInputModes: ['text/plain', 'application/json'],
    defaultOutputModes: ['text/plain', 'application/json'],
    skills,
    endpoints: {
      mcp: `${base}/mcp`,
      a2a: `${base}/a2a`,
      proxyOpenai: `${base}/v1/proxy/openai/v1`,
      proxyAnthropic: `${base}/v1/proxy/anthropic/v1`,
      tools: `${base}/v1/tools`,
      call: `${base}/v1/tools/{name}`,
      openapi: `${base}/v1/openapi.json`,
      adapters: `${base}/v1/adapters/manifest`,
      oauth: `${base}/.well-known/oauth-authorization-server`,
      health: `${base}/v1/health`,
      discovery: `${base}/.well-known/nektar`
    },
    authentication: {
      schemes: authRequired ? ['Bearer', 'OAuth2'] : ['None'],
      scheme: 'bearer',
      header: 'Authorization',
      required: authRequired
    },
    capabilities: {
      memory: true,
      receipts: true,
      streaming: true,
      llmProxy: true,
      market: marketOn,
      marketHosts: marketHosts(env)
    },
    join: {
      mcp: 'any streamable-HTTP MCP client: url <mcp>, header Authorization: Bearer <token>',
      proxy: 'set OPENAI_BASE_URL=<proxyOpenai> (or ANTHROPIC_BASE_URL=<proxyAnthropic>) for zero-code docking',
      rest: 'GET /v1/tools then POST /v1/tools/{name} with a JSON body',
      a2a: 'POST /a2a JSON-RPC 2.0 tasks/send or message/send',
      doc: 'docs/ADAPTERS.md'
    }
  }
}

export async function handleA2aRoute(
  req: IncomingMessage,
  res: ServerResponse,
  readBody: () => Promise<unknown>,
  mem: NektarMemory,
  ops: ToolCtx['ops'] | undefined,
  authed: boolean
): Promise<void> {
  const send = (code: number, body: unknown) => {
    res.writeHead(code, { 'content-type': 'application/json' })
    res.end(JSON.stringify(body, (_k, x: unknown) => (typeof x === 'bigint' ? x.toString() : x instanceof Uint8Array ? Buffer.from(x).toString('hex') : x)))
  }

  if (req.method !== 'POST') {
    send(405, { jsonrpc: '2.0', id: null, error: { code: -32600, message: 'POST required' } })
    return
  }

  const rpc = (await readBody().catch(() => ({}))) as {
    id?: string | number | null
    method?: string
    params?: {
      id?: string
      message?: { parts?: Array<{ type?: string; text?: string; data?: Record<string, unknown> }> }
    }
  }
  const id = rpc.id ?? null
  if (rpc.method !== 'tasks/send' && rpc.method !== 'message/send') {
    send(200, { jsonrpc: '2.0', id, error: { code: -32601, message: `unsupported A2A method: ${String(rpc.method ?? '')}` } })
    return
  }

  const parts = Array.isArray(rpc.params?.message?.parts) ? rpc.params.message.parts : []
  const dataPart = parts.find((p) => p.type === 'data' && p.data && typeof p.data === 'object')?.data
  const textPart = parts
    .filter((p) => (p.type === 'text' || p.type === undefined) && typeof p.text === 'string')
    .map((p) => p.text ?? '')
    .join('\n')
    .trim()

  let toolName = 'recall'
  let toolArgs: Record<string, unknown> = textPart ? { query: textPart, limit: 5 } : {}
  if (dataPart && typeof dataPart.tool === 'string') {
    toolName = dataPart.tool
    toolArgs = dataPart.args && typeof dataPart.args === 'object' ? (dataPart.args as Record<string, unknown>) : {}
  }

  const allowed = await visibleTools(mem)
  if (!allowed.some((t) => t.name === toolName)) {
    send(200, { jsonrpc: '2.0', id, error: { code: -32602, message: `unknown or disallowed tool: ${toolName}` } })
    return
  }

  try {
    const result = await callTool(mem, toolName, toolArgs, { ops, external: !authed })
    const taskId = typeof rpc.params?.id === 'string' && rpc.params.id ? rpc.params.id : randomUUID()
    send(200, {
      jsonrpc: '2.0',
      id,
      result: {
        id: taskId,
        status: { state: 'completed', timestamp: new Date().toISOString() },
        artifacts: [
          {
            name: toolName,
            parts: [
              { type: 'data', data: result },
              { type: 'text', text: JSON.stringify(result) }
            ]
          }
        ]
      }
    })
  } catch (e) {
    send(200, { jsonrpc: '2.0', id, error: { code: -32000, message: (e as Error).message } })
  }
}
