# audit.md — worksheets and procedures

Load when running a state-audit. Each phase has a worksheet; fill it before
writing findings — the worksheets are where the empty cells come from.

## Phase 1 — authority map

One row per **fact** (not per table — a fact can span stores):

| fact | arbiter | projections | writers | disagreement rule | gap |
| ---- | ------- | ----------- | ------- | ----------------- | --- |

Grep seeds: `setTxState`, `transition(`, `UPDATE.*state|status`, `state:`,
`WHERE.*state IN`, `reserved`, `tombstoned`, `cache`, `lastStatus`,
`remaining`. Then:

- A "gap" entry of `-` means the rule exists — name it. Anything else is
  `data/hidden-source-of-truth`.
- Watch for **write-side projections**: a column updated alongside the
  arbiter by the same writer (status denormalized into a sibling row, a
  count cached on the parent). Drift there is invisible until a partial
  write or a stale reader exposes it.
- Chain vs local: a local row claiming an on-chain fact (mined, spent,
  landed) is a projection of the chain arbiter — its update path is the
  reconcile. `data/read-your-writes` when a fresh write is read back through
  a lagging index.

## Phase 2 — transition table

Fill the columns in this order — event enumeration is the hard part:

1. **States** — from the union/type, the column's check constraint, and the
   actual `WHERE state IN` usage (they drift; list all three).
2. **Events** — for each state, what can arrive: normal next-step, every
   refusal shape, timeout, duplicate, out-of-order sibling, late evidence
   contradicting a verdict, external signal (chain, host, counterparty).
3. **Event source** — name the emitter class: own write, verified external
   evidence, hint (status word/indexer), timer, replay. Only evidence-class
   sources may fire verdict events — everything else fires `unknown`-ward
   transitions or gets logged as a hint.
4. **Guard → action → targets** — the required evidence, the effect, and
   both outcomes. `target on failure/timeout` left blank is the finding.

Outcome taxonomy — map the code's vocabulary onto it:

| taxonomy  | meaning                           | nektar example                                  |
| --------- | --------------------------------- | ----------------------------------------------- |
| expected  | intent recorded, nothing sent     | op `created`, tx `prepared`                     |
| submitted | left the process, outcome unknown | `submitted`, `submission_unknown`               |
| accepted  | a counterparty/host acknowledged  | offer `quoted`, host `accepted`                 |
| confirmed | independent evidence verifies it  | `confirmed` (proof/depth), `proven` tree        |
| failed    | definitive negative verdict       | `rejected`, `conflicted`                        |
| unknown   | no verdict obtainable yet         | `submission_unknown`, `TX_MISSING_PARENT` waits |

Non-verdicts that must not collapse into `failed`: `PENDING_RETRY` /
`TX_MISSING_PARENT` (wait — ancestor may still land), daily-cap refusal
(defer-window — the `funding_deferred` state exists because burning a
refusal threshold on a policy window was a real incident), nLockTime
`locktime` rejections (park — `nektar/refund-height`), transport timeouts
(unknown — the send may have landed).

## Phase 3 — failure-ownership matrix

| failure mode | detector | resolver | bound | silence reason |
| ------------ | -------- | -------- | ----- | -------------- |

- "Nobody" in detector or resolver is the finding. "Logs" is not a detector
  unless something reads it.
- Retry safety: idempotency key on the effect (op ID / action ID), bounded
  attempts, and refusal classes routed to distinct handlers — not a
  catch-all `includes('fail')` (`nektar/refusal-classified`).
- Adversarial timeline per transition: crash between persist→act and
  act→record (the `data/durability-point` drill — walk all four death
  points); two writers racing one state; a stale lock outliving its owner;
  the same event delivered twice; writes 2-of-3 landing
  (`data/partial-fanout`); events arriving reordered (`data/ordering-scope`).
- Restart inventory per structure: **persists / rebuilt / discarded**. For
  every discard, write down what it costs — a discarded reservation set is
  fine if the durable lease survives; a discarded watch list is not if
  nothing re-arms it. F12 is the format; the audit adds "who reconciles the
  divergence after boot" — boot replay is the reference implementation
  (`runtime.ts` replays `prepared`/`submission_unknown` rows).

## Phase 4 — dormant-path trace

For each recovery/rollback/reconcile function:

1. **Entry points**: list every production entry (boot, timers, event
   handlers, tool calls, dispatch tables, registered callbacks). Work
   forward from entries to the function — not backward from the function
   hoping a caller exists. Missing callers in text search prove nothing:
   dynamic dispatch, string-keyed handlers, and external invocation don't
   grep. `data/dormant-path` is the finding ID.
2. **Reachability**: follow the actual call chain — a gate that can never be
   true in production config is dormancy, not safety. Flags, env defaults,
   and "temporary" disables count.
3. **Test realism**: a test proves the path only if it (a) drives the
   production entry point, (b) uses realistic refusal/failure shapes, (c)
   asserts the observable recovery outcome — not the internal call. A test
   that invokes the recovery function directly with a hand-built state is a
   name-check, not coverage. Heavy-arrange paths rot first (test-boundaries
   calls this the dormant-path incubator).
4. **Ordering**: a recovery path registered after a path that always wins
   first (catch-all, earlier matcher, unconditional early return) is
   dormant by construction.

## Phase 5 — boundary & representation checks

- **Declared vs returned vs consumed**: diff the producer's contract, its
  actual output shape, and each consumer's reads. Any consumer reading a
  field the contract doesn't declare is schema debt; a writer emitting a
  shape an old reader rejects is `data/schema-drift`.
- **Limit site vs measurement site**: for every bound, find (a) where it's
  checked and (b) what the receiver actually constrains. The bound must be
  evaluated on the constrained artifact: sealed envelope not plaintext
  (`nektar/sealed-budget`), total spend not pre-fee subtotal, decoded rows
  not compressed blob, wire message not domain object. A check measured on
  the wrong representation passes artifacts that then fail downstream —
  after the side effects may already have run.
- **Failure vocabulary**: enumerate the failure types crossing the boundary
  and confirm each maps to a distinct handling where recovery differs.
  Conflation usually happens at the first `catch`.
- **Policy authority**: for each operational control (cap, gate, TTL, allow
  list) — where is it declared, who can change it, can the party it
  restricts discover+request it? A policy gate reachable only via an env
  var the operator doesn't know exists is hidden authority; controls belong
  in the policy schema with the rest.

## Phase 6 — verification recipes

The six drills — pick per finding, don't run the whole set:

1. **Crash persist→act**: kill between journal/intent write and the side
   effect. Reboot → replays once, or correctly resumes? (Double-send =
   `data/exactly-once-fantasy`.)
2. **Crash act→record**: kill after broadcast/send, before outcome write.
   Reboot → dedup'd retry or reconcile, not a blind repeat?
3. **Timeout-no-verdict**: make the external verdict unreachable; assert the
   state parks in `unknown`-class with a live reconcile — never settles as
   `failed`.
4. **Duplicate / out-of-order delivery**: same event twice; child event
   before parent; late event after terminal. Assert idempotent absorb or
   named handling.
5. **Kill-restart inventory**: `kill -9` mid-cycle; diff pre-crash durable
   state vs post-boot visible state. Every divergence is either rebuilt,
   reconciled, or an explained discard.
6. **Refused-but-landed**: external system applies the effect despite the
   refusal/timeout (mined anyway, delivered anyway). Assert the late
   evidence revives or re-routes — never strands.

Fixture rules (test-boundaries owns the details): fixtures obey domain
invariants — no double-spends in arrange, real refusal shapes not invented
strings, and `dist/` rebuilt before reading results as signal (mcp tests
run `dist/test/*.test.js`).

**Model-based layer** — when a declared machine exists for the resource:
enumerate reachable states and paths (`@xstate/graph` or equivalent),
assert every declared state is reachable, every non-terminal state reaches
a terminal, and every recovery state is reachable from a real event — not
only from a test-only `force` transition. Keep ≥1 integration crash/restart
test: graph checks verify the model, not that production emits the events.
