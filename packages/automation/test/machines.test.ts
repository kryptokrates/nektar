import test from 'node:test'
import assert from 'node:assert/strict'
import { declaredTransitions, type AnyStateMachine, type TransitionTable } from '../src/machines/machine.js'
import { OPS_TABLE, opsMachine, opsStep } from '../src/machines/ops.machine.js'
import { TX_TABLE, txMachine, txStep } from '../src/machines/tx.machine.js'
import { LEASE_TABLE, leaseMachine, leaseStep } from '../src/machines/lease.machine.js'
import { INBOX_TABLE, inboxMachine, inboxStep } from '../src/machines/inbox.machine.js'
import { OUTBOX_TABLE, outboxMachine, outboxStep } from '../src/machines/outbox.machine.js'
import { LANE_TABLE, laneMachine, laneStep } from '../src/machines/lane.machine.js'
import { SLOT_TABLE, slotMachine, slotStep } from '../src/machines/slot.machine.js'
import { LOCK_TABLE, lockMachine, lockStep } from '../src/machines/lock.machine.js'
import { HOST_TABLE, hostMachine, hostStep } from '../src/machines/host.machine.js'

function bfs(table: TransitionTable): Map<string, { via: string; from: string }> {
  const seen = new Map<string, { via: string; from: string }>()
  const q = [...(table.roots ?? [table.states[0]])]
  for (const r of q) seen.set(r, { via: 'root', from: '' })
  const edges = declaredTransitions(table)
  while (q.length) {
    const cur = q.shift()!
    for (const e of edges) {
      if (e.state !== cur || seen.has(e.target)) continue
      seen.set(e.target, { via: e.event, from: cur })
      q.push(e.target)
    }
  }
  return seen
}

function pathTo(table: TransitionTable, from: string, sinks: Set<string>): boolean {
  const seen = new Set([from])
  const q = [from]
  const edges = declaredTransitions(table)
  while (q.length) {
    const cur = q.shift()!
    if (sinks.has(cur)) return true
    for (const e of edges) {
      if (e.state !== cur || seen.has(e.target)) continue
      seen.add(e.target)
      q.push(e.target)
    }
  }
  return sinks.has(from)
}

function tableTests(
  name: string,
  machine: AnyStateMachine,
  table: TransitionTable,
  stepFn: (s: string, e: string, p?: Record<string, unknown>) => { next: string; accepted: boolean }
) {
  test(`${name}: every declared table row evaluates to its declared target`, () => {
    for (const row of table.transitions) {
      const rowSources = row.state === '*' ? table.states : ([] as string[]).concat(row.state)
      for (const src of rowSources) {
        // a specific row for (src,event) shadows the wildcard row — skip the shadowed expansion
        if (row.state === '*' && table.transitions.some((r) => r.state !== '*' && ([] as string[]).concat(r.state).includes(src) && r.event === row.event)) continue
        const want = row.target === 'self' ? src : row.target
        const r = stepFn(src, row.event, row.guard ? { guard: row.guard } : {})
        assert.equal(r.accepted, true, `${src}+${row.event} rejected`)
        assert.equal(r.next, want, `${src}+${row.event}(${row.guard ?? 'unguarded'}) → ${r.next}, want ${want}`)
      }
    }
  })

  test(`${name}: union is closed — every state is terminal | quiescent | parked | nonterminal`, () => {
    const classified = new Set([...table.terminal, ...(table.quiescent ?? []), ...(table.parked ?? []), ...(table.nonterminal ?? [])])
    for (const s of table.states) assert.ok(classified.has(s), `${s} unclassified`)
    for (const s of classified) assert.ok(table.states.includes(s), `${s} classified but not a state`)
  })

  test(`${name}: every state reachable from a root`, () => {
    const seen = bfs(table)
    for (const s of table.states) assert.ok(seen.has(s), `${s} unreachable`)
  })

  test(`${name}: every non-terminal has a path to terminal or quiescent`, () => {
    const sinks = new Set([...table.terminal, ...(table.quiescent ?? [])])
    for (const s of table.states) {
      if (sinks.has(s)) continue
      assert.ok(pathTo(table, s, sinks), `${s} has no path out`)
    }
  })

  test(`${name}: every recovery state is reachable from a real event`, () => {
    const seen = bfs(table)
    const incoming = new Set(declaredTransitions(table).map((t) => t.target))
    for (const s of [...(table.quiescent ?? []), 'refund_pending', 'funding_deferred', 'uncertain', 'escalated', 'abandoned', 'expired']) {
      if (!table.states.includes(s)) continue
      const reach = seen.get(s)
      // a root∩quiescent state (up, free) is an entry state — it must still
      // have a real incoming edge or nothing can ever return to it
      assert.ok(reach && (reach.via !== 'root' || incoming.has(s)), `${s} only reachable as a root`)
    }
  })

  test(`${name}: impossible transitions are rejected, never silently applied`, () => {
    void machine
    // every (state, event) pair NOT in the table must leave the state unchanged
    const declared = new Set(declaredTransitions(table).map((t) => `${t.state}|${t.event}`))
    const events = new Set(table.transitions.map((r) => r.event))
    for (const s of table.states) {
      for (const e of ['bogus.event', ...events]) {
        if (declared.has(`${s}|${e}`)) continue
        const r = stepFn(s, e)
        assert.equal(r.next, s, `${s}+${e} must not move`)
      }
    }
  })
}

// ---------- wallet_transactions ----------

tableTests('txMachine', txMachine, TX_TABLE, (s, e, p) => txStep(s, e, p))

test('txMachine: confirmed only reachable via proof-bearing event', () => {
  // 'confirmed' is never a declared target of an unverified event
  for (const row of TX_TABLE.transitions) {
    const target = row.target === 'self' ? row.state : row.target
    if (target === 'confirmed') assert.match(row.guard ?? '', /proof/, `unproven path to confirmed: ${row.state}+${row.event}`)
  }
  // and stepping without naming the satisfied guard is rejected — proof is caller-asserted
  assert.equal(txStep('prepared', 'obs.proven').accepted, false)
  assert.equal(txStep('submitted', 'obs.proven').accepted, false) // no proof asserted → rejected
  assert.equal(txStep('submitted', 'obs.proven', { guard: 'proof' }).next, 'confirmed')
})

test('txMachine: timeout-without-verdict goes to unknown/quiescent, never failed', () => {
  const r = txStep('submitted', 'abandon')
  assert.equal(r.next, 'abandoned') // absence ≠ conflicted, ≠ rejected
  assert.ok(r.intents.includes('tx-died'))
  // and submission_unknown never exits except on a verdict
  for (const e of ['bogus', 'tick.error', 'stuck.timeout']) {
    assert.equal(txStep('submission_unknown', e).next, 'submission_unknown')
  }
})

test('txMachine: crash/restart is identity — state lives in the row, not memory', () => {
  // "restart" = resolve the persisted value again; the pure function is total
  for (const s of TX_TABLE.states) {
    const snap = txMachine.resolveState({ value: s, context: {} })
    assert.equal(snap.value, s)
  }
  // duplicate + out-of-order delivery
  assert.equal(txStep('observed', 'obs.seen', { guard: 'verified' }).next, 'observed') // dup seen
  assert.equal(txStep('submitted', 'obs.proven', { guard: 'proof' }).next, 'confirmed') // seen skipped
  assert.equal(txStep('confirmed', 'obs.seen', { guard: 'verified' }).accepted, false) // late dup on terminal
  assert.equal(txStep('expired', 'obs.seen', { guard: 'verified' }).next, 'observed') // late landing revives
})

// ---------- market_operations ----------

tableTests('opsMachine', opsMachine, OPS_TABLE, (s, e, p) => opsStep(s, e, p))

test('opsMachine: disclosure_verified is parked — money settled, external actor only', () => {
  assert.equal(opsStep('disclosure_verified', 'marked.useful').next, 'useful')
  assert.equal(opsStep('disclosure_verified', 'tick.error').next, 'disclosure_verified') // label ≠ transition
  assert.equal(opsStep('disclosure_verified', 'stuck.timeout').accepted, false) // no bound — parked
})

test('opsMachine: revival edges are proof-gated', () => {
  assert.equal(opsStep('funding_failed', 'escrow.landed').accepted, false) // no verdict named
  assert.equal(opsStep('funding_failed', 'escrow.landed', { guard: 'verdict=mined' }).next, 'refund_pending')
  assert.equal(opsStep('refunded', 'refund.dead', { guard: 'verified' }).next, 'uncertain')
})

test('opsMachine: crash/restart identity + guarded siblings disambiguate', () => {
  assert.equal(opsStep('selected', 'cap.refused', { guard: 'policy.escalate' }).next, 'escalated')
  assert.equal(opsStep('selected', 'cap.refused', { guard: '!policy.escalate' }).next, 'denied')
})

// ---------- Phase D: small machines (spec + shadow period) ----------

tableTests('leaseMachine', leaseMachine, LEASE_TABLE, (s, e, p) => leaseStep(s, e, p))

test('leaseMachine: released→spent is declared — the counterparty-air edge', () => {
  // 3bb8271b: a lease released on our refusal still spent — the revive path
  assert.equal(leaseStep('released', 'spend', { guard: 'aired|proven' }).next, 'spent')
  assert.equal(leaseStep('leased', 'spend', { guard: 'aired|proven' }).next, 'spent')
  assert.equal(leaseStep('released', 'lease').next, 'leased') // a freed coin re-leases normally
  assert.equal(leaseStep('spent', 'lease').accepted, false) // consumed stays consumed
})

tableTests('inboxMachine', inboxMachine, INBOX_TABLE, (s, e, p) => inboxStep(s, e, p))

test('inboxMachine: park is parent-op-guarded — delivered under funding_deferred', () => {
  assert.equal(inboxStep('pending', 'park', { guard: 'op-state=funding_deferred' }).next, 'parked')
  assert.equal(inboxStep('pending', 'park').accepted, false) // no op state named → can't park
  assert.equal(inboxStep('parked', 'resume').next, 'pending')
  assert.equal(inboxStep('parked', 'process').next, 'done')
})

tableTests('outboxMachine', outboxMachine, OUTBOX_TABLE, (s, e, p) => outboxStep(s, e, p))

test('outboxMachine: deferred unblocks on its class only', () => {
  assert.equal(outboxStep('pending', 'defer').next, 'deferred')
  assert.equal(outboxStep('deferred', 'unblock', { guard: 'same-class' }).next, 'pending')
  assert.equal(outboxStep('deferred', 'unblock').accepted, false) // wrong class never wakes it
})

tableTests('laneMachine', laneMachine, LANE_TABLE, (s, e, p) => laneStep(s, e, p))

test('laneMachine: proven is quiescent not final — reorg demotes', () => {
  assert.equal(laneStep('proven', 'reorg', { guard: 'reheader' }).next, 'unproven')
  assert.equal(laneStep('unproven', 'prove', { guard: 'bump' }).next, 'proven')
  assert.equal(laneStep('unproven', 'prove').accepted, false) // no BUMP named → no proof
})

tableTests('slotMachine', slotMachine, SLOT_TABLE, (s, e, p) => slotStep(s, e, p))

test('slotMachine: a dead mint returns the leaf — local refusal is not consumption', () => {
  assert.equal(slotStep('taken', 'mint-died', { guard: 'verified' }).next, 'free')
  assert.equal(slotStep('taken', 'mint-aired', { guard: 'aired|proven' }).next, 'spent')
})

tableTests('lockMachine', lockMachine, LOCK_TABLE, (s, e, p) => lockStep(s, e, p))

test('lockMachine: takeover requires evidence — never a guess', () => {
  assert.equal(lockStep('stale', 'takeover', { guard: 'pid-dead|beat-dead' }).next, 'held')
  assert.equal(lockStep('stale', 'takeover').accepted, false)
  assert.equal(lockStep('held', 'stale', { guard: 'beat-gap' }).next, 'stale')
})

tableTests('hostMachine', hostMachine, HOST_TABLE, (s, e, p) => hostStep(s, e, p))

test('hostMachine: a single miss is a suspect, never a verdict', () => {
  assert.equal(hostStep('up', 'miss').next, 'suspect')
  assert.equal(hostStep('suspect', 'ok').next, 'up')
  assert.equal(hostStep('suspect', 'miss', { guard: 'repeat' }).next, 'down')
  assert.equal(hostStep('up', 'deaf', { guard: 'answered-others' }).next, 'deaf')
})
