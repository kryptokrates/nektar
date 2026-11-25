// proxy.ts — L0 Transparent LLM Memory Proxy (/v1/proxy/openai/* and
// /v1/proxy/anthropic/*). Lets ANY existing running agent (LangGraph, CrewAI,
// AutoGen, n8n, Open WebUI, Aider, custom Python/TS daemons) dock to nektar by
// changing ONE env var (OPENAI_BASE_URL or ANTHROPIC_BASE_URL) with zero code
// changes and zero MCP requirement:
//   1. Pre-turn deterministic verified memory recall (bounded, fail-open)
//   2. Stream-friendly forwarding to the configured upstream (caller API key
//      passes through untouched and is never logged or persisted)
//   3. Post-turn async receipt verification + optional opt-in turn capture
import { timingSafeEqual } from 'node:crypto'
import type { IncomingMessage, ServerResponse } from 'node:http'
import { deadline, mkLog, nektarEnv, nektarEnvFlag, nektarEnvNum, type EnvSource } from '@nektar/core'
import { isLoopbackPeer } from './auth.js'
import type { NektarMemory } from './memory.js'
import { callTool } from './server.js'

const DEFAULT_OPENAI_UPSTREAM = 'https://api.openai.com'
const DEFAULT_ANTHROPIC_UPSTREAM = 'https://api.anthropic.com'
const DEFAULT_RECALL_TIMEOUT_MS = 600
const DEFAULT_MAX_INJECT_CHARS = 1600
const MAX_TEE_CHARS = 64 * 1024
const RECEIPT_RE = /nektarr1\.[A-Za-z0-9_-]{40,}/g

const SKIP_REQ_HEADERS = new Set([
  'host',
  'connection',
  'keep-alive',
  'transfer-encoding',
  'content-length',
  'accept-encoding',
  'cookie',
  'x-nektar-token',
  'x-nektar-shard',
  'x-nektar-capture',
  'x-nektar-recall'
])

const SKIP_RES_HEADERS = new Set(['connection', 'keep-alive', 'transfer-encoding', 'content-length', 'content-encoding'])

const plog = mkLog('proxy')

const safeEq = (a: string, b: string): boolean => {
  const ab = Buffer.from(a)
  const bb = Buffer.from(b)
  return ab.length === bb.length && timingSafeEqual(ab, bb)
}

const norm = (s: string): string => s.toLowerCase().replace(/\s+/g, ' ').trim()

/** Extract nektar auth + clean upstream key from either:
 *  - X-Nektar-Token header (leaving Authorization / x-api-key for upstream), OR
 *  - Composite key `nkt:<nektarToken>:<upstreamKey>` in Authorization / x-api-key
 *    (so black-box agents that only accept OPENAI_API_KEY still authenticate cleanly), OR
 *  - Standard session/bearer auth when NEKTAR_HTTP_TOKEN is unset on loopback. */
function resolveProxyAuth(
  req: IncomingMessage,
  provider: 'openai' | 'anthropic',
  alreadyAuthed: boolean,
  env: EnvSource = process.env
): { ok: boolean; upstreamKeyOverride?: string } {
  const expected = nektarEnv('NEKTAR_HTTP_TOKEN', env)
  const opExpected = nektarEnv('NEKTAR_OPERATOR_TOKEN', env)
  const matchesNektar = (cand: string): boolean => (expected !== undefined && safeEq(cand, expected)) || (opExpected !== undefined && safeEq(cand, opExpected))

  const xTok = typeof req.headers['x-nektar-token'] === 'string' ? req.headers['x-nektar-token'].trim() : ''
  if (xTok && matchesNektar(xTok)) return { ok: true }

  const rawKeyHeader =
    provider === 'anthropic'
      ? typeof req.headers['x-api-key'] === 'string'
        ? req.headers['x-api-key'].trim()
        : ''
      : typeof req.headers.authorization === 'string' && req.headers.authorization.startsWith('Bearer ')
        ? req.headers.authorization.slice(7).trim()
        : ''

  if (rawKeyHeader.startsWith('nkt:')) {
    const parts = rawKeyHeader.split(':')
    if (parts.length >= 3) {
      const nktTok = parts[1]
      const upstreamKey = parts.slice(2).join(':')
      if (matchesNektar(nktTok)) return { ok: true, upstreamKeyOverride: upstreamKey }
      return { ok: false }
    }
  }

  if (alreadyAuthed) return { ok: true }
  if (expected === undefined && isLoopbackPeer(req)) return { ok: true }
  return { ok: false }
}

function extractTextFromContent(content: unknown): string {
  if (typeof content === 'string') return content
  if (Array.isArray(content)) {
    return content
      .map((part) => {
        if (typeof part === 'string') return part
        if (part !== null && typeof part === 'object' && 'text' in part && typeof (part as { text?: unknown }).text === 'string') {
          return (part as { text: string }).text
        }
        return ''
      })
      .filter(Boolean)
      .join('\n')
  }
  return ''
}

function extractUserPrompt(provider: 'openai' | 'anthropic', path: string, body: Record<string, unknown>): string {
  if (provider === 'openai' && path.endsWith('/responses')) {
    if (typeof body.input === 'string') return body.input
    if (Array.isArray(body.input)) {
      for (let i = body.input.length - 1; i >= 0; i--) {
        const m = body.input[i] as { role?: string; content?: unknown } | undefined
        if (m?.role === 'user') return extractTextFromContent(m.content)
      }
    }
    return ''
  }
  const msgs = Array.isArray(body.messages) ? (body.messages as Array<{ role?: string; content?: unknown }>) : []
  for (let i = msgs.length - 1; i >= 0; i--) {
    if (msgs[i]?.role === 'user') return extractTextFromContent(msgs[i].content)
  }
  return ''
}

function injectMemoryBlock(provider: 'openai' | 'anthropic', path: string, body: Record<string, unknown>, block: string): Record<string, unknown> {
  const clone: Record<string, unknown> = { ...body }
  if (provider === 'openai' && path.endsWith('/responses')) {
    const prev = typeof clone.instructions === 'string' ? clone.instructions : ''
    clone.instructions = prev ? `${prev}\n\n${block}` : block
    return clone
  }
  if (provider === 'anthropic') {
    if (typeof clone.system === 'string' && clone.system.trim() !== '') {
      clone.system = `${clone.system}\n\n${block}`
    } else if (Array.isArray(clone.system)) {
      clone.system = [...clone.system, { type: 'text', text: block }]
    } else {
      clone.system = block
    }
    return clone
  }
  const msgs = Array.isArray(clone.messages) ? [...(clone.messages as Array<Record<string, unknown>>)] : []
  const sysIdx = msgs.findIndex((m) => m.role === 'system' || m.role === 'developer')
  if (sysIdx >= 0) {
    const cur = msgs[sysIdx]
    const prev = extractTextFromContent(cur.content)
    msgs[sysIdx] = { ...cur, content: prev ? `${prev}\n\n${block}` : block }
  } else {
    msgs.unshift({ role: 'system', content: block })
  }
  clone.messages = msgs
  return clone
}

function formatRecallBlock(
  items: Array<{ shard?: string; seq?: number; text?: string; check?: { verified?: boolean } }>,
  maxChars = DEFAULT_MAX_INJECT_CHARS
): { block: string; count: number } {
  const verified = items.filter((it) => typeof it.text === 'string' && it.text.trim() !== '' && it.check?.verified !== false)
  if (verified.length === 0) return { block: '', count: 0 }
  const header = '[nektar verified memory — cite [shard:seq] when relevant; recorded & unaltered ≠ true]'
  const lines = [header]
  let used = header.length
  let count = 0
  for (const it of verified) {
    const snippet = (it.text ?? '').replace(/\s+/g, ' ').trim().slice(0, 280)
    const line = `- [${it.shard ?? '?'}:${it.seq ?? 0}] ${snippet}`
    if (used + line.length + 1 > maxChars) break
    lines.push(line)
    used += line.length + 1
    count++
  }
  return count > 0 ? { block: lines.join('\n'), count } : { block: '', count: 0 }
}

function extractAssistantTextFromResponse(provider: 'openai' | 'anthropic', rawText: string, isStream: boolean): string {
  if (!isStream) {
    try {
      const j = JSON.parse(rawText) as Record<string, unknown>
      if (provider === 'anthropic') return extractTextFromContent(j.content)
      if (typeof j.output_text === 'string') return j.output_text
      const choices = Array.isArray(j.choices) ? (j.choices as Array<{ message?: { content?: unknown } }>) : []
      return extractTextFromContent(choices[0]?.message?.content)
    } catch {
      return ''
    }
  }
  const chunks: string[] = []
  for (const line of rawText.split(/\r?\n/)) {
    if (!line.startsWith('data:')) continue
    const payload = line.slice(5).trim()
    if (!payload || payload === '[DONE]') continue
    try {
      const ev = JSON.parse(payload) as Record<string, unknown>
      if (provider === 'anthropic') {
        const delta = ev.delta as { text?: unknown } | undefined
        if (typeof delta?.text === 'string') chunks.push(delta.text)
      } else {
        const choices = Array.isArray(ev.choices) ? (ev.choices as Array<{ delta?: { content?: unknown } }>) : []
        if (typeof choices[0]?.delta?.content === 'string') chunks.push(choices[0].delta.content)
        else if (typeof ev.delta === 'string') chunks.push(ev.delta)
      }
    } catch {
      /* partial SSE event */
    }
  }
  return chunks.join('')
}

function sanitizeUpstreamPath(rawSuffix: string): string {
  const clean = rawSuffix.startsWith('/') ? rawSuffix : `/${rawSuffix}`
  if (clean.includes('..') || clean.includes('//') || clean.includes('@')) throw new Error('invalid proxy path')
  return clean
}

export interface ProxyContext {
  mem: NektarMemory
  alreadyAuthed: boolean
  env?: EnvSource
  fetchImpl?: typeof fetch
}

export async function handleProxyRoute(
  req: IncomingMessage,
  res: ServerResponse,
  url: URL,
  readBody: () => Promise<unknown>,
  ctx: ProxyContext
): Promise<boolean> {
  if (!url.pathname.startsWith('/v1/proxy/')) return false
  const env = ctx.env ?? process.env
  const fetchImpl = ctx.fetchImpl ?? fetch
  const send = (code: number, body: unknown) => {
    res.writeHead(code, { 'content-type': 'application/json' })
    res.end(JSON.stringify(body))
  }

  const openaiUpstream = (nektarEnv('NEKTAR_PROXY_OPENAI_UPSTREAM', env) ?? DEFAULT_OPENAI_UPSTREAM).replace(/\/+$/, '')
  const anthropicUpstream = (nektarEnv('NEKTAR_PROXY_ANTHROPIC_UPSTREAM', env) ?? DEFAULT_ANTHROPIC_UPSTREAM).replace(/\/+$/, '')
  const recallTimeoutMs = nektarEnvNum('NEKTAR_PROXY_RECALL_TIMEOUT_MS', env) ?? DEFAULT_RECALL_TIMEOUT_MS

  if (req.method === 'GET' && url.pathname === '/v1/proxy/health') {
    const auth = resolveProxyAuth(req, 'openai', ctx.alreadyAuthed, env)
    if (!auth.ok) {
      send(401, { error: 'unauthorized' })
      return true
    }
    send(200, {
      ok: true,
      openaiUpstream,
      anthropicUpstream,
      recallTimeoutMs,
      defaultCapture: nektarEnvFlag('NEKTAR_PROXY_CAPTURE', env) === true
    })
    return true
  }

  const m = /^\/v1\/proxy\/(openai|anthropic)(\/.*)?$/.exec(url.pathname)
  if (!m) {
    send(404, { error: 'unknown proxy provider (use /v1/proxy/openai or /v1/proxy/anthropic)' })
    return true
  }
  const provider = m[1] as 'openai' | 'anthropic'
  const rawSuffix = m[2] || '/'
  const auth = resolveProxyAuth(req, provider, ctx.alreadyAuthed, env)
  if (!auth.ok) {
    send(401, { error: 'unauthorized — supply X-Nektar-Token or composite key nkt:<nektarToken>:<providerKey>' })
    return true
  }

  let upstreamPath: string
  try {
    upstreamPath = sanitizeUpstreamPath(rawSuffix)
  } catch (e) {
    send(400, { error: (e as Error).message })
    return true
  }

  const baseUpstream = provider === 'openai' ? openaiUpstream : anthropicUpstream
  // Normalize `/v1/proxy/openai/chat/completions` -> `${upstream}/v1/chat/completions` when upstream has no /v1 suffix
  const needsV1 =
    !upstreamPath.startsWith('/v1/') &&
    !baseUpstream.endsWith('/v1') &&
    (upstreamPath === '/chat/completions' || upstreamPath === '/messages' || upstreamPath === '/responses' || upstreamPath === '/models')
  const targetUrl = `${baseUpstream}${needsV1 ? '/v1' : ''}${upstreamPath}${url.search}`

  const shardHdr = typeof req.headers['x-nektar-shard'] === 'string' ? req.headers['x-nektar-shard'].trim() : ''
  const defaultShard = shardHdr || nektarEnv('NEKTAR_PROXY_SHARD', env) || 'proxy'
  const recallDisabled = req.headers['x-nektar-recall'] === '0' || req.headers['x-nektar-recall'] === 'false'
  const captureOn =
    req.headers['x-nektar-capture'] === '1' ||
    req.headers['x-nektar-capture'] === 'true' ||
    (req.headers['x-nektar-capture'] !== '0' && nektarEnvFlag('NEKTAR_PROXY_CAPTURE', env) === true)

  const isInferencePost =
    req.method === 'POST' && (upstreamPath.endsWith('/chat/completions') || upstreamPath.endsWith('/messages') || upstreamPath.endsWith('/responses'))

  let forwardBody: string | undefined
  let userPrompt = ''
  let memoryHits = 0

  if (req.method !== 'GET' && req.method !== 'HEAD') {
    const parsed = (await readBody().catch(() => ({}))) as Record<string, unknown>
    let payload = parsed
    if (isInferencePost && !recallDisabled) {
      userPrompt = extractUserPrompt(provider, upstreamPath, parsed)
      if (userPrompt.trim() !== '') {
        const recallArgs: Record<string, unknown> = { query: userPrompt.trim(), limit: 5 }
        if (shardHdr) recallArgs.shard = shardHdr
        const resRecall = await deadline(
          async () =>
            (await callTool(ctx.mem, 'recall', recallArgs)) as {
              items?: Array<{ shard?: string; seq?: number; text?: string; check?: { verified?: boolean } }>
            },
          recallTimeoutMs
        ).catch(() => undefined)
        if (resRecall?.outcome === 'settled' && Array.isArray(resRecall.value.items)) {
          const formatted = formatRecallBlock(resRecall.value.items)
          if (formatted.count > 0) {
            payload = injectMemoryBlock(provider, upstreamPath, parsed, formatted.block)
            memoryHits = formatted.count
          }
        }
      }
    }
    forwardBody = JSON.stringify(payload)
  }

  const fwdHeaders: Record<string, string> = {}
  for (const [k, v] of Object.entries(req.headers)) {
    const low = k.toLowerCase()
    if (SKIP_REQ_HEADERS.has(low) || v === undefined) continue
    fwdHeaders[k] = Array.isArray(v) ? v.join(', ') : v
  }
  if (auth.upstreamKeyOverride !== undefined) {
    if (provider === 'anthropic') fwdHeaders['x-api-key'] = auth.upstreamKeyOverride
    else fwdHeaders['authorization'] = `Bearer ${auth.upstreamKeyOverride}`
  }
  if (forwardBody !== undefined && !fwdHeaders['content-type']) fwdHeaders['content-type'] = 'application/json'

  let upstreamRes: Response
  try {
    upstreamRes = await fetchImpl(targetUrl, {
      method: req.method,
      headers: fwdHeaders,
      body: forwardBody,
      signal: AbortSignal.timeout(120_000)
    })
  } catch (e) {
    send(502, { error: `upstream ${provider} unreachable: ${(e as Error).message}` })
    return true
  }

  const outHeaders: Record<string, string> = {
    'x-nektar-memory-hits': String(memoryHits),
    'x-nektar-shard': shardHdr || '*'
  }
  upstreamRes.headers.forEach((v, k) => {
    if (!SKIP_RES_HEADERS.has(k.toLowerCase())) outHeaders[k] = v
  })
  const ct = String(upstreamRes.headers.get('content-type') ?? '').toLowerCase()
  const isStream = ct.includes('text/event-stream')
  if (isStream) outHeaders['cache-control'] = 'no-cache'

  res.writeHead(upstreamRes.status, outHeaders)

  let teed = ''
  if (upstreamRes.body) {
    const reader = upstreamRes.body.getReader()
    const dec = new TextDecoder()
    try {
      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        res.write(Buffer.from(value))
        if (isInferencePost && teed.length < MAX_TEE_CHARS) {
          teed += dec.decode(value, { stream: true })
        }
      }
    } finally {
      reader.releaseLock()
    }
  }
  res.end()

  if (isInferencePost && upstreamRes.ok && teed.length > 0) {
    void (async () => {
      try {
        const assistantText = extractAssistantTextFromResponse(provider, teed, isStream)
        if (!assistantText) return
        const tokens = [...new Set(assistantText.match(RECEIPT_RE) ?? [])].slice(0, 3)
        if (tokens.length > 0) {
          const { verifyReceipt } = await import('./receipt.js')
          for (const tok of tokens) {
            const vr = await verifyReceipt(ctx.mem, { token: tok }).catch(() => undefined)
            const ans = (vr?.receipt as { answer?: string } | undefined)?.answer
            const drift = Boolean(ans) && !norm(assistantText).includes(norm(ans ?? ''))
            const ok = vr?.ok === true && !drift
            plog(`receipt ${ok ? 'VERIFIED' : 'FAILED'} ${tok.slice(0, 24)}…${drift ? ' (answer-drift)' : ''}`)
          }
          ctx.mem.events.emit('change', 'proxy')
        }
        if (captureOn && userPrompt.trim() !== '') {
          const summary = `Q: ${userPrompt.trim().slice(0, 300)}\nA: ${assistantText.trim().slice(0, 500)}`
          await ctx.mem.remember(defaultShard, summary, { source: `proxy:${provider}` }).catch(() => {})
        }
      } catch {
        /* post-turn verification/capture never disrupts responses */
      }
    })()
  }

  return true
}
