---
name: state-audit
description: >
  Use when auditing the state, lifecycle, or recovery surface of a resource —
  an op, an outpoint, a carrier, a lease, a row — or a diff that adds states,
  transitions, or recovery code. Trigger surfaces: "who owns this failure",
  stale status labels, two components disagreeing about the same fact,
  restart-durability questions, rollback/retry/reconcile correctness, or a
  timeout being treated as a verdict. The deliverable is a machine-shaped
  transition table (state × event → guard / effect / targets / owner) plus
  ranked findings. For patch-level review use code-review; for whole-system
  runtime topology use arch-review; for wait-site/event design use
  event-arch; for what counts as believed use spv-strict; for the coin path
  use bsv; for test purity use test-boundaries.
---

# state-audit — the lifecycle is the unit

State machines rot in the **gaps** — the transitions nobody wrote. The bug
classes this skill hunts all live in those gaps: an expected outcome recorded
before it's confirmed with nothing that reconciles reality; two stores
holding the same fact with no named winner; a recovery path that exists but
never fires; a label that outlives the ledger it was derived from. The
method: pick a resource, draw its transition table, and let the empty cells
show you where it breaks.

The audit's product is a **transition table implementable as a statechart** —
no library assumed. Gaps become findings; the table itself becomes the diff
baseline if the resource later gets a declared machine.

## Scope

- `--resource <name>` — one stateful object (an op, an outpoint, a tree, a
  lease). Default deliverable: its full table.
- `--subsystem <path>` — every resource the code under the path owns.
- `--failure-class <name>` — hunt one class across resources (e.g.
  "unowned failure modes", "late-landing transactions").
- `--files` / `--pkg` / `--last [n]` — diff scope, same flags as
  design-guardrails. Audit the states/transitions the diff adds or mutates.

## The six phases

### 1. Map state & ownership

For every fact the resource carries, name:

- **the arbiter** — the one store whose write wins. In nektar the chain
  arbitrates money facts; local stores hold intent (cite `data-systems.md`
  sources-of-truth table, don't re-derive it).
- **the projections** — caches, labels, denormalized columns, external
  systems representing the same fact.
- **the writers** — every code path that can move the state.
- **the disagreement rule** — what runs when arbiter and projection differ,
  and which side it trusts. `data/hidden-source-of-truth` is the finding ID
  when nothing names the winner.

Separate projections are legal — multiple databases are not the bug. The bug
is an *unnamed* authority or a projection that silently became one. The
strongest fix often **deletes a stored truth**: compute the label from the
ledger as a pure selector instead of reconciling two writes (the
`deals`-label vs `walletTxs` pattern in `references/nektar.md`).

### 2. Trace the lifecycle as a transition table

One row per `(state, event)` — creation, reservation, submission,
acceptance, confirmation, failure, cancellation, recovery, cleanup:

| state | event | event source (who may emit) | guard (required evidence) | action/effect | target on success | target on failure / timeout | owner |
| ----- | ----- | --------------------------- | ------------------------- | ------------- | ----------------- | --------------------------- | ----- |

- An **empty cell is a finding** — a state with no row for `timeout`,
  `reject`, or `late-confirm` is an unmodelled reality.
- **Event source is an authority question.** Who may emit `confirmed`? If a
  status word, a broadcaster hint, or the local write that *sent* the thing
  can emit it, that's `spv/hint-as-verified` / `nektar/write-side-verify` —
  load spv-strict for the guard, don't re-derive it.
- **Outcome taxonomy**: expected / submitted / accepted / confirmed /
  failed / **unknown**. A timeout or lost response is `unknown`, never a
  verdict — `submission_unknown` in `automation/src/store.ts` is the
  repo's canonical example (`ledger.ts` sets it on broadcast throw).

### 3. Failure & recovery ownership

For each consequential failure mode: **who detects, who resolves, what is
the bound.** Then:

- Retries: safe (idempotent — same key, same result), bounded (attempts,
  window), and classified (`nektar/refusal-classified` — a defer-window is
  not a refusal).
- Adversarial timeline: crash between persist→act and act→record
  (`data/durability-point` drill), concurrent operations, stale locks,
  duplicate delivery (`data/at-least-once-victim`), partial writes,
  out-of-order events (`data/ordering-scope`).
- Restart inventory per structure: **persists / rebuilt from source /
  deliberately discarded** — and for each discard, the consequence. F12 in
  arch-review's failure-modes.md is the inventory format; this phase adds
  "who reconciles the divergence after boot."

### 4. Dormant paths

A recovery path is dormant when it exists but can't fire: unreachable from
production entry points, gated off by config, ordered after an exit that
always wins, or untested under realistic conditions.

- Trace from **production entry points through real callers** — dispatch
  tables, timers, replay loops, event handlers, MCP tool surfaces. Never
  declare dead code from a missing text-search hit; `data/dormant-path`.
- Reachable ≠ tested: check the test exercises the *observable outcome*,
  not just the function's name, and under conditions that occur (real
  stores, real refusal shapes — test-boundaries owns fixture purity).
- Config/flags/feature gates count as callers — or as proof of dormancy.

### 5. Boundaries & representations

- Contract vs actual: what the producer declares, what it returns, what
  consumers read — three different things. `data/schema-drift` when the
  persisted shape outlives the writer.
- Limits on the **constrained artifact**: the bound lives where the receiver
  enforces it — post-serialization, post-encryption, post-compression.
  `nektar/envelope-bound` + `nektar/sealed-budget` are the canonical case;
  generalize: a fee cap checked pre-fee, a size check on plaintext.
- Failure types distinguished where recovery differs — same rule as
  `nektar/refusal-classified`, applied at every boundary not just retries.
- Policy/config authority: who can change a limit, where it's declared, and
  whether a caller can discover+grant it. An env var only the operator knows
  is hidden authority — the friction skill's nektar-map calls this out.

### 6. Verification design

For each finding, the smallest test that would have caught it — crash
mid-transition, uncertain outcome, duplicate event, out-of-order event,
restart, refused-but-landed. Fixtures must obey domain invariants
(test-boundaries T4/T7); a fixture violating custody proves nothing. Confirm
the suite runs what you edited — mcp tests run `dist/`, rebuild first.

**Model-based layer** (once a resource has a declared machine): enumerate
states/paths with a graph tool; assert every state reachable, every
non-terminal has a path to terminal, every recovery state reachable from a
real event. Keep ≥1 real crash/restart test — graphs can't prove the
integration emits the right events.

## Three structural checks (run on every table)

1. **Terminal-with-late-evidence.** For each terminal state, name the late
   evidence that could contradict it and the revival edge. A `refused` or
   `failed` terminal with no "landed anyway" edge is a modelling bug — the
   class `refund_pending` was added to fix.
2. **Unknown is a state.** `submission_unknown`-class states exit only via a
   verification event (chain lookup, verdict). Timeout moves *into* unknown,
   never into `failed`.
3. **Exhaustiveness.** Every non-terminal state handles every failure event
   or records why it can't occur. Cheap once the table exists.

## Report format

Per audited resource: **the transition table**, then findings in priority
order:

```
### <finding title>
invariant: <what should hold>
evidence:  file:line + the call path that reaches it
sequence:  <the concrete interleaving that breaks it>
impact:    <blast radius> — severity: BLOCKER | HIGH | MED
owner:     <who detects/resolves today, or the gap>
fix:       <correction> + <test that fails before it>
status:    confirmed | plausible | lead
```

Footer (mandatory): components reviewed, coverage limits, assumptions,
tests actually run. Never claim a path is fixed or safe without executed
evidence (`nektar/fake-evidence` applies to audit output too).

## Sibling skills — load, don't re-derive

| Ask                                   | Skill                                             |
| ------------------------------------- | ------------------------------------------------- |
| Rule IDs for a finding                | code-review/references (`data/*`, `nektar/*`)     |
| Whole-system runtime topology         | arch-review (F-catalog)                           |
| Wait sites, pushes vs polls           | event-arch                                        |
| What counts as believed (proof truth) | spv-strict — event-source guards plug in here     |
| Broadcast/status/coin mechanics       | bsv                                               |
| Test-file purity, fixture invariants  | test-boundaries                                   |
| Module-shape smells found en route    | design-guardrails                                 |

## Sources

Kleppmann (authority, reconcile, dedup — via `data-systems.md`), Nygard
(failure-mode ownership), Cook (complex systems fail at boundaries), the
statechart tradition for the table shape, and nektar's incident ledger —
`references/nektar.md` maps each incident class to the phase that hunts it.

Detailed procedures: [references/audit.md](references/audit.md) · repo audit
surface: [references/nektar.md](references/nektar.md) · implementation
contract: [references/statecharts.md](references/statecharts.md)
