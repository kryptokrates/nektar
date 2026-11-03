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

const norm = s => (s ?? '').toLowerCase().replace(/\s+/g, ' ').trim()

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

// verify host: env override > the 'verify:' URL the answer printed (so the
// hook follows whichever server minted the token) > localhost
let base = process.env.MAMEM_VERIFY_BASE
if (!base) {
  try { base = new URL(text.match(/verify:\s*(https?:\/\/\S+)/i)?.[1] ?? '').origin } catch {}
}
base ??= `http://localhost:${process.env.MAMEM_HTTP_PORT ?? 8080}`

// drift: the receipt commits an exact answer text — if the reply doesn't
// contain it, the receipt doesn't cover what the user was actually shown
const results = []
for (const t of tokens) {
  try {
    const r = await fetch(`${base}/v1/verify?r=${t}`, { signal: AbortSignal.timeout(3000) })
    const j = await r.json()
    const fails = (j.checks ?? []).filter(c => c.ok === false).map(c => c.name)
    const drift = !!j.receipt?.answer && !norm(text).includes(norm(j.receipt.answer))
    if (drift) fails.push('answer-drift')
    results.push({ token: t, ok: j.ok === true && !drift, failNames: fails })
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
  const names = [...new Set(failed.flatMap(f => f.failNames))]
  process.stdout.write(JSON.stringify({
    decision: 'block',
    reason: `mamem receipt verification FAILED — checks that failed: ${names.join(', ')}. Tell the user the receipt did not verify (which check failed and why that matters); do NOT claim the answer is verified.${names.includes('answer-drift') ? ' answer-drift means the printed answer is not the committed receipt text — the receipt does not cover what the user was shown.' : ''}`
  }) + '\n')
}
process.exit(0)
