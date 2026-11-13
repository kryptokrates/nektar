#!/usr/bin/env node
// bench-real-stack.mjs — run the real-stack scenario N times sequentially and
// report median/p95/min/max per step + total. Each run gets its own
// .state/run-real-* dir; the scenario's treasury is the namespaced
// real-stack-treasury.json so benchmark runs can't race other consumers.
//
//   node packages/e2e/src/scenarios/bench-real-stack.mjs [N]
//   NEKTAR_BENCH_RUNS=N node packages/e2e/src/scenarios/bench-real-stack.mjs
//   NEKTAR_BENCH_LABEL=before node …   (tags the report file)
//
// Writes .state/bench-real-stack-<label>-<ts>.json and prints the table.

import { spawn } from 'node:child_process'
import { readFileSync, writeFileSync, readdirSync, statSync } from 'node:fs'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = join(dirname(fileURLToPath(import.meta.url)), '..', '..', '..', '..')
const scenario = join(dirname(fileURLToPath(import.meta.url)), 'real-stack.mjs')
const N = Number(process.argv[2] ?? process.env.NEKTAR_BENCH_RUNS ?? 10)
const LABEL = process.env.NEKTAR_BENCH_LABEL ?? 'bench'
const runDir = join(root, '.state')

const quant = (xs, q) => {
  if (xs.length === 0) return undefined
  const s = [...xs].sort((a, b) => a - b)
  const i = Math.min(s.length - 1, Math.max(0, Math.ceil(q * s.length) - 1))
  return s[i]
}
const stats = (xs) =>
  xs.length === 0
    ? undefined
    : { n: xs.length, min: quant(xs, 0), p50: quant(xs, 0.5), p95: quant(xs, 0.95), max: quant(xs, 1) }
const fmt = (v) => (v === undefined ? '—' : (v / 1000).toFixed(1))

const before = new Set(readdirSync(runDir).filter((d) => d.startsWith('run-real-')))
const results = []
for (let i = 0; i < N; i++) {
  console.log(`\n=== run ${i + 1}/${N} ===`)
  const t0 = Date.now()
  const child = spawn(process.execPath, [scenario], { stdio: ['ignore', 'pipe', 'pipe'], env: process.env })
  let dirLine = ''
  child.stdout.on('data', (d) => {
    process.stdout.write(d)
    const m = String(d).match(/run directory: (.+)/)
    if (m) dirLine = m[1].trim()
  })
  child.stderr.on('data', (d) => process.stderr.write(d))
  const code = await new Promise((r) => child.on('exit', r))
  let summary
  try {
    const dir = dirLine || [...readdirSync(runDir).filter((d) => d.startsWith('run-real-') && !before.has(d))].sort((a, b) => statSync(join(runDir, b)).mtimeMs - statSync(join(runDir, a)).mtimeMs)[0]
    summary = JSON.parse(readFileSync(join(dir, 'summary.json'), 'utf8'))
  } catch (e) {
    console.log(`  no summary.json (${e.message})`)
  }
  results.push({ i: i + 1, code, wallMs: summary?.wallClockMsToDisclosureVerified, outcome: summary?.outcome ?? (code === 0 ? 'PASS' : 'FAIL'), steps: summary?.steps ?? [], hopMs: summary?.hopMs ?? {}, runId: summary?.runId })
}

// aggregate: per-step t(s) across passing runs + totals + hops
const stepNames = [...new Set(results.flatMap((r) => (r.steps ?? []).slice(1).map((s) => s[0])))]
const pass = results.filter((r) => r.outcome === 'PASS')
const fail = results.filter((r) => r.outcome !== 'PASS')
const perStep = {}
for (const name of stepNames) {
  const xs = pass.map((r) => r.steps.slice(1).find((s) => s[0] === name)?.[1]).filter((v) => v !== undefined && v !== '—').map((v) => Number(v) * 1000)
  perStep[name] = stats(xs)
}
const hops = {}
for (const k of [...new Set(results.flatMap((r) => Object.keys(r.hopMs ?? {})))]) hops[k] = stats(pass.map((r) => r.hopMs[k]).filter((v) => v !== undefined))
const total = stats(pass.map((r) => r.wallMs).filter((v) => v !== undefined))

console.log(`\n================ ${LABEL} — ${pass.length} PASS / ${fail.length} FAIL of ${N} ================`)
console.log(`${'step'.padEnd(34)} ${'p50'.padStart(6)} ${'p95'.padStart(6)} ${'min'.padStart(6)} ${'max'.padStart(6)}`)
for (const name of stepNames) {
  const s = perStep[name]
  console.log(`${name.padEnd(34)} ${fmt(s?.p50).padStart(6)} ${fmt(s?.p95).padStart(6)} ${fmt(s?.min).padStart(6)} ${fmt(s?.max).padStart(6)}`)
}
for (const [k, s] of Object.entries(hops)) console.log(`${(`hop: ${k}`).padEnd(34)} ${fmt(s?.p50).padStart(6)} ${fmt(s?.p95).padStart(6)} ${fmt(s?.min).padStart(6)} ${fmt(s?.max).padStart(6)}`)
console.log(`${'TOTAL want→disclosure'.padEnd(34)} ${fmt(total?.p50).padStart(6)} ${fmt(total?.p95).padStart(6)} ${fmt(total?.min).padStart(6)} ${fmt(total?.max).padStart(6)}`)
if (fail.length > 0) console.log(`failed runs: ${fail.map((r) => `${r.runId ?? `#${r.i}`}(exit ${r.code})`).join(', ')}`)

const report = join(runDir, `bench-real-stack-${LABEL}-${new Date().toISOString().replace(/[:.]/g, '-')}.json`)
writeFileSync(report, JSON.stringify({ label: LABEL, at: new Date().toISOString(), runs: results.length, pass: pass.length, fail: fail.length, perStep, hops, total, results }, null, 1))
console.log(`report: ${report}`)
