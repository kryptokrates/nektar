// MCP server on the official SDK: tools (the JSON-Schema definitions in
// server.ts are the single source, executed by callTool), resources, prompts
// and server instructions — served over stdio or Streamable HTTP.
import { McpServer, ResourceTemplate } from '@modelcontextprotocol/sdk/server/mcp.js'
import { CallToolRequestSchema, ListToolsRequestSchema } from '@modelcontextprotocol/sdk/types.js'
import { z } from 'zod'
import type { NektarMemory } from './memory.js'
import { visibleTools, callTool } from './server.js'

export const INSTRUCTIONS = [
  'nektar is verifiable, encrypted agent memory anchored on BSV.',
  'Before answering a factual question, call `recall` (or use the `grounded-answer` prompt) and answer only from items whose check is verified.',
  'For a verifiable answer, pass your draft claims to `groundedAnswer`: every claim must cite a verified fragment and be quoted or entailed — ungrounded text is stripped and an answer receipt is anchored.',
  'When `groundedAnswer`/`commitReceipt` returns receiptToken, end the answer with a one-line footer `receipt: <receiptToken>` and the `verify:` URL — anyone can scan it at /v1/verify to check the answer was committed and derivable.',
  'When the user states a fact worth keeping, call `remember` with its source.',
  'Call `modelManifest` once per session to anchor which model you are; receipts then carry that declared identity.',
  'Cite the shard/seq and anchor txid you relied on; `prove` returns the proof.',
  'Proofs establish that something was RECORDED and has not been altered — never that it is true. Say so when it matters.'
].join(' ')

const READ_ONLY = new Set([
  'recall',
  'prove',
  'verifyMembership',
  'proveMembership',
  'proveSubset',
  'verifySubset',
  'verifyDerivation',
  'verifyAttestation',
  'verifyReceipt'
])
const DESTRUCTIVE = new Set(['rotate', 'revoke'])

const json = (v: unknown) => JSON.stringify(v, (_k, x) => (typeof x === 'bigint' ? x.toString() : x instanceof Uint8Array ? Buffer.from(x).toString('hex') : x))

export function createMcpServer(mem: NektarMemory, ops?: import('./server.js').ToolCtx['ops']): McpServer {
  const server = new McpServer({ name: 'nektar', version: '0.2.0' }, { instructions: INSTRUCTIONS, capabilities: { tools: {}, resources: {}, prompts: {} } })
  const low = server.server
  low.setRequestHandler(ListToolsRequestSchema, async () => ({
    tools: visibleTools().map((t) => ({
      ...t,
      annotations: { readOnlyHint: READ_ONLY.has(t.name), destructiveHint: DESTRUCTIVE.has(t.name), idempotentHint: t.name === 'prove', openWorldHint: false }
    }))
  }))
  low.setRequestHandler(CallToolRequestSchema, async (req) => {
    try {
      if (!visibleTools().some((t) => t.name === req.params.name)) throw new Error(`unknown tool ${req.params.name}`)
      const result = await callTool(mem, req.params.name, (req.params.arguments ?? {}) as Record<string, any>, { ops })
      const text = json(result ?? null)
      const parsed = JSON.parse(text) as unknown
      // structuredContent must be a JSON object (spec); null/arrays go as text only
      const structured = parsed !== null && typeof parsed === 'object' && !Array.isArray(parsed) ? { structuredContent: parsed as Record<string, unknown> } : {}
      return { content: [{ type: 'text' as const, text }], ...structured }
    } catch (e) {
      return { isError: true, content: [{ type: 'text' as const, text: (e as Error).message }] }
    }
  })

  server.registerResource(
    'record',
    new ResourceTemplate('nektar://record/{shard}/{seq}', { list: undefined }),
    { title: 'Record proof status', description: 'Verification result and anchor of one record (no plaintext)', mimeType: 'application/json' },
    async (uri, { shard, seq }) => {
      const s = String(shard)
      const n = Number(seq)
      const rec = (await mem.records(s)).find((r) => r.seq === n)
      if (!rec) throw new Error(`no record ${s}/${n}`)
      const pr = await mem.prove(s, n)
      return {
        contents: [
          {
            uri: uri.href,
            mimeType: 'application/json',
            text: json({
              shard: s,
              seq: n,
              state: rec.state,
              level: rec.level ?? 0,
              anchorTxid: rec.anchorTxid ?? null,
              proof: pr.result,
              status: await mem.anchorStatus(s, n)
            })
          }
        ]
      }
    }
  )
  server.registerResource(
    'checkpoint',
    new ResourceTemplate('nektar://checkpoint/{shard}', { list: undefined }),
    { title: 'Latest signed checkpoint', description: 'LOG-007 checkpoint + log public key', mimeType: 'application/json' },
    async (uri, { shard }) => {
      const cp = mem.checkpoints(String(shard)).at(-1)
      if (!cp) throw new Error(`no checkpoint for shard ${String(shard)}`)
      return { contents: [{ uri: uri.href, mimeType: 'application/json', text: json({ checkpoint: cp, logPublicKey: mem.logPublicKey() }) }] }
    }
  )

  server.registerPrompt(
    'grounded-answer',
    {
      title: 'Answer from verified memory',
      description: 'Recall, answer only from verified items, cite them',
      argsSchema: { question: z.string(), shard: z.string().optional() }
    },
    ({ question, shard }) => ({
      messages: [
        {
          role: 'user' as const,
          content: {
            type: 'text' as const,
            text: `Question: ${question}\n\n1. Call \`recall\`${shard ? ` on shard "${shard}"` : ' on the relevant shard(s)'}.\n2. Answer ONLY from items whose check is verified; quote them.\n3. Cite shard/seq and anchor txid for every claim.\n4. Mark anything not supported by a verified item as "not in verified memory".\n5. Remember: verified means recorded and unaltered, not true.`
          }
        }
      ]
    })
  )
  server.registerPrompt(
    'remember-with-source',
    { title: 'Store a fact with provenance', argsSchema: { fact: z.string(), source: z.string(), shard: z.string().optional() } },
    ({ fact, source, shard }) => ({
      messages: [
        {
          role: 'user' as const,
          content: {
            type: 'text' as const,
            text: `Call \`remember\` with shard "${shard ?? 'notes'}", data ${JSON.stringify(fact)} and source ${JSON.stringify(source)}. Then report the returned seq and inclusion promise.`
          }
        }
      ]
    })
  )
  server.registerPrompt('audit-answer', { title: 'Audit an answer', argsSchema: { answer: z.string() } }, ({ answer }) => ({
    messages: [
      {
        role: 'user' as const,
        content: {
          type: 'text' as const,
          text: `Audit this answer:\n${answer}\n\nFor every cited shard/seq call \`prove\`; report each result. Flag claims with no citation or a failed proof. Do not judge truth — only whether each claim is backed by a verified record.`
        }
      }
    ]
  }))
  return server
}

/** MCP over stdio (official SDK) — the entry bin.ts uses. Lives here (not
 *  server.ts) so the tool layer never imports the transport layer. */
export async function serveStdio(mem: import('./memory.js').NektarMemory, ops?: import('./server.js').ToolCtx['ops']): Promise<void> {
  const { StdioServerTransport } = await import('@modelcontextprotocol/sdk/server/stdio.js')
  await createMcpServer(mem, ops).connect(new StdioServerTransport())
}
