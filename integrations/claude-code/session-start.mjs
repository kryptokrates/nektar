#!/usr/bin/env node
// SessionStart hook — injects one line of memory context so the agent knows
// verified memory is available before it answers anything. Reads the running
// instance's `doctor` tool over HTTP; exits silently when the instance isn't
// up (hook output must never block or error a session open).
//
// Env it honours (same as the server): NEKTAR_HTTP_PORT (default 8080),
// NEKTAR_HTTP_TOKEN (bearer, only needed when the server binds publicly).
const port = process.env.NEKTAR_HTTP_PORT ?? '8080'
const headers = { 'content-type': 'application/json' }
if (process.env.NEKTAR_HTTP_TOKEN) headers.authorization = `Bearer ${process.env.NEKTAR_HTTP_TOKEN}`
try {
  const r = await fetch(`http://127.0.0.1:${port}/v1/tools/doctor`, {
    method: 'POST', headers, body: '{}', signal: AbortSignal.timeout(2_000)
  })
  if (!r.ok) process.exit(0)
  const d = await r.json()
  const f = d.funding ?? {}
  console.log(
    `nektar ready: ${d.records} record(s) on ${d.shards} shard(s), ` +
    `${d.anchoredRecords} anchored (${d.network}) — recall first, then route ` +
    `answers through groundedAnswer; keep the receipt footer it returns. ` +
    `Fee key ${f.address ?? 'unfunded'} ${f.balanceSats ?? 0} sats · prover ${d.prover}`
  )
} catch { process.exit(0) }
