// hooks-cli.ts — portable, zero-checkout AI-harness hooks built into the
// nektar binary:
//   nektar hook session-start            inject 1-line status + grounding rule
//   nektar hook prefetch                 pre-turn bounded verified memory recall
//   nektar hook verify-receipt           Claude Code Stop hook (blocks once on failure)
//   nektar hook verify-receipt --codex   Codex notify hook (logs + optional --chain)
import { spawn } from 'node:child_process'
import { appendFileSync, readFileSync } from 'node:fs'
import { deadline, nektarEnv, writeErr, writeOut, type EnvSource } from '@nektar/core'
import { discoverDaemon } from './daemon.js'

const isLoopbackOrigin = (origin: string): boolean => {
  try {
    const h = new URL(origin).hostname
    return h === '127.0.0.1' || h === 'localhost' || h === '::1' || h === '[::1]'
  } catch {
    return false
  }
}

export interface HookIo {
  out?: (s: string) => void
  err?: (s: string) => void
  stdinText?: () => Promise<string>
  fetchImpl?: typeof fetch
  env?: EnvSource
  dataDir?: string
}

const norm = (s: string | undefined): string => (s ?? '').toLowerCase().replace(/\s+/g, ' ').trim()

const readStdin = async (): Promise<string> => {
  if (process.stdin.isTTY) return ''
  let buf = ''
  for await (const chunk of process.stdin) buf += String(chunk)
  return buf
}

const checkLine = (checks: Array<{ name: string; ok: unknown }>, receipt?: { modelAttestation?: { verifierTxid?: string } }): string => {
  const ico = (ok: unknown) => (ok === true ? '✓' : ok === false ? '✗' : '·')
  return checks
    .map(
      (c) =>
        `${c.name === 'model' && receipt?.modelAttestation ? `model:${receipt.modelAttestation.verifierTxid ? 'on-chain' : 'attested'}` : c.name}${ico(c.ok)}`
    )
    .join(' ')
}

function extractTokens(text: string): string[] {
  return [...new Set(text.match(/nektarr1\.[A-Za-z0-9_-]{40,}/g) ?? [])].slice(0, 3)
}

function rankItems(
  items: Array<{ shard?: string; seq?: number; text?: string; check?: { verified?: boolean } }>,
  query: string
): Array<{ shard?: string; seq?: number; text?: string }> {
  const words = norm(query)
    .split(/[^a-z0-9]+/)
    .filter((w) => w.length >= 3)
  const verified = items.filter((i) => typeof i.text === 'string' && i.text.trim() !== '' && i.check?.verified !== false)
  if (words.length === 0) return verified.slice(0, 5)
  const scored = verified.map((item, idx) => {
    const low = norm(item.text)
    let score = 0
    for (const w of words) if (low.includes(w)) score += 1
    return { item, score, idx }
  })
  scored.sort((a, b) => b.score - a.score || a.idx - b.idx)
  const hits = scored.filter((s) => s.score > 0).map((s) => s.item)
  return (hits.length > 0 ? hits : verified).slice(0, 5)
}

async function runSessionStart(io: HookIo): Promise<number> {
  const out = io.out ?? writeOut
  const fetchImpl = io.fetchImpl ?? fetch
  const env = io.env ?? process.env
  const daemon = await discoverDaemon({ dataDir: io.dataDir, env, fetchImpl, timeoutMs: 800 })
  if (!daemon) return 0
  const headers: Record<string, string> = { 'content-type': 'application/json' }
  if (daemon.token) headers.authorization = `Bearer ${daemon.token}`
  try {
    const r = await fetchImpl(`${daemon.url}/v1/tools/doctor`, {
      method: 'POST',
      headers,
      body: '{}',
      signal: AbortSignal.timeout(1_500)
    })
    if (!r.ok) return 0
    const d = (await r.json()) as {
      records?: number
      shards?: number
      anchoredRecords?: number
      network?: string
      prover?: string
      funding?: { address?: string; balanceSats?: number }
    }
    const f = d.funding ?? {}
    out(
      `nektar ready (${daemon.url}): ${d.records ?? 0} record(s) on ${d.shards ?? 0} shard(s), ` +
        `${d.anchoredRecords ?? 0} anchored (${d.network ?? 'unknown'}) — recall first, then route ` +
        `answers through groundedAnswer; keep the receipt footer it returns. ` +
        `Fee key ${f.address ?? 'unfunded'} ${f.balanceSats ?? 0} sats · prover ${d.prover ?? 'unknown'}\n`
    )
  } catch {
    /* fail-open: hooks never block session start */
  }
  return 0
}

async function runPrefetch(io: HookIo): Promise<number> {
  const out = io.out ?? writeOut
  const fetchImpl = io.fetchImpl ?? fetch
  const env = io.env ?? process.env
  const raw = await (io.stdinText ?? readStdin)()
  let prompt = ''
  try {
    const parsed = JSON.parse(raw || '{}') as { prompt?: string; user_prompt?: string; message?: string }
    prompt = parsed.prompt ?? parsed.user_prompt ?? parsed.message ?? ''
  } catch {
    prompt = raw
  }
  const daemon = await discoverDaemon({ dataDir: io.dataDir, env, fetchImpl, timeoutMs: 400 })
  if (!daemon) return 0
  const headers: Record<string, string> = { 'content-type': 'application/json' }
  if (daemon.token) headers.authorization = `Bearer ${daemon.token}`
  try {
    const bodyPayload: Record<string, unknown> = { limit: 5 }
    if (prompt.trim()) bodyPayload.query = prompt.trim()
    const r = await fetchImpl(`${daemon.url}/v1/tools/recall`, {
      method: 'POST',
      headers,
      body: JSON.stringify(bodyPayload),
      signal: AbortSignal.timeout(800)
    })
    if (!r.ok) return 0
    const j = (await r.json()) as { items?: Array<{ shard?: string; seq?: number; text?: string; check?: { verified?: boolean } }> }
    const picks = rankItems(j.items ?? [], prompt)
    if (picks.length === 0) return 0
    const lines: string[] = ['[nektar verified memory — cite via groundedAnswer; proofs show recorded+unaltered, not truth]']
    let chars = lines[0].length
    for (const p of picks) {
      const snippet = (p.text ?? '').replace(/\s+/g, ' ').trim().slice(0, 260)
      const line = `- [${p.shard ?? '?'}:${p.seq ?? 0}] ${snippet}`
      if (chars + line.length > 1400) break
      lines.push(line)
      chars += line.length + 1
    }
    if (lines.length > 1) out(lines.join('\n') + '\n')
  } catch {
    /* fail-open */
  }
  return 0
}

async function runVerifyReceipt(argv: string[], io: HookIo): Promise<number> {
  const out = io.out ?? writeOut
  const err = io.err ?? writeErr
  const fetchImpl = io.fetchImpl ?? fetch
  const env = io.env ?? process.env
  const isCodex = argv.includes('--codex')

  let text = ''
  let stopHookActive = false

  if (isCodex) {
    const chainIdx = argv.indexOf('--chain')
    const payload = argv.at(-1) ?? ''
    const chainArgs = chainIdx >= 0 ? argv.slice(chainIdx + 1, -1) : []
    if (chainArgs.length > 0) {
      try {
        await deadline(async (signal) => {
          await new Promise<void>((res) => {
            const c = spawn(chainArgs[0], [...chainArgs.slice(1), payload], { stdio: 'inherit' })
            const onAbort = () => {
              try {
                c.kill()
              } catch {
                /* already exited */
              }
              res()
            }
            signal.addEventListener('abort', onAbort, { once: true })
            c.on('close', () => {
              signal.removeEventListener('abort', onAbort)
              res()
            })
            c.on('error', () => {
              signal.removeEventListener('abort', onAbort)
              res()
            })
          })
        }, 5_000)
      } catch {
        /* ignore chain error */
      }
    }
    try {
      const j = JSON.parse(payload || '{}') as Record<string, unknown>
      const rawMsg = j['last-assistant-message'] ?? j.lastAssistantMessage
      text = typeof rawMsg === 'string' ? rawMsg : ''
    } catch {
      text = payload
    }
  } else {
    const raw = await (io.stdinText ?? readStdin)()
    let meta: { transcript_path?: string; stop_hook_active?: boolean; last_assistant_message?: string } = {}
    try {
      meta = JSON.parse(raw || '{}') as typeof meta
    } catch {
      /* ignore */
    }
    stopHookActive = meta.stop_hook_active === true
    if (typeof meta.last_assistant_message === 'string') {
      text = meta.last_assistant_message
    } else if (typeof meta.transcript_path === 'string' && meta.transcript_path !== '') {
      try {
        const lines = readFileSync(meta.transcript_path, 'utf8').trim().split('\n')
        for (let i = lines.length - 1; i >= 0 && !text; i--) {
          try {
            const m = JSON.parse(lines[i]) as { type?: string; message?: { content?: Array<{ type?: string; text?: string }> } }
            if (m.type === 'assistant') {
              text = (m.message?.content ?? [])
                .filter((p) => p.type === 'text')
                .map((p) => p.text ?? '')
                .join('\n')
            }
          } catch {
            /* skip bad line */
          }
        }
      } catch {
        return 0
      }
    }
  }

  const tokens = extractTokens(text)
  if (tokens.length === 0) return 0

  let base = nektarEnv('NEKTAR_VERIFY_BASE', env)
  if (!base) {
    try {
      const m = text.match(/verify:\s*(https?:\/\/\S+)/i)
      const cand = m?.[1] ? new URL(m[1]).origin : undefined
      // Only trust a loopback verify: origin extracted from untrusted assistant
      // text — prevents SSRF, token exfiltration, and remote verification spoofing.
      if (cand && isLoopbackOrigin(cand)) base = cand
    } catch {
      /* ignore invalid URL */
    }
  }
  if (!base) {
    const discovered = await discoverDaemon({ dataDir: io.dataDir, env, fetchImpl, timeoutMs: 500 })
    base = discovered?.url ?? `http://127.0.0.1:${nektarEnv('NEKTAR_HTTP_PORT', env) ?? '18420'}`
  }

  const results: Array<{
    token: string
    ok: boolean | 'local' | null
    failNames: string[]
    checks: Array<{ name: string; ok: unknown }>
    receipt?: { answer?: string; modelAttestation?: { verifierTxid?: string } }
    note?: string
  }> = []

  for (const t of tokens) {
    try {
      const r = await fetchImpl(`${base}/v1/verify?r=${encodeURIComponent(t)}`, { signal: AbortSignal.timeout(3_000) })
      const j = (await r.json()) as {
        ok?: boolean
        localVerified?: boolean
        checks?: Array<{ name: string; ok: unknown }>
        receipt?: { answer?: string; modelAttestation?: { verifierTxid?: string } }
      }
      const checks = j.checks ?? []
      const fails = checks.filter((c) => c.ok === false).map((c) => c.name)
      const drift = Boolean(j.receipt?.answer) && !norm(text).includes(norm(j.receipt?.answer))
      if (drift) fails.push('answer-drift')
      const localOnly = !drift && j.ok !== true && j.localVerified === true
      results.push({
        token: t,
        ok: j.ok === true && !drift ? true : localOnly ? 'local' : false,
        failNames: fails,
        checks,
        receipt: j.receipt
      })
    } catch (e) {
      results.push({ token: t, ok: null, failNames: [], checks: [], note: `verify endpoint unreachable (${(e as Error).message})` })
    }
  }

  const logLines: string[] = []
  for (const r of results) {
    const icon = r.ok === true ? '✓ VERIFIED' : r.ok === 'local' ? '~ LOCAL-SIGNED (unanchored)' : r.ok === false ? '✗ FAILED' : '? UNCHECKED'
    const summary = `${icon} ${r.token.slice(0, 24)}…${r.ok === false ? ' — ' + r.failNames.join(', ') : r.ok === null ? ' — ' + (r.note ?? '') : ''}`
    logLines.push(summary)
    err(`nektar ${summary}\n`)
    if (r.checks.length > 0) {
      const cl = `  ${checkLine(r.checks, r.receipt)}`
      logLines.push(cl)
      err(`${cl}\n`)
    }
  }

  if (isCodex) {
    const stamp = new Date().toISOString()
    try {
      appendFileSync('codex-receipts.log', logLines.map((l) => `${stamp} nektar ${l}`).join('\n') + '\n')
    } catch {
      /* read-only cwd */
    }
    const notifyCmd = nektarEnv('NEKTAR_NOTIFY_CMD', env)
    if (notifyCmd) {
      try {
        // Pass untrusted verify summaries via env + stdin — never interpolate
        // untrusted text as shell-evaluated command arguments.
        const notifyText = logLines.join('\n')
        const child = spawn(notifyCmd, [], {
          stdio: ['pipe', 'ignore', 'ignore'],
          shell: true,
          env: { ...process.env, NEKTAR_NOTIFY_TEXT: notifyText }
        })
        child.on('error', () => {})
        child.stdin?.on('error', () => {})
        child.stdin?.end(notifyText + '\n')
        child.unref()
      } catch {
        /* ignore */
      }
    }
    return 0
  }

  const failed = results.filter((r) => r.ok === false)
  if (failed.length > 0 && !stopHookActive) {
    const names = [...new Set(failed.flatMap((f) => f.failNames))]
    out(
      JSON.stringify({
        decision: 'block',
        reason: `nektar receipt verification FAILED — checks that failed: ${names.join(', ')}. Tell the user the receipt did not verify (which check failed and why that matters); do NOT claim the answer is verified.${names.includes('answer-drift') ? ' answer-drift means the printed answer is not the committed receipt text — the receipt does not cover what the user was shown.' : ''}`
      }) + '\n'
    )
  }
  return 0
}

const USAGE = `nektar hook — portable AI-harness hooks (no checkout required)

  nektar hook session-start                 print 1-line memory status for SessionStart
  nektar hook prefetch                      pre-turn verified memory recall (UserPromptSubmit)
  nektar hook verify-receipt [--codex]      verify nektarr1. receipts (Claude Stop / Codex notify)
`

export async function runHookCli(argv: string[], io: HookIo = {}): Promise<number> {
  const out = io.out ?? writeOut
  const err = io.err ?? writeErr
  const sub = argv[0]
  if (!sub || sub === 'help' || sub === '--help' || sub === '-h') {
    out(USAGE)
    return 0
  }
  if (sub === 'session-start') return await runSessionStart(io)
  if (sub === 'prefetch') return await runPrefetch(io)
  if (sub === 'verify-receipt') return await runVerifyReceipt(argv.slice(1), io)
  err(`nektar hook: unknown subcommand "${sub}"\n`)
  return 2
}
