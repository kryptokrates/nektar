---
name: test-boundaries
description: >
  Use when auditing test files for business logic — run it on the working
  diff, a named test file/package, or the last commit — and when writing or
  refactoring tests. Enforces the boundary: a test file contains arrange /
  act / assert, fixtures (data), and harness glue (mechanics) — never domain
  rules, retry/cadence policy, state machines, schema probing, or a second
  implementation of production code. A test that re-derives what it verifies
  proves nothing and rots silently. Repo-agnostic rules; gate mappings are
  suggestions each repo wires to its own tooling.
---

# test-boundaries — no business logic in test files

A test is an **oracle**, not an **implementation**. Its job is to exercise
production code and report a verdict. The moment a test file decides
outcomes on its own — reconciles state, encodes policy, mirrors a state
machine — it stops verifying and starts _competing_: two codebases that
must stay in lockstep, where the test's copy silently becomes the spec.

## The classification test (apply to every non-trivial helper in a test file)

Ask one question: **if production semantics changed in a way the test suite
is supposed to catch, would this helper need to change too?**

- **Yes → business logic.** It lives in the wrong file. Move it to src
  (behind the documented seam the test then injects against) or delete the
  helper and call the production implementation.
- **No → test logic.** Scheduling, assertion plumbing, fake wiring, fixture
  loading — stays.

Two sharper forms of the same question for argument-resistant cases:

- **The lockstep test:** grep for the production counterpart. If one exists
  and the helper must "stay in sync" with it, the helper is a parallel
  implementation — T2.
- **The oracle test:** does the helper decide _what the right answer is_
  rather than _whether the system produced it_? Oracles compare; they don't
  compute.

## Rules (full catalog: [references/rules.md](references/rules.md))

| ID  | Rule                                                                                                                                                            | Default severity |
| --- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------- |
| T1  | **No domain decisions in test files** — money, escrow, proofs, scoring, admissions, policy evaluation                                                           | BLOCKER          |
| T2  | **No parallel implementations** — a helper mirroring src semantics is a second codebase; import or extract instead                                              | BLOCKER          |
| T3  | **Assembly through the composition root** — inject fakes at documented seams; hand-built stacks of real components drift from production wiring                 | HIGH             |
| T4  | **Fixtures are data, not logic** — literals, serialized objects, expected outputs. A policy computed inside the test is logic in a trench coat                  | HIGH             |
| T5  | **Harness = mechanics, not semantics** — drivers may schedule calls; they must not encode what _should_ happen (cadences, windows, thresholds copied from prod) | HIGH             |
| T6  | **No probing production internals** — no SQL against prod tables, no reading private file formats, no reaching past the port/interface the app exposes          | MED              |
| T7  | **No test-side bookkeeping of shared truth** — coin books, ledgers, reserve marks, dedup maps re-derived in a test are how phantom state ships                  | BLOCKER          |

## Extraction targets (in order)

1. **The production implementation** — if src already has it, call it.
   Inject only the seams src documents (ports, env, `*For`-style config).
2. **A src-side seam the test then uses** — when the logic is genuinely
   needed by both, it belongs in src and ships to production (e.g. this
   repo's `testkit.ts`/`testtier.ts` convention).
3. **A shared testkit** — pure harness mechanics reused across files
   (drivers, waiters, fakes). It schedules and stubs; it never decides.
4. **The test file itself** — arrange/act/assert + literal fixtures only.

## Audit procedure

- `--files <paths>` / `--pkg <name>` / `--last [n]` — same scoping as
  design-guardrails. Default: `git diff` + untracked files under
  `**/test/`, `**/*.test.*`, `**/e2e/**`.
- For each top-level helper/const in scope: run the classification test.
  Report findings as `T<ID> file:line — <why> — <extraction target>`.
- Sweep the cheap signals first: test file >300 LOC with >10 top-level
  helpers; `SELECT|INSERT|UPDATE|DELETE|knex\(` in a test; `new ProdClass(`
  chains that mirror the composition root; `process.env.X=` writes that
  outlive the test; comments like "must stay in sync", "same as in", "copy of".
- A finding is confirmed when the production counterpart (or its absence)
  is named — never report "probably duplicates something" without the
  something.

## Writing mode — the checkpoint

Before adding a helper to a test file: name the production counterpart it
verifies. If you can't, you're writing a second implementation — put it in
src where it gets reviewed, gated, and shipped, then test _it_.

## nektar-specific gate mapping

- `check:arch` ratchets LOC — extend `tools/.arch-baseline.json` tracking
  to `packages/*/test/**` if test-dir growth should gate too.
- `check:leftovers` counts `as any`/`@ts-ignore` — same debt class in
  tests (a cast around a production type is often hiding a T2 drift).
- `check:knip` — a helper only tests use should live under `test/` or the
  testkit; a src export consumed by nothing but tests is the inverse smell
  (test-only surface on the public API).
- `check:dupes` (jscpd) — clones across test files are T2's mechanical
  signature when scoped to `test/**`.

## Harness friction — traps that eat time, not correctness

- **dist vs src**: `pnpm --filter @nektar/mcp test` runs
  `dist/test/*.test.js` — source edits are invisible until rebuild. A
  red test after a fix is stale build until proven otherwise; rebuild
  before reading the result as signal.
- **Fixtures must respect the domain's own invariants.** A test that
  re-leases one outpoint across two submissions is arranging a custody
  violation, not a scenario — `take()`'s lease conflict is correct
  behavior. When an arrange needs two spends, fund two outpoints.
- **TS narrowing traps in asserts**: `assert.deepEqual(arr, [])` narrows
  `arr` to `never[]` under strict mode — later `push` calls in closures
  fail typecheck. Assert `arr.length === 0` instead.
- **Heavy arrange is a dormant-path incubator.** If exercising a
  refusal/fallback needs a full wallet + coin book + real tx, the path
  stays untested and rots (`data/dormant-path`). Prefer extracting the
  seam the path needs over skipping the test.
- **`assert.equal`/`deepEqual` on enum verdicts**: check the real union
  first (`TxVerdict` is `mined|pending|dead`, no `'held'`) — an
  assertion written against an imagined value passes the arrange and
  dies at runtime.
