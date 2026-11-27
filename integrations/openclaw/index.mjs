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

function readDaemonPointers(home = homedir(), cwd = process.cwd()) {
  return [
    readDaemonInfo(join(cwd, '.nektar')),
    readDaemonFile(join(home, '.nektar', 'active-daemon.json')),
    readDaemonInfo(join(home, '.nektar', 'main')),
    readDaemonInfo(join(home, '.nektar'))
  ].filter(Boolean)
}

export async function discoverNektar(cfg, fetchImpl = fetch, opts = {}) {
  const explicitToken = (cfg?.token ?? process.env.NEKTAR_HTTP_TOKEN ?? '').trim() || undefined
  const seen = new Map()
  const candidates = []
  const add = (u, tok) => {
    if (!u) return
    const clean = String(u).trim().replace(/\/+$/, '')
    if (!clean) return
    const resolvedTok = explicitToken ?? (tok ? String(tok).trim() || undefined : undefined)
    if (seen.has(clean)) {
      const existing = seen.get(clean)
      if (!existing.token && resolvedTok) existing.token = resolvedTok
      return
    }
    const entry = { url: clean, token: resolvedTok }
    seen.set(clean, entry)
    candidates.push(entry)
  }

  add(cfg?.url, explicitToken)
  add(process.env.NEKTAR_URL, explicitToken)
  add(process.env.NEKTAR_DAEMON_URL, explicitToken)
  for (const d of readDaemonPointers(opts.home, opts.cwd)) {
    // Pair each URL with its own daemon.json token — never latch a stale daemon's token onto another candidate.
    add(d.url, d.token)
  }
  if (process.env.NEKTAR_HTTP_PORT) add(`http://127.0.0.1:${process.env.NEKTAR_HTTP_PORT}`, explicitToken)
  for (const p of DEFAULT_PORTS) add(`http://127.0.0.1:${p}`, explicitToken)

  for (const cand of candidates) {
    try {
      const r = await fetchImpl(`${cand.url}/.well-known/agent.json`, { signal: AbortSignal.timeout(500) })
      if (!r.ok) continue
      const card = await r.json()
      if (card?.name === 'nektar') return { url: cand.url, token: cand.token }
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
  if (!r.ok) {
    const err = new Error(`nektar ${tool} failed (HTTP ${r.status})`)
    err.status = r.status
    throw err
  }
  return await r.json()
}

export const NEKTAR_OPENCLAW_TOOL_DEFS = [
  {
    name: 'nektar_recall',
    remoteName: 'recall',
    description: 'Recall verified fragments from nektar memory (supports query, limit, shard, repo, branch, atCommit).',
    parameters: {
      type: 'object',
      properties: {
        shard: { type: 'string', description: 'Optional shard/repo name; omit to recall across all readable shards.' },
        repo: { type: 'string', description: 'Optional Memory Repo name alias for shard.' },
        branch: { type: 'string', description: 'Optional branch name (default: main).' },
        atCommit: { type: 'string', description: 'Optional commit hash for epistemic time-travel recall.' },
        query: { type: 'string', description: 'Optional FTS5 search query to rank relevant memories.' },
        limit: { type: 'integer', description: 'Maximum number of fragments to return.' },
        includeSuperseded: { type: 'boolean', description: 'Include superseded records.' }
      }
    }
  },
  {
    name: 'nektar_remember',
    remoteName: 'remember',
    description: 'Persist a durable fact or file into nektar verifiable memory with provenance (supports text/data, path, supersedes, autoAnchor).',
    parameters: {
      type: 'object',
      properties: {
        shard: { type: 'string', description: 'Target shard or repo name.' },
        repo: { type: 'string', description: 'Optional Memory Repo alias for shard.' },
        text: { type: 'string', description: 'Fact or file content to persist.' },
        data: { type: 'string', description: 'Alias for text.' },
        path: { type: 'string', description: 'Optional file path inside the memory repo.' },
        branch: { type: 'string', description: 'Optional branch name.' },
        commitMessage: { type: 'string', description: 'Optional commit message.' },
        source: { type: 'string', description: 'Provenance source tag.' },
        supersedes: { type: 'integer', description: 'Optional prior seq superseded by this fact.' },
        autoAnchor: { type: 'boolean', description: 'Anchor on BSV immediately if wallet is funded.' }
      }
    }
  },
  {
    name: 'nektar_forget',
    remoteName: 'forget',
    description: 'Shred local plaintext of a fragment while preserving its on-chain Merkle commitment.',
    parameters: {
      type: 'object',
      properties: {
        shard: { type: 'string', description: 'Shard name.' },
        seq: { type: 'integer', description: 'Fragment sequence number.' },
        reason: { type: 'string', description: 'Optional audit reason.' }
      },
      required: ['shard', 'seq']
    }
  },
  {
    name: 'nektar_grounded_answer',
    remoteName: 'groundedAnswer',
    description: 'Produce a grounded answer citing verified fragments with a checkable nektarr1.* receipt footer.',
    parameters: {
      type: 'object',
      properties: {
        question: { type: 'string', description: 'Question being answered.' },
        claims: {
          type: 'array',
          description: 'Claims to ground against verified fragments.',
          items: {
            type: 'object',
            properties: {
              text: { type: 'string' },
              cites: {
                type: 'array',
                items: {
                  type: 'object',
                  properties: { shard: { type: 'string' }, seq: { type: 'integer' } },
                  required: ['shard', 'seq']
                }
              }
            },
            required: ['text', 'cites']
          }
        }
      },
      required: ['claims']
    }
  },
  {
    name: 'nektar_verify_receipt',
    remoteName: 'verifyReceipt',
    description: 'Verify a nektarr1.<token> answer receipt and the SPV status of every cited fragment.',
    parameters: {
      type: 'object',
      properties: {
        token: { type: 'string', description: 'Full nektarr1.<token> string.' }
      },
      required: ['token']
    }
  },
  {
    name: 'nektar_repo',
    remoteName: 'repo',
    description: 'Full Git + SingularGrit Memory Repo operations anchored on BSV (action: list|log|show|tree|read|diff|blame|verify|commit|rm|branch|fork|merge|cherryPick|revert|tag|export|import|prove|rotate).',
    parameters: {
      type: 'object',
      properties: {
        action: { type: 'string', description: 'Repo operation to execute.' },
        repo: { type: 'string', description: 'Target Memory Repo name.' },
        branch: { type: 'string', description: 'Branch name (default: main).' },
        path: { type: 'string', description: 'File path for read/commit/rm/blame.' },
        text: { type: 'string', description: 'File content for commit.' },
        commit: { type: 'string', description: 'Commit SHA for show/diff/revert/tag/prove.' },
        from: { type: 'string', description: 'Base ref/branch for diff/branch.' },
        to: { type: 'string', description: 'Target ref/branch for diff.' },
        sourceBranch: { type: 'string', description: 'Source branch for merge.' },
        targetBranch: { type: 'string', description: 'Target branch for merge.' },
        newRepo: { type: 'string', description: 'Destination repo name for fork.' },
        tag: { type: 'string', description: 'Tag name.' },
        message: { type: 'string', description: 'Commit or tag message.' },
        autoAnchor: { type: 'boolean', description: 'Anchor on BSV immediately if wallet is funded.' }
      },
      required: ['action']
    }
  },
  {
    name: 'nektar_want',
    remoteName: 'want',
    description: 'Publish a policy-bounded knowledge buy want on the nektar BSV market.',
    parameters: {
      type: 'object',
      properties: {
        domains: { type: 'array', items: { type: 'string' }, description: 'Topic domains to buy.' },
        question: { type: 'string', description: 'Specific verified question/facts desired.' },
        maxSats: { type: 'integer', description: 'Maximum satoshis willing to pay.' },
        standingTtlSeconds: { type: 'integer', description: 'Optional TTL to create a StandingOrder if no seller is online.' }
      },
      required: ['domains', 'maxSats']
    }
  },
  {
    name: 'nektar_sell',
    remoteName: 'sell',
    description: 'List a shard for sale under an operator-approved seller policy, or submit a fragment to a Hub bounty.',
    parameters: {
      type: 'object',
      properties: {
        shard: { type: 'string', description: 'Local shard name to sell.' },
        domains: { type: 'array', items: { type: 'string' }, description: 'Topic domains for discovery.' },
        priceSats: { type: 'integer', description: 'Ask price in satoshis.' },
        description: { type: 'string', description: 'Short listing description.' },
        bountyId: { type: 'string', description: 'Optional Hub bounty ID to fulfill.' },
        seq: { type: 'integer', description: 'Fragment seq when submitting to a Hub bounty.' }
      },
      required: ['shard']
    }
  },
  {
    name: 'nektar_deals',
    remoteName: 'deals',
    description: 'Inspect active and completed nektar knowledge trades and pending approvals.',
    parameters: {
      type: 'object',
      properties: {
        operationId: { type: 'string', description: 'Optional operation ID to inspect.' }
      }
    }
  },
  {
    name: 'nektar_doctor',
    remoteName: 'doctor',
    description: 'Check nektar instance health, shards, funding balance, and next-step hints.',
    parameters: {
      type: 'object',
      properties: {}
    }
  }
]

export function createNektarOpenClawPlugin(opts = {}) {
  const fetchImpl = opts.fetchImpl ?? fetch
  const discoveryOpts = { home: opts.home, cwd: opts.cwd }
  return {
    id: 'nektar-memory',
    name: 'nektar Verifiable Memory',
    kind: 'memory',

    register(api) {
      const cfg = api.pluginConfig ?? api.config ?? {}
      const explicitToken = (cfg.token ?? process.env.NEKTAR_HTTP_TOKEN ?? '').trim() || undefined
      const defaultShard = cfg.defaultShard ?? 'openclaw'
      const autoRecall = cfg.autoRecall !== false
      const verifyReceipts = cfg.verifyReceipts !== false
      const autoCapture = cfg.autoCapture === true
      const prefetchTimeoutMs = Number(cfg.prefetchTimeoutMs ?? 800)
      const prefetchMaxChars = Number(cfg.prefetchMaxChars ?? 1600)

      let cachedConn = undefined
      const resolveConnection = async (forceRefresh = false) => {
        if (cachedConn && !forceRefresh) return cachedConn
        const discovered = await discoverNektar(cfg, fetchImpl, discoveryOpts)
        if (discovered) {
          cachedConn = discovered
          return cachedConn
        }
        if (cfg.url) {
          const clean = String(cfg.url).trim().replace(/\/+$/, '')
          const matched = readDaemonPointers(discoveryOpts.home, discoveryOpts.cwd).find((d) => d.url === clean)
          cachedConn = { url: clean, token: explicitToken ?? matched?.token }
          return cachedConn
        }
        cachedConn = undefined
        return undefined
      }

      const callWithRetry = async (tool, args, timeoutMs) => {
        let conn = await resolveConnection(false)
        if (!conn) throw new Error('nektar daemon not reachable — start with `nektar serve`')
        try {
          return await callNektarTool(conn.url, conn.token, tool, args, timeoutMs, fetchImpl)
        } catch (err) {
          // On 401 or transient connection failure, re-discover active-daemon.json + token once.
          if (err?.status === 401 || err?.status === undefined) {
            const refreshed = await resolveConnection(true)
            if (refreshed && (refreshed.url !== conn.url || refreshed.token !== conn.token)) {
              return await callNektarTool(refreshed.url, refreshed.token, tool, args, timeoutMs, fetchImpl)
            }
          }
          throw err
        }
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
            const query = String(event?.prompt ?? event?.message ?? event?.userMessage ?? '')
            try {
              const payload = query.trim() ? { query: query.trim(), limit: 5 } : { limit: 5 }
              const res = await callWithRetry('recall', payload, prefetchTimeoutMs)
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
            const conn = await resolveConnection(false)
            if (!conn) return
            const base = conn.url
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
                  await callWithRetry(
                    'remember',
                    {
                      shard: defaultShard,
                      data: `Q: ${userText.slice(0, 300)}\nA: ${assistantText.trim().slice(0, 500)}`,
                      source: `openclaw:turn:${event?.sessionId ?? 'session'}`
                    },
                    3000
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
        for (const def of NEKTAR_OPENCLAW_TOOL_DEFS) {
          api.registerTool({
            name: def.name,
            description: def.description,
            parameters: def.parameters,
            execute: async (args) => {
              const payload = { ...(args ?? {}) }
              if (def.remoteName === 'remember' && !payload.shard && !payload.repo) payload.shard = defaultShard
              try {
                return await callWithRetry(def.remoteName, payload, 15_000)
              } catch (err) {
                return { ok: false, error: err instanceof Error ? err.message : String(err) }
              }
            }
          })
        }
      }
    }
  }
}

export default createNektarOpenClawPlugin()
