#!/usr/bin/env node
// Claude Code Stop hook — auto-verifies mamem receipt tokens the assistant
// printed ("receipt: mamemr1.…").
//
// Stdin: {transcript_path, stop_hook_active}. Finds mamemr1. tokens in the
// LAST assistant message, GETs /v1/verify for each (max 3, 3s timeout).
//   - all pass / no token / endpoint down → quiet line, exit 0
//   - any check FAILED → block the stop once: the failure goes back to the
//     model so it must tell the user instead of silently stopping
//     (stop_hook_active guard prevents a block loop)
//
// Env: MAMEM_VERIFY_BASE (https://host) or MAMEM_HTTP_PORT (default 8080).
import { readFileSync } from 'node:fs'

let input = ''
for await (const c of process.stdin) input += c
let meta = {}
try { meta = JSON.parse(input || '{}') } catch {}
const { transcript_path, stop_hook_active } = meta
if (!transcript_path) process.exit(0)

const base = process.env.MAMEM_VERIFY_BASE ?? `http://localhost:${process.env.MAMEM_HTTP_PORT ?? 8080}`

let lines = []
try { lines = readFileSync(transcript_path, 'utf8').trim().split('\n') } catch { process.exit(0) }
let text = ''
for (let i = lines.length - 1; i >= 0 && !text; i--) {
  try {
    const m = JSON.parse(lines[i])
    if (m.type === 'assistant') {
      text = (m.message?.content ?? []).filter(p => p.type === 'text').map(p => p.text).join('\n')
    }
  } catch {}
}
const tokens = [...new Set(text.match(/mamemr1\.[A-Za-z0-9_-]{40,}/g) ?? [])].slice(0, 3)
if (!tokens.length) process.exit(0)

const results = []
for (const t of tokens) {
  try {
    const r = await fetch(`${base}/v1/verify?r=${t}`, { signal: AbortSignal.timeout(3000) })
    const j = await r.json()
    results.push({ token: t, ok: j.ok === true, failNames: (j.checks ?? []).filter(c => c.ok === false).map(c => c.name) })
  } catch (e) {
    results.push({ token: t, ok: null, note: `verify endpoint unreachable (${(e).message})` })
  }
}

for (const r of results) {
  const icon = r.ok === true ? '✓ VERIFIED' : r.ok === false ? '✗ FAILED' : '? UNCHECKED'
  process.stderr.write(`mamem ${icon} ${r.token.slice(0, 24)}…${r.ok === false ? ' — ' + r.failNames.join(', ') : r.ok === null ? ' — ' + r.note : ''}\n`)
}

const failed = results.filter(r => r.ok === false)
if (failed.length && !stop_hook_active) {
  process.stdout.write(JSON.stringify({
    decision: 'block',
    reason: `mamem receipt verification FAILED — checks that failed: ${[...new Set(failed.flatMap(f => f.failNames))].join(', ')}. Tell the user the receipt did not verify (which check failed and why that matters); do NOT claim the answer is verified.`
  }) + '\n')
}
process.exit(0)
