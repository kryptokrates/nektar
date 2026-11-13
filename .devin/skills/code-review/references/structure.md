# structure — structural complexity & design integrity

Load for diffs that add, move, or split modules, classes, exports, or
abstraction boundaries — or any diff large enough that "where does this
belong" is a question. Rules distilled from Ousterhout's _A Philosophy of
Software Design_, Robert Martin's package metrics, and CRAP scoring —
adapted under MIT from ciembor/agent-rules-books. Thresholds are measured,
not vibes; cite the number when you cite the rule.

## The one question

Does this change make the codebase **cheaper to understand, modify, verify,
and extend** — or does it lower today's diff at the price of tomorrow's
cognitive load? Working code is not evidence of simple design.

## Module depth (Ousterhout)

| Rule ID                       | Severity | Rule                                                                                                                                                                                                                                                                                                    |
| ----------------------------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `struct/shallow-module`       | MAJOR    | A new module, helper, wrapper, service, or layer whose interface is nearly as complex as its implementation — it adds names and files without hiding complexity. The test: could a caller inline it in fewer lines than the interface costs to learn?                                                   |
| `struct/push-down`            | MINOR    | Complexity pushed upward to callers when the lower module owns the knowledge — repeated setup ceremony, caller-side validation of internal invariants, or every call site duplicating the same orchestration. Pull complexity down; a slightly more complex implementation is worth a simpler contract. |
| `struct/leaky-interface`      | MAJOR    | An interface exposing internals: storage shape, wire format, key custody, protocol staging, or implementation modes the caller must sequence correctly. Callers needing to know HOW is information leaking — the interface is not deep.                                                                 |
| `struct/temporal-coupling`    | MAJOR    | Correctness depends on calls happening in an order the types don't enforce (`init`/`prepare`/`process`/`finalize` sequences, staged objects). Restructure around the real concept or make the invalid order unrepresentable.                                                                            |
| `struct/mode-flag`            | MINOR    | A boolean/enum parameter that switches between two behaviors inside a function — usually two functions wearing a trench coat, and a hidden dependency for every caller.                                                                                                                                 |
| `struct/change-amplification` | MAJOR    | One conceptual change requires edits in N unrelated places. The knowledge is scattered, not the work — extract or relocate so the fact lives in one place.                                                                                                                                              |
| `struct/pass-through`         | MINOR    | State or arguments that transit a function untouched en route to a deeper callee — intermediate layers coupling to data they don't use. Route around them.                                                                                                                                              |

## Coupling & cohesion (package metrics — compute, don't guess)

For packages/modules, measure roughly from imports:

- **Ca (afferent)** — inbound dependents; **Ce (efferent)** — outbound deps
- **Instability I = Ce/(Ce+Ca)** — 0 maximally stable, 1 maximally unstable
- **Distance D = |A + I − 1|** — how far the package sits from the ideal
  abstractness↔stability line

| Rule ID                     | Severity | Rule                                                                                                                                                                                                                                              |
| --------------------------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `struct/stable-concrete`    | MINOR    | A heavily-depended-on module (low I) accumulating concrete implementation with no stable interface — the "zone of pain": changes ripple outward through every dependent. Domain cores (`memory`, `automation/market`) must stay behind contracts. |
| `struct/dep-inversion`      | MAJOR    | A stable/core module importing an unstable concrete detail (domain importing transport, persistence, SDK internals directly). Dependency direction must point toward stability — adapters at the edge.                                            |
| `struct/fan-out`            | MINOR    | A module depending on many unrelated others (Ce high) — god-context accumulating. Split by what changes together.                                                                                                                                 |
| `struct/scattered-cohesion` | MAJOR    | Code about one concept split across modules that must change in lockstep (shotgun surgery inverse: fragmentation instead of duplication). Co-locate what varies together.                                                                         |

Existing gates already measure part of this — don't re-derive by eyeball:
`check:cruise` (cycles, orphans), `check:knip` (dead exports/files),
`check:dupes` (jscpd >5%), `.dependency-cruiser.cjs` allowlists.

## Complexity metrics (numbers, not taste)

For a **touched** function f: CC = cyclomatic complexity, cov = test coverage
of that function (estimate honestly from the tests you can see).

- **CRAP(f) = CC² · (1−cov)³ + CC** — the risk score that weights untested
  complexity.

| Rule ID                | Severity | Rule                                                                                                                                    |
| ---------------------- | -------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| `struct/crap-critical` | MAJOR    | Touched function scores CRAP > 30 — too complex AND too untested to change safely. Either add coverage or simplify before extending it. |
| `struct/crap-elevated` | MINOR    | CRAP 8–30 — the function is on the slope; new branches should come with tests.                                                          |
| `struct/cc-branchy`    | MINOR    | CC > 6 while CRAP stays ≤ 8 (tested but branchy) — complexity is contained but consider flattening dispatch/early-returns.              |
| `struct/deep-nesting`  | MINOR    | Nesting past 3 levels obscures the path structure — prefer guard clauses/extraction.                                                    |

## Design-integrity smells (what linters can't see)

| Rule ID                        | Severity | Rule                                                                                                                                                                                                  |
| ------------------------------ | -------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `struct/obscured-dependency`   | MAJOR    | A dependency a reader can't see from the code — module-level mutable state, ambient singletons, globals-as-parameters, ordering enforced only by convention. Make the dependency a parameter/field.   |
| `struct/obscured-obscurity`    | MAJOR    | The reader can't tell whether a call is safe without reading N other files (hidden invariants, undocumented preconditions, "the caller always…"). Contract belongs at the interface — doc or enforce. |
| `struct/speculative-layer`     | MINOR    | Abstraction for a second implementation that doesn't exist (one-implementation interfaces, factory-of-one, plugin points with no plugin). Earn abstractions with the second caller.                   |
| `struct/inconsistent-idiom`    | MINOR    | Same problem solved differently within one file/package than the established convention (promise style, error shapes, naming patterns). Consistency is itself information.                            |
| `struct/deep-vs-flat-mismatch` | MINOR    | New code that wraps the codebase's existing thin-wrapper mistake to "match style" — consistency with a bad pattern propagates the bad pattern; flag the pattern, not just the instance.               |

## Review heuristics

- **Read the change as a stranger**: list the facts you had to hold in your
  head simultaneously to verify it. That's the cognitive-load score — if it
  exceeds ~7 for a routine change, something's hiding.
- **Count the blast radius**: how many files must be re-read to confirm this
  diff is safe? High blast radius = the boundary is wrong, not the code.
- **Interfaces before implementations**: review the exported signature first;
  if the contract is wrong the code behind it doesn't matter yet.
- **Name test**: every new public name should answer "what concept" not
  "what mechanism" (`shrinkWire` yes, `handleOversizePayloadRetryLogic` no).
