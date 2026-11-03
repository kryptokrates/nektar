#!/usr/bin/env node
// Codex `notify` adapter — the Codex-side analog of the Claude Code Stop hook.
// Codex calls `notify = [...]` with the turn payload as the LAST argv:
//   {"type":"agent-turn-complete","turn-id":"…","last-assistant-message":"…"}
//
// What it does:
//   1. OPTIONAL CHAIN — `--chain <exe> <args…>` first invokes your existing
//      notify command with the same payload (we don't clobber what you had)
//   2. finds `mamemr1.` tokens in `last-assistant-message`
//   3. GETs /v1/verify for each (3s, max 3, fail-open)
//   4. appends a line to `codex-receipts.log` in the cwd — `tail -f` it, or
//      set MAMEM_NOTIFY_CMD to any command that shows the line (e.g. a toast)
//
// HONEST BOUNDARY: codex notify is fire-and-forget — it can FLAG a failed
// receipt but cannot block the turn the way the Claude Code hook can. The
// in-session check (the agent calling verifyReceipt itself) is the visible
// one here; this is the out-of-band tripwire.
import { spawn } from 'node:child_process'
import { appendFileSync } from 'node:fs'

const argv = process.argv.slice(2)

// 0. optional chain: everything between --chain and the payload runs first
const chainIdx = argv.indexOf('--chain')
const payload = argv.at(-1)
const chainArgs = chainIdx >= 0 ? argv.slice(chainIdx + 1, -1) : []
if (chainArgs.length) {
  try { await new Promise(res => { const c = spawn(chainArgs[0], [...chainArgs.slice(1), payload], { stdio: 'inherit' }); c.on('close', res); c.on('error', res) }) } catch {}
}

// 1. extract the last assistant message from the payload
let text = ''
try {
  const j = JSON.parse(payload ?? '{}')
  text = j['last-assistant-message'] ?? j.lastAssistantMessage ?? ''
} catch { text = payload ?? '' } // some setups may pass raw text
const tokens = [...new Set(text.match(/mamemr1\.[A-Za-z0-9_-]{40,}/g) ?? [])].slice(0, 3)
if (!tokens.length) process.exit(0)

// verify host: env override > the 'verify:' URL in the message > localhost
let base = process.env.MAMEM_VERIFY_BASE
if (!base) {
  try { base = new URL(text.match(/verify:\s*(https?:\/\/\S+)/i)?.[1] ?? '').origin } catch {}
}
base ??= `http://localhost:${process.env.MAMEM_HTTP_PORT ?? 8080}`
const norm = s => (s ?? '').toLowerCase().replace(/\s+/g, ' ').trim()
const lines = []
for (const t of tokens) {
  try {
    const r = await fetch(`${base}/v1/verify?r=${t}`, { signal: AbortSignal.timeout(3000) })
    const j = await r.json()
    const fails = (j.checks ?? []).filter(c => c.ok === false).map(c => c.name)
    // drift: committed answer text must appear in the assistant's reply,
    // else the receipt doesn't cover what the user was shown
    if (j.receipt?.answer && !norm(text).includes(norm(j.receipt.answer))) fails.push('answer-drift')
    const bad = fails.length > 0 || j.ok === false
    lines.push(`${bad ? '✗ FAILED' : '✓ VERIFIED'} ${t.slice(0, 24)}…${fails.length ? ' — ' + fails.join(', ') : ''}`)
  } catch (e) {
    lines.push(`? UNCHECKED ${t.slice(0, 24)}… — verify endpoint unreachable (${e.message})`)
  }
}
const stamp = new Date().toISOString()
try { appendFileSync('codex-receipts.log', lines.map(l => `${stamp} mamem ${l}`).join('\n') + '\n') } catch {}
const out = lines.join('\n')
process.stderr.write(`mamem ${out}\n`)
if (process.env.MAMEM_NOTIFY_CMD) {
  try { spawn(process.env.MAMEM_NOTIFY_CMD, [out], { stdio: 'ignore', shell: true }) } catch {}
}
process.exit(0)
