# test-boundaries — rule catalog with worked examples

Severity legend: **BLOCKER** = the test can certify a falsehood or hide a
regression the suite was built to catch. **HIGH** = drift surface — the
suite will diverge from production silently. **MED** = coupling/cost —
breaks on refactors, extends blast radius.

## T1 — No domain decisions in test files

Any code inside a test that answers "what is the right outcome" rather than
"did the system produce it": spend/escrow math, scoring and admission
checks, policy evaluation, state transition tables, quota/threshold logic.

```
BLOCKER — test computes escrow timeout itself, then asserts the engine met it.
Fix: engine exposes the decision; the test asserts the engine's answer.
```

The failure mode is subtle: prod changes 15s→30s, the test still computes
15s, and either the test goes red for a correct system (alarm fatigue →
somebody "fixes" the assert) or — worse — someone tunes prod to match the
test, and the test file has become the spec.

## T2 — No parallel implementations

A helper whose semantics must "stay in sync" with a src function is a
second codebase. Signatures: near-duplicate logic with different syntax,
comments like "same as production", a clone that evolved separately
(jscpd's type-2/3 clones — and type-4 convergent rewrites it can't see).

Worked example (real): a test-side UTXO reconcile that marked outpoints
spent when "the index doesn't serve the parent" while production's funds
book required positive spend evidence. The test's looser rule zeroed live
reserves; production's stricter one held them — the suite tested a funding
book that didn't exist in prod.

Fix: the logic is needed by both → it ships in src (the repo's
`testkit.ts`/`testtier.ts` convention is the pattern: test infra in src,
gated like src).

## T3 — Assembly through the composition root

Tests that wire real (non-fake) production objects by hand re-implement the
composition root. When the root gains a component — a second witness, a new
decorator — every hand-rolled stack silently keeps the old topology and the
suite passes on a system that doesn't exist.

```
HIGH — test builds new WitnessedTracker(new ChaintracksTracker(mc), new IndexTracker(mc))
while port.ts composes the same pair plus strict-mode options the test forgot.
Fix: tests call the port/composition entrypoint and inject fakes at the seams
it documents (ports, env, *For config).
```

Legitimate exception: the test is _testing the composition root itself_, or
deliberately isolating one unit with all-fake collaborators — then every
collaborator is a fake by construction and there's nothing to drift.

## T4 — Fixtures are data, not logic

Fixtures: literal strings/bytes, serialized objects, frozen expected
outputs, recorded wire traffic. Logic: anything that computes an input at
runtime — a policy object built by calling production code, an expected
value derived by running the algorithm under test.

```
HIGH — "expected" is computed by the function under test (golden-master
self-citation: the test can only ever pass).
Fix: freeze the expected output as a literal, with a comment naming the
version/inputs that produced it.
```

A policy literal is fine (it's data describing a scenario). A policy
_evaluated_ — limits arithmetic, precedence resolution — inside the test is
T1 in disguise.

## T5 — Harness = mechanics, not semantics

Drivers/waiters/schedulers are infrastructure: `until(cond)`, `drive(n)`,
`collectOffers(hosts)`, fake clocks. They may control _when_ calls happen;
they may not encode _what should happen_.

The boundary: a driver that loops "every 800ms for up to 15s" is mechanics.
A driver that implements "deals every pass, upkeep on a 10s ceiling because
the collect window is 15s" has encoded the engine's cadence policy —
production's `cycle()` already makes that decision; the test should call it
or the cadence should be configurable from prod, not re-declared in the
test.

## T6 — No probing production internals

Tests must observe through the same ports the system exposes — ops
interfaces, public queries, the event surface. Probing private persistence
(SQLite tables, journal files, internal schemas) couples the suite to
storage shape: a legitimate refactor (rename a table, move to Postgres)
turns the suite red without changing behavior — and a test that _gates on_
private state (e.g. "are there 8 free slots in bstack_trees") is asserting
implementation detail, not contract.

Exception: the private state _is_ the contract under test (migration tests,
store-level suites in the owning package's test dir).

## T7 — No test-side bookkeeping of shared truth

The money-path version of T2: reserve marks, spent ledgers, lease tables,
dedup maps, sequence counters — anything that tracks _which outpoints /
records / items are consumed_ — must never be re-derived in a test. A
parallel coin book is how double-spends and phantom reserves ship: the test
book says free, production's book says spent, and whichever one the
deployed path trusts is a coin flip.

This is a real incident class in this repo (UTXO_SPENT double-mint wedge,
stale-lease funding_failed). The test-side reconcile/oracle must call the
same `Funds`/`markSpent`/`utxoSpendStatus` the production path calls —
preferably through the custody port so the test exercises the real wiring.

## Writing-mode checklist (use before adding a helper to a test file)

1. Name the production counterpart this helper verifies. Can't → it's an
   implementation; src or testkit, not the test file.
2. Will it need a change when prod semantics change? → T2.
3. Does it decide the right answer vs check the system's answer? → T1.
4. Does it touch money/proof/state truth? → T7 and it goes through the
   production port or not at all.
5. Is it mechanics (when) or semantics (what)? → T5.
