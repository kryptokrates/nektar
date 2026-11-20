import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, readdirSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { WAITS } from '../src/waits.js'

// waits-census — the wait-site inventory made executable. wake-coverage
// enumerates the JOBS (every service/cadence names its wake source); this
// enumerates the WAITS themselves: every wait primitive inside
// automation/src lives on the sanctioned list, every duration literal at
// a wait site is a named value (WAITS or an UPPER_SNAKE *_MS const), and
// every registered wait declares its class/durable/wake — a 'mechanism'
// class must also say WHY the wait itself is the trigger. The same
// enumerable-pattern the repo runs for causes, machine edges and oracle
// writes: what was smeared across literals is now a reviewable table,
// and a new anonymous wait fails here instead of drifting.

// tests run compiled (dist/test/*.js) — anchor on package.json so src/
// resolves to real sources, never dist/*.d.ts
const pkgRoot = join(fileURLToPath(new URL('.', import.meta.url)), '..', '..')
const srcDir = join(pkgRoot, 'src')
const read = (f: string): string => readFileSync(f, 'utf8')
const srcFiles = (dir = srcDir): string[] =>
  readdirSync(dir, { withFileTypes: true }).flatMap((e) => (e.isDirectory() ? srcFiles(join(dir, e.name)) : e.name.endsWith('.ts') ? [join(dir, e.name)] : []))

test('wait primitives only at sanctioned sites', () => {
  // setTimeout/setInterval/Promise.race are the wait primitives — every
  // callsite must be a named implementation (the wheel, the abortable nap,
  // the io-merge helper) or a sanctioned io bound; a new raw site is a
  // wait nobody classified. deadline()/nap() in @nektar/core are the one
  // race+sleep dialect — a fresh Promise.race re-forks it.
  const SANCTIONED: Record<string, string> = {
    'watches.ts': 'the abortable nap impl + the io-bound watch fetch',
    'lanes.ts': 'the io() merge helper — signals meet at the socket',
    'ai.ts': 'per-job io bound on caller-supplied timeoutMs',
    'toolbox-services.ts': 'rpc io bound (WAITS.rpcIo)',
    'exec.ts': 'rt/: LocalExecutor delay timer (test/dev executor) + ingress io bound (WAITS.ingressIo)',
    'endpoint.ts': 'rt/: deployment-registration io bound (WAITS.registerIo/registerRetry)'
  }
  for (const f of srcFiles()) {
    const rel = f.slice(srcDir.length + 1).replaceAll('\\', '/')
    const base = rel.split('/').pop()!
    const src = read(f)
    for (const m of src.matchAll(/setTimeout\(|setInterval\(|Promise\.race\(|AbortSignal\.timeout\(/g)) {
      const line = src.slice(0, m.index).split('\n').length
      assert.ok(
        SANCTIONED[base] !== undefined,
        `${rel}:${line} — raw wait primitive outside the sanctioned list (classify it here or route through deadline()/nap()/WAITS)`
      )
      break // one finding per file is enough — the allowlist is per-file
    }
  }
})

test('every wait-site duration literal is named', () => {
  // a raw \d+_000-shaped literal on a wait-context line must resolve
  // through WAITS.<name>.ms or an UPPER_SNAKE const — the literal is the
  // wait's un-enumerated form. market/types.ts is exempt by FILE: its
  // maxAgeMs/DEFAULTS tables ARE that domain's registry. cfg/env-derived
  // fields name themselves (NEKTAR_*_MS / cfg.fooMs) and carry no
  // literal at the use site.
  const WAIT_TOKEN =
    /timeout|Timeout|setTimeout|setInterval|race|deadline|Deadline|next_at|nextAt|dueIn|delay|Delay|backoff|attempt|attempts|ttl|Ttl|TTL|expir|Expir|until|Until|cooldown|held|Held|defer|Defer|interval|Interval|cadence|poll|Poll|retry|Retry|floor|rearm|throttle|Throttle|window|Window|since|Since|age|Age|sweep|Sweep|ceiling|Ceil|ceil|nap|sleep|ms\b/
  const LITERAL = /\d+_000\b|\b86_400_000\b|\b60_000\b/
  const EXEMPT_FILES = new Set(['waits.ts', 'market/types.ts', 'conformance.ts', 'policy.ts'])
  const NAMED = /WAITS\.\w+\.ms|const [A-Z][A-Z0-9_]*(_MS|Ms|_TTL)\b|exportedTtl|deliverBoundMs|timeoutMs|waitMs=|checkInMs|attempts\b/
  for (const f of srcFiles()) {
    const rel = f.slice(srcDir.length + 1).replaceAll('\\', '/')
    if (EXEMPT_FILES.has(rel)) continue
    const lines = read(f).split('\n')
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i]
      if (!LITERAL.test(line) || !WAIT_TOKEN.test(line)) continue
      // a named const's own declaration line registers the literal
      if (NAMED.test(line)) continue
      assert.fail(`${rel}:${i + 1} — unnamed wait-site duration literal: ${line.trim().slice(0, 100)}`)
    }
  }
})

test('the registry itself conforms — class, durable, wake; mechanism needs why', () => {
  for (const [name, w] of Object.entries(WAITS)) {
    assert.ok(['ceiling', 'ttl', 'mechanism', 'backoff', 'budget', 'io'].includes(w.class), `${name}: bad class`)
    assert.ok(typeof w.durable === 'boolean', `${name}: durable not declared`)
    assert.ok(typeof w.wake === 'string' && w.wake.length > 0, `${name}: wake not declared`)
    assert.ok(Number.isFinite(w.ms) && w.ms > 0, `${name}: ms must be a positive finite number`)
    if (w.class === 'mechanism')
      assert.ok(w.why !== undefined && w.why.length > 0, `${name}: mechanism class needs a why — what makes the wait itself the trigger`)
  }
})
