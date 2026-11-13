// preflight.ts — `automation.preflight`: the readiness gate for the market
// agent. `mem.doctor()` checks memory-side health; this surface checks the
// dependencies a market agent actually dies on, in dependency order:
//
//   chain        tip() answers; height not regressed vs the cached probe
//   broadcaster  the tx-status endpoint answers a read-only probe
//   hosts        ls_finger (+ ls_bbox when an office is configured) answers on
//                each configured host — read-only: no sats, no host noise,
//                and only ever on the preflight tick
//   store        one known object fetches back through fetchObjectByRef
//   payments     LedgerReceiver opens and payments.jsonl is writable
//   policy       an active policy exists for this network + role
//   funds        reserve ≥ one deal + fee runway
//   watch        in-flight wallet txids still resolve a chain state
//   spendguard   an over-budget spend is REFUSED (proof the guard is live,
//                not configured) + BudgetAuthority rejects over-cap
//   killswitch   the operator stop flag is not set (fail-closed)
//   conformance  NEKTAR_UNATTENDED needs a valid operator-signed token
//
// Every check returns {check, pass, detail, fix} — doctor's shape — and the
// report's verdict is deterministic: pass = every check green.
import { nektarEnv } from '@nektar/core'
import { Transaction, P2PKH, PrivateKey, UnlockingScript } from '@bsv/sdk'
import { mkdirSync, readFileSync, writeFileSync, accessSync, constants } from 'node:fs'
import { join } from 'node:path'
import type { AutomationHost } from './host.js'
import type { AutomationStore } from './store.js'
import { killSwitchActive } from './kill.js'
import { conformanceStatus } from './conformance.js'
import { fetchObjectByRef } from '@nektar/store'
import type { S3ObjectStore } from '@nektar/store'

// PreflightCheck/PreflightReport live in types.ts — the shared type barrel —
// so the barrel never has to reach back into this module (type-only cycle).
import type { PreflightCheck, PreflightReport } from './types.js'
export type { PreflightReport }

interface PreflightCfg {
  network: string
  dataDir?: string
  marketHosts: string[]
  boxOffice?: string
  /** NEKTAR_UNATTENDED=1 — makes the conformance token check required */
  unattended?: boolean
  now?: () => number
}

export interface PreflightDeps {
  mem: AutomationHost
  store?: AutomationStore
  objectStore?: S3ObjectStore
  cfg: PreflightCfg
  /** per-dependency ceilings (D1 budgets): host ≤5s, chain ≤3s, store ≤10s */
  ceilings?: { hostMs?: number; chainMs?: number; storeMs?: number }
  /** stronger liveness probes the transport can supply when it knows the
   *  endpoints (a raw HTTP answer proves the socket; txState alone can't
   *  distinguish a dead endpoint vs an unrecognized txid) */
  probes?: { broadcaster?: () => Promise<boolean> }
  /** which checks to run — the watchdog re-runs only the cheap probe tier */
  only?: string[]
}

const CHEAP = new Set(['chain', 'broadcaster', 'hosts', 'killswitch'])
/** the checks the watchdog re-runs on its slow tick — read-only, no spend */
export const WATCHDOG_CHECKS = CHEAP

const ZERO = '00'.repeat(32)

const bounded = async <T>(work: Promise<T>, ms: number, what: string): Promise<T> =>
  await Promise.race([work, new Promise<never>((_, rej) => setTimeout(() => rej(new Error(`${what} timed out (> ${ms}ms)`)), ms))])

export async function runPreflight(deps: PreflightDeps): Promise<PreflightReport> {
  const { mem, store, cfg } = deps
  const now = cfg.now?.() ?? Date.now()
  const ceil = { hostMs: deps.ceilings?.hostMs ?? 5_000, chainMs: deps.ceilings?.chainMs ?? 3_000, storeMs: deps.ceilings?.storeMs ?? 10_000 }
  const wanted = deps.only === undefined ? undefined : new Set(deps.only)
  const checks: PreflightCheck[] = []
  // the checks are independent — run them together inside one Promise.all;
  // each is itself bounded by the per-dependency ceiling
  const pending: Promise<void>[] = []
  const push = (name: string, fn: () => Promise<PreflightCheck>): void => {
    if (wanted !== undefined && !wanted.has(name)) return
    pending.push(
      (async () => {
        try {
          return await fn()
        } catch (e) {
          return { check: name, pass: false, detail: `check threw: ${(e as Error).message.slice(0, 160)}` }
        }
      })().then((c) => {
        checks.push(c)
      })
    )
  }

  // A1 — chain: tip answers and height has not regressed vs the cached probe
  push('chain', async () => {
    const height = await bounded(mem.chainHeight(), ceil.chainMs, 'chain height')
    let prev: number | undefined
    const stateFile = cfg.dataDir === undefined ? undefined : join(cfg.dataDir, 'preflight-state.json')
    if (stateFile !== undefined) {
      try {
        prev = (JSON.parse(readFileSync(stateFile, 'utf8')) as { chainHeight?: number }).chainHeight
      } catch {
        /* first probe */
      }
      try {
        mkdirSync(cfg.dataDir!, { recursive: true })
        writeFileSync(stateFile, JSON.stringify({ chainHeight: height, at: now }))
      } catch {
        /* cache is advisory */
      }
    }
    if (prev !== undefined && height < prev)
      return {
        check: 'chain',
        pass: false,
        detail: `height ${height} regressed below cached ${prev}`,
        fix: 'check the configured RPC/tracker — the chain view moved backwards'
      }
    return {
      check: 'chain',
      pass: true,
      detail: `height ${height}${prev === undefined ? ' (first probe)' : height > prev ? ` advancing (+${height - prev})` : ' unchanged since last probe'}`
    }
  })

  // A2 — broadcaster: a read-only tx-status probe must ANSWER (any verdict
  // counts; a transport error does not). Stronger endpoint probes win when
  // the transport supplied one.
  push('broadcaster', async () => {
    if (deps.probes?.broadcaster !== undefined) {
      const ok = await bounded(deps.probes.broadcaster(), ceil.chainMs, 'broadcaster probe')
      return ok
        ? { check: 'broadcaster', pass: true, detail: 'broadcast endpoint answered the probe' }
        : {
            check: 'broadcaster',
            pass: false,
            detail: 'broadcast endpoint did not answer',
            fix: 'broadcasts will black-hole — check the propagation/broadcaster URL'
          }
    }
    const st = await bounded(mem.txState(ZERO), ceil.chainMs, 'txState probe')
    return {
      check: 'broadcaster',
      pass: true,
      detail: `chain view answered the tx probe ('${st}') — no stronger broadcaster endpoint configured`
    }
  })

  // A3 — hosts: ls_nektar answers on each configured host — it is the carrier
  // service deals actually use, so any failure there (transport or service)
  // means the host cannot carry want/offer envelopes → genuinely 'down'.
  // ls_finger (+ ls_bbox when an office is configured) are optional features:
  // their failures degrade the report but never mark a host down. All probes
  // are read-only lookups — no sats, no host noise.
  const downHosts: string[] = []
  push('hosts', async () => {
    if (cfg.marketHosts.length === 0)
      return { check: 'hosts', pass: false, detail: 'no market hosts configured', fix: 'set NEKTAR_MARKET_HOSTS (or let bootstrap resolve them)' }
    const { lookupAll } = await import('@nektar/bstack')
    const me = mem.operatorKey().toPublicKey().toString().toLowerCase()
    const [carriers, fingers, boxes] = await Promise.all([
      lookupAll(cfg.marketHosts, 'ls_nektar', { wants: { net: cfg.network, domain: '_preflight', limit: 1 } }, { timeoutMs: ceil.hostMs }),
      lookupAll(cfg.marketHosts, 'ls_finger', { identityKey: me }, { timeoutMs: ceil.hostMs }),
      cfg.boxOffice !== undefined
        ? lookupAll(cfg.marketHosts, 'ls_bbox', { office: cfg.boxOffice, to: me }, { timeoutMs: ceil.hostMs })
        : Promise.resolve({ answers: [] })
    ])
    const errs: string[] = []
    const notes: string[] = []
    for (const a of carriers.answers) {
      if ('error' in a || !('outputs' in a)) {
        downHosts.push(a.host)
        errs.push(`${a.host} (${'error' in a ? a.error : 'no answer'})`)
        continue
      }
      const f = fingers.answers.find((x) => x.host === a.host)
      if (f !== undefined && ('error' in f || !('outputs' in f))) notes.push(`${a.host} ls_finger (${'error' in f ? f.error : 'no answer'})`)
      const b = boxes.answers.find((x) => x.host === a.host)
      if (b !== undefined && ('error' in b || !('outputs' in b))) notes.push(`${a.host} ls_bbox (${'error' in b ? b.error : 'no answer'})`)
    }
    return errs.length === 0
      ? {
          check: 'hosts',
          pass: true,
          detail:
            `${cfg.marketHosts.length} host(s) answered ls_nektar${cfg.boxOffice !== undefined ? ' + ls_bbox' : ''}` +
            (notes.length === 0 ? '' : ` — ${notes.join('; ')}`)
        }
      : {
          check: 'hosts',
          pass: false,
          detail: `${downHosts.length}/${cfg.marketHosts.length} host(s) down: ${errs.join('; ')}`,
          fix: 'deals cannot communicate — restore the hosts or update NEKTAR_MARKET_HOSTS'
        }
  })

  // A4 — store read-back: one known object must resolve through the shared
  // ref chain (chirp → unsigned chirp → keyed signed → keyed unsigned).
  push('store', async () => {
    const st = deps.objectStore ?? mem.automation?.objectStore
    const refs: Array<{ ref: Record<string, unknown>; src: string }> = []
    if (store !== undefined)
      for (const op of await store.operations({ states: ['disclosure_verified', 'useful'] }))
        if (op.data.evidenceRef !== undefined) refs.push({ ref: op.data.evidenceRef as Record<string, unknown>, src: `evidence ${op.operationId.slice(0, 8)}` })
    for (const s of mem.shards?.() ?? []) {
      const r = mem.packStoreRef?.(s)
      if (r !== undefined) refs.push({ ref: r as unknown as Record<string, unknown>, src: `pack ${s}` })
    }
    if (refs.length === 0) return { check: 'store', pass: true, detail: 'no stored objects yet — nothing to fetch back' }
    for (const { ref, src } of refs.slice(0, 3))
      try {
        const bytes = await bounded(fetchObjectByRef(st, ref), ceil.storeMs, 'object fetch')
        if (bytes.length === 0) throw new Error('empty object')
        return { check: 'store', pass: true, detail: `read-back ok (${src}, ${bytes.length} bytes)` }
      } catch {
        /* try the next known ref */
      }
    return {
      check: 'store',
      pass: false,
      detail: `${refs.length} stored ref(s) — none fetched back`,
      fix: 'buyers cannot retrieve evidence — check NEKTAR_BSTORE_* / the store endpoint'
    }
  })

  // A5 — payments: the ledger must open and its file must be writable when
  // payments are enabled; off = nothing to drop, reported as on.
  push('payments', async () => {
    const on = /^(1|true|yes)$/i.test(nektarEnv('NEKTAR_PAYMENTS') ?? '')
    if (!on) return { check: 'payments', pass: true, detail: 'payments off (NEKTAR_PAYMENTS unset)' }
    const dir = cfg.dataDir ?? nektarEnv('NEKTAR_DATA_DIR') ?? join(process.cwd(), '.nektar')
    try {
      mkdirSync(dir, { recursive: true })
      accessSync(dir, constants.W_OK)
      const { LedgerReceiver } = await import('@lightwebinc/bcommon/host')
      const r = new LedgerReceiver(dir)
      r.close()
      return { check: 'payments', pass: true, detail: `payments.jsonl opens writable in ${dir}` }
    } catch (e) {
      return {
        check: 'payments',
        pass: false,
        detail: `payment ledger unusable: ${(e as Error).message.slice(0, 120)}`,
        fix: 'payments would be silently dropped — fix the data dir'
      }
    }
  })

  // A6 — policy: an active policy must exist for this network and one of the
  // agent's roles, or publishWant/enableSeller throw on first use.
  push('policy', async () => {
    if (store === undefined)
      return { check: 'policy', pass: false, detail: 'automation store not open', fix: 'the automation runtime must open before preflight runs' }
    const me = mem.operatorKey().toPublicKey().toString()
    const live: string[] = []
    for (const row of await store.policies()) {
      if (row.revokedAt !== undefined) continue
      const p = await store.activePolicy(row.policyHash)
      if (p === undefined || p.network !== cfg.network || p.agentPub !== me) continue
      live.push(`${p.roles.join('+')} ${row.policyHash.slice(0, 12)}`)
    }
    return live.length > 0
      ? { check: 'policy', pass: true, detail: `${live.length} active policy for ${cfg.network}: ${live.join(', ')}` }
      : { check: 'policy', pass: false, detail: `no active policy for ${cfg.network} + this agent`, fix: 'approve a policy first (pnpm automation policy ...)' }
  })

  // A7 — funds: the reserve must cover one deal's lock + fee runway.
  push('funds', async () => {
    const f = await mem.fundingStatus()
    // one deal = the largest lock a policy can create (or a 1500-sat
    // stand-in) + the escrow fee reserve + ~500 sats of runway
    let need = 2000
    if (store !== undefined)
      for (const row of await store.policies()) {
        const p = row.revokedAt === undefined ? await store.activePolicy(row.policyHash) : undefined
        if (p !== undefined) need = Math.max(need, p.limits.maxLockedSats + p.limits.perTxFeeSats + 500)
      }
    return f.balanceSats >= need
      ? { check: 'funds', pass: true, detail: `${f.balanceSats} sats across ${f.utxos} utxo(s) — ≥ one deal + runway (${need})` }
      : { check: 'funds', pass: false, detail: `${f.balanceSats} sats — needs ≥${need} for one deal + fees`, fix: `fund ${f.address}` }
  })

  // A8 — watch liveness: in-flight wallet txids must still resolve a chain
  // state — a watched map that never drains stalls refund automation quietly.
  push('watch', async () => {
    if (store === undefined) return { check: 'watch', pass: true, detail: 'no automation store — nothing in flight' }
    const inflight = await store.walletTxs(['submitted', 'submission_unknown', 'observed', 'exported'])
    if (inflight.length === 0) return { check: 'watch', pass: true, detail: 'nothing in flight — watcher idle' }
    const stale: string[] = []
    for (const w of inflight.slice(0, 4)) {
      const st = await bounded(mem.txState(w.txid), ceil.chainMs, 'txState').catch(() => 'timeout')
      if (st === 'unknown' || st === 'timeout') stale.push(`${w.txid.slice(0, 12)} (${w.state})`)
    }
    return stale.length === 0
      ? { check: 'watch', pass: true, detail: `${inflight.length} in-flight tx(s) all resolve a chain state` }
      : {
          check: 'watch',
          pass: false,
          detail: `${stale.length}/${inflight.length} in-flight tx(s) resolve no state: ${stale.join(', ')}`,
          fix: 'the broadcaster view is stale — refund automation may be stuck'
        }
  })

  // D2 — spendguard ACTIVE (not configured): an over-budget spend must be
  // refused by the guard itself. The dry-run spends a fabricated parent so a
  // bypassed or unlimited path can never cost real sats — the refusal has to
  // come from SpendGuard, the network from nothing.
  push('spendguard', async () => {
    const tmp = PrivateKey.fromRandom()
    const parent = new Transaction()
    parent.addOutput({ lockingScript: new P2PKH().lock(tmp.toPublicKey().toHash()), satoshis: 1_000_000 })
    const tx = new Transaction()
    tx.addInput({ sourceTXID: parent.id('hex'), sourceOutputIndex: 0, sourceTransaction: parent, unlockingScript: new UnlockingScript([{ op: 0 }]) })
    tx.addOutput({ lockingScript: new P2PKH().lock(tmp.toPublicKey().toHash()), satoshis: 546 })
    const r = await bounded(mem.broadcastTx(tx), ceil.chainMs * 2, 'spend dry-run')
    const desc = r.description ?? ''
    if (!/SpendGuard refused/i.test(desc))
      return {
        check: 'spendguard',
        pass: false,
        detail: `over-budget dry-run was not refused by the guard (${r.status}: ${desc.slice(0, 120) || 'no reason'})`,
        fix: 'the broadcast path is unguarded — wrap it in SpendGuard before allowing deals'
      }
    // BudgetAuthority: over-cap reservation must deny, then a legal one must
    // pass — 'denied' proves the cap branch, not just an unknown-grant error.
    let bdetail = 'no BudgetAuthority on this instance'
    if (mem.budget !== undefined) {
      mem.budget.setCap('__preflight_probe__', 1)
      const deny = mem.budget.reserve('__preflight_probe__', '__probe_over__', 2)
      const ok = mem.budget.reserve('__preflight_probe__', '__probe_ok__', 1)
      if (ok === 'ok') mem.budget.release('__preflight_probe__', '__probe_ok__')
      if (deny !== 'denied' || ok !== 'ok')
        return {
          check: 'spendguard',
          pass: false,
          detail: `BudgetAuthority misbehaves (deny=${deny}, ok=${ok})`,
          fix: 'the grant ledger must deny over-cap and admit within-cap'
        }
      bdetail = 'BudgetAuthority denies over-cap and admits within-cap'
    }
    return { check: 'spendguard', pass: true, detail: `guard refused the dry-run (${desc.slice(0, 100)}); ${bdetail}` }
  })

  // D2 — kill switch: report the current stop state (a stopped agent is not
  // safe to START; a running one passes).
  push('killswitch', async () => {
    const k = killSwitchActive(cfg.dataDir)
    return k.killed
      ? { check: 'killswitch', pass: false, detail: `stop flag set (${k.source}) — new deals blocked`, fix: 'remove the flag to resume' }
      : { check: 'killswitch', pass: true, detail: 'no stop flag' }
  })

  // A11 — conformance: only REQUIRED under NEKTAR_UNATTENDED; otherwise it
  // reports the token's state without gating.
  push('conformance', async () => {
    const { defaultSuiteDir } = await import('./conformance.js')
    const c = conformanceStatus({ dataDir: cfg.dataDir, network: cfg.network, marketHosts: cfg.marketHosts, suiteDir: defaultSuiteDir() })
    if (cfg.unattended === true)
      return { check: 'conformance', pass: c.pass, detail: c.detail, fix: c.pass ? undefined : 'run pnpm scenarios:verify to (re)issue the token' }
    return { check: 'conformance', pass: true, detail: `unattended not requested — token: ${c.detail}` }
  })

  await Promise.all(pending)
  checks.sort(
    (a, b) =>
      ['chain', 'broadcaster', 'hosts', 'store', 'payments', 'policy', 'funds', 'watch', 'spendguard', 'killswitch', 'conformance'].indexOf(a.check) -
      ['chain', 'broadcaster', 'hosts', 'store', 'payments', 'policy', 'funds', 'watch', 'spendguard', 'killswitch', 'conformance'].indexOf(b.check)
  )
  return { pass: checks.every((c) => c.pass), at: now, checks, downHosts }
}
