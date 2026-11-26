// integrations/openclaw/index.mjs — native OpenClaw `plugins.slots.memory`
// plugin for nektar. Zero external runtime dependencies: talks directly to the
// local or remote nektar HTTP surface (/v1/tools/*, /.well-known/agent.json,
// /v1/verify) with bounded fail-open hooks.
import { existsSync, readFileSync } from 'node:fs'
import { homedir } from 'node:os'
import { join } from 'node:path'

const RECEIPT_RE = /nektarr1\.[A-Za-z0-9_-]{40,}/g
const VERIFY_URL_RE = /verify:\s*(https?:\/\/\S+)/i
const DEFAULT_PORTS = [18420, 8080]

const norm = (s) =>
  String(s ?? '')
    .toLowerCase()
    .replace(/\s+/g, ' ')
    .trim()

function readDaemonFile(p) {
  if (!existsSync(p)) return undefined
  try {
    const j = JSON.parse(readFileSync(p, 'utf8'))
    const url = typeof j?.url === 'string' && /^https?:\/\//i.test(j.url) ? j.url.replace(/\/+$/, '') : undefined
    const token = typeof j?.authToken === 'string' && j.authToken ? j.authToken : typeof j?.token === 'string' && j.token ? j.token : undefined
    return url ? { url, token } : undefined
  } catch {
    return undefined
  }
}

function readDaemonInfo(dir) {
  return readDaemonFile(join(dir, 'daemon.json'))
}

async function discoverNektar(cfg, fetchImpl = fetch) {
  const seen = new Set()
  const candidates = []
  const add = (u) => {
    if (!u) return
    const clean = String(u).trim().replace(/\/+$/, '')
    if (clean && !seen.has(clean)) {
      seen.add(clean)
      candidates.push(clean)
    }
  }
  add(cfg?.url)
  add(process.env.NEKTAR_URL)
  add(process.env.NEKTAR_DAEMON_URL)
  for (const d of [
    readDaemonInfo(join(process.cwd(), '.nektar')),
    readDaemonFile(join(homedir(), '.nektar', 'active-daemon.json')),
    readDaemonInfo(join(homedir(), '.nektar', 'main')),
    readDaemonInfo(join(homedir(), '.nektar'))
  ]) {
    if (d?.token && cfg && !cfg.token) cfg.token = d.token
    add(d?.url)
  }
  if (process.env.NEKTAR_HTTP_PORT) add(`http://127.0.0.1:${process.env.NEKTAR_HTTP_PORT}`)
  for (const p of DEFAULT_PORTS) add(`http://127.0.0.1:${p}`)

  for (const base of candidates) {
    try {
      const r = await fetchImpl(`${base}/.well-known/agent.json`, { signal: AbortSignal.timeout(500) })
      if (!r.ok) continue
      const card = await r.json()
      if (card?.name === 'nektar') return base
    } catch {
      /* try next */
    }
  }
  return undefined
}

function rankItems(items, query, limit = 5) {
  const words = norm(query)
    .split(/[^a-z0-9]+/)
    .filter((w) => w.length >= 3)
  const verified = (items ?? []).filter((it) => typeof it?.text === 'string' && it.text.trim() !== '' && it?.check?.verified !== false)
  if (words.length === 0) return verified.slice(0, limit)
  const scored = verified.map((item, idx) => {
    const low = norm(item.text)
    let score = 0
    for (const w of words) if (low.includes(w)) score += 1
    return { item, score, idx }
  })
  scored.sort((a, b) => b.score - a.score || a.idx - b.idx)
  const hits = scored.filter((s) => s.score > 0).map((s) => s.item)
  return (hits.length > 0 ? hits : verified).slice(0, limit)
}

async function callNektarTool(baseUrl, token, tool, args, timeoutMs = 4000, fetchImpl = fetch) {
  const headers = { 'content-type': 'application/json' }
  if (token) headers.authorization = `Bearer ${token}`
  const r = await fetchImpl(`${baseUrl}/v1/tools/${encodeURIComponent(tool)}`, {
    method: 'POST',
    headers,
    body: JSON.stringify(args ?? {}),
    signal: AbortSignal.timeout(timeoutMs)
  })
  if (!r.ok) throw new Error(`nektar ${tool} failed (HTTP ${r.status})`)
  return await r.json()
}

export function createNektarOpenClawPlugin(opts = {}) {
  const fetchImpl = opts.fetchImpl ?? fetch
  return {
    id: 'nektar-memory',
    name: 'nektar Verifiable Memory',
    kind: 'memory',

    register(api) {
      const cfg = api.pluginConfig ?? api.config ?? {}
      const token = cfg.token ?? process.env.NEKTAR_HTTP_TOKEN
      const defaultShard = cfg.defaultShard ?? 'openclaw'
      const autoRecall = cfg.autoRecall !== false
      const verifyReceipts = cfg.verifyReceipts !== false
      const autoCapture = cfg.autoCapture === true
      const prefetchTimeoutMs = Number(cfg.prefetchTimeoutMs ?? 800)
      const prefetchMaxChars = Number(cfg.prefetchMaxChars ?? 1600)

      let cachedUrl = cfg.url ? String(cfg.url).replace(/\/+$/, '') : undefined
      const resolveUrl = async () => {
        if (cachedUrl) return cachedUrl
        cachedUrl = await discoverNektar(cfg, fetchImpl)
        return cachedUrl
      }

      if (typeof api.registerContextEngine === 'function') {
        api.registerContextEngine('nektar-memory', () => ({
          info: { id: 'nektar-memory', name: 'nektar Verifiable Memory', ownsCompaction: false }
        }))
      }

      // 1. Pre-turn verified memory prefetch (bounded, fail-open)
      if (autoRecall && typeof api.on === 'function') {
        api.on(
          'before_prompt_build',
          async (event) => {
            const base = await resolveUrl()
            if (!base) return undefined
            const query = String(event?.prompt ?? event?.message ?? event?.userMessage ?? '')
            try {
              const payload = query.trim() ? { query: query.trim(), limit: 5 } : { limit: 5 }
              const res = await callNektarTool(base, token, 'recall', payload, prefetchTimeoutMs, fetchImpl)
              const picks = rankItems(res?.items, query, 5)
              if (picks.length === 0) return undefined
              const lines = ['[nektar verified memory — cite via nektar_grounded_answer; proofs show recorded+unaltered, not truth]']
              let used = lines[0].length
              for (const it of picks) {
                const snippet = String(it.text ?? '')
                  .replace(/\s+/g, ' ')
                  .trim()
                  .slice(0, 260)
                const line = `- [${it.shard ?? '?'}:${it.seq ?? 0}] ${snippet}`
                if (used + line.length + 1 > prefetchMaxChars) break
                lines.push(line)
                used += line.length + 1
              }
              if (lines.length <= 1) return undefined
              return { prependContext: lines.join('\n') }
            } catch {
              return undefined
            }
          },
          { priority: 100 }
        )
      }

      // 2. Post-turn receipt verification (+ opt-in capture)
      if ((verifyReceipts || autoCapture) && typeof api.on === 'function') {
        api.on(
          'agent_end',
          async (event) => {
            const assistantText = String(event?.assistantMessage ?? event?.output ?? event?.response ?? '')
            const base = await resolveUrl()
            if (!base) return
            if (verifyReceipts && assistantText) {
              const tokens = [...new Set(assistantText.match(RECEIPT_RE) ?? [])].slice(0, 3)
              let verifyBase = base
              const m = assistantText.match(VERIFY_URL_RE)
              if (m?.[1]) {
                try {
                  const u = new URL(m[1])
                  const h = u.hostname.toLowerCase()
                  // Only trust loopback or our own configured base from untrusted assistant text
                  if (h === '127.0.0.1' || h === 'localhost' || h === '::1' || h === '[::1]' || u.origin === base) {
                    verifyBase = u.origin
                  }
                } catch {
                  /* fallback to base */
                }
              }
              for (const tok of tokens) {
                try {
                  const r = await fetchImpl(`${verifyBase}/v1/verify?r=${encodeURIComponent(tok)}`, { signal: AbortSignal.timeout(2500) })
                  const j = await r.json()
                  const drift = Boolean(j?.receipt?.answer) && !norm(assistantText).includes(norm(j.receipt.answer))
                  const ok = j?.ok === true && !drift
                  api.logger?.info?.(`nektar receipt ${ok ? 'VERIFIED' : 'FAILED'} ${tok.slice(0, 24)}…${drift ? ' (answer-drift)' : ''}`)
                } catch {
                  /* fail-open */
                }
              }
            }
            if (autoCapture) {
              const userText = String(event?.userMessage ?? event?.prompt ?? '').trim()
              if (userText && assistantText.trim()) {
                try {
                  await callNektarTool(
                    base,
                    token,
                    'remember',
                    {
                      shard: defaultShard,
                      data: `Q: ${userText.slice(0, 300)}\nA: ${assistantText.trim().slice(0, 500)}`,
                      source: `openclaw:turn:${event?.sessionId ?? 'session'}`
                    },
                    3000,
                    fetchImpl
                  )
                } catch {
                  /* fail-open */
                }
              }
            }
          },
          { priority: 100 }
        )
      }

      // 3. Native tool registration (works even without separate MCP config)
      if (typeof api.registerTool === 'function') {
        const defs = [
          ['nektar_recall', 'recall', 'Recall verified fragments from nektar memory (supports query, limit, shard).'],
          ['nektar_remember', 'remember', 'Persist a durable fact into nektar verifiable memory with provenance (supports supersedes).'],
          ['nektar_forget', 'forget', 'Append a tombstone hiding an earlier record from active recall views without deleting history.'],
          ['nektar_grounded_answer', 'groundedAnswer', 'Produce a grounded answer citing verified fragments with a checkable receipt footer.'],
          ['nektar_verify_receipt', 'verifyReceipt', 'Verify a nektarr1.<token> answer receipt.'],
          ['nektar_want', 'want', 'Publish a policy-bounded knowledge buy want on the nektar BSV market.'],
          ['nektar_sell', 'sell', 'List a shard for sale under an operator-approved seller policy.'],
          ['nektar_deals', 'deals', 'Inspect active and completed nektar knowledge trades.'],
          ['nektar_doctor', 'doctor', 'Check nektar instance health, shards, funding balance, and next-step hints.']
        ]
        for (const [toolName, remoteName, description] of defs) {
          api.registerTool({
            name: toolName,
            description,
            execute: async (args) => {
              const base = await resolveUrl()
              if (!base) return { ok: false, error: 'nektar daemon not reachable — start with `nektar serve`' }
              const payload = { ...args }
              if (remoteName === 'remember' && !payload.shard) payload.shard = defaultShard
              return await callNektarTool(base, token, remoteName, payload, 15_000, fetchImpl)
            }
          })
        }
      }
    }
  }
}

export default createNektarOpenClawPlugin()
