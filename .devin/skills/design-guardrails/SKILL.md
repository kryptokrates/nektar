---
name: design-guardrails
description: >
  Use to audit in-flight or newly written code for deep structural issues —
  run it on the working diff, a named file/package, or the last commit —
  and when designing non-trivial code (new modules, packages, interfaces,
  storage shapes, message flows, public APIs). Applies Ousterhout's
  A Philosophy of Software Design and Kleppmann's Designing Data-Intensive
  Applications as enforceable rules, each mapped to the gate or review
  rule ID that catches a miss. For PR/diff review use code-review; for
  whole-system analysis use arch-review.
---

# design-guardrails — structural rules audit + authoring checks

Structural defects are cheap to avoid while writing and expensive to
extract after merge: shallow abstractions, leaked decisions, persisted
shapes nobody can evolve, invariants only a comment defends. This skill
has two jobs:

- **AUDIT** — walk in-flight code against the rule catalogs and report
  findings with rule IDs, `file:line`, why, and the fix.
- **CHECKPOINT** — the same rules applied before/during writing, so the
  audit finds nothing.

## Audit mode — process

### 1. Scope

- `--files <paths>` → audit those files.
- `--pkg <name>` → that package's src/ (a scope audit, not a diff audit).
- `--last [n]` → last n commits on this branch (`git show`).
- Default → in-flight work: `git diff` + `git diff --cached` + untracked
  files under `packages/*/src`. Generated artifacts (contract JSON,
  verifier.js, dist/) are out of scope.

### 2. Load the catalogs

- [references/module-design.md](references/module-design.md) — ALWAYS:
  depth, hiding, coupling, error semantics, names/comments, obviousness.
- [references/system-design.md](references/system-design.md) — when the
  diff touches persistence, wire formats, outbox/inbox, host/chain calls,
  or restartable paths.
- `../code-review/references/nektar.md` — when the diff touches keys,
  sats, escrow, SpendGuard, or proofs.
- `../event-arch` (skill) — when the scope is specifically the realtime
  event layer (waits, subscriptions, watch/wake machinery, outbox
  delivery, broadcaster statuses): run its event-surface audit instead;
  this skill stays the general structural auditor.

### 3. Run the audit pass — per touched module/file

For each new or re-shaped module, export, or boundary, ask:

1. **Depth** — is the interface much simpler than what it hides? Would a
   caller do better inlining it? (shallow-module)
2. **Hidden decision** — name the decision(s) the module keeps from
   callers: layout, staging, error policy, timing, custody. Can't name
   one → why does it exist?
3. **Caller knowledge** — list the facts a caller must hold to call this
   correctly. Each is cognitive load or leakage; count them.
4. **Change amplification** — pick the module's most likely future
   change; trace how many files it would touch. >1 → the decision lives
   in too many places.
5. **Layering** — do new dependencies point toward stability? Check the
   edge matrix in `tools/check-arch.mjs`, not vibes — depcruise can't
   see `@nektar/*` edges (they resolve into dist/).
6. **Red-flag sweep** — walk the file against the reflex table below;
   every hit gets a finding or a justified exception.
7. **For persistence/wire paths** — compat contract named? durability
   order (persist intent → act → record outcome)? idempotency/dedup?
   derived-state rebuild path?
8. **Enforcement** — which existing gate protects each new invariant?
   A rule with no gate is an "unenforced invariant" finding.

### 4. Run the gates that measure the change

`pnpm --filter <pkg> build` → `pnpm check:cruise` → `pnpm check:arch` →
`pnpm check:knip` → `pnpm check:leftovers` → `pnpm check:dupes`; for
money/durability paths also `pnpm --filter <pkg> test` and
`pnpm check:lint:types`. "Not run" is an honest report line; a claimed
gate that didn't run is not.

### 5. Report

```
## Design audit — <scope>

**Verdict: CLEAN | MINOR ISSUES | NEEDS RESHAPE | BLOCKED**

### Findings
- [SEV] `<rule-id>` — file:line — <why it matters here> — <the fix>
  (SEV: BLOCKER = money/durability/custody at risk; MAJOR = structural
   debt that compounds; MINOR = worth fixing while you're here)

### Public-surface check
<new exports/interfaces — is each deep? does the index say what it promises?>

### Gate results
- pnpm check:cruise: pass/fail — <details>
- pnpm check:arch: pass/fail — <details>
- <every gate actually run; "not run" entries listed honestly>

### Unenforced invariants
<each structural rule the change relies on that no gate/review rule
catches — propose the gate, or name the promotion explicitly deferred>
```

Never invent findings to look thorough. If the code is clean, say what
was checked and move on — evidence over confidence: cite `file:line`.

## Checkpoint mode — before writing non-trivial code

Answer explicitly (in the task plan for package/persisted-shape/wire/API
work, in your head otherwise):

1. **Module shape** — deep (interface ≪ implementation) or shallow?
   Shallow: inline it, merge it, or pull complexity inside.
2. **What does it hide?** If no design decision is hidden, the module is
   decoration — delete it before it exists.
3. **Who must know what?** Every fact a caller must hold is a leak
   candidate — push it inside or make it unrepresentable.
4. **Blast radius** — which files change when this decision changes?
5. **Error semantics** — can the exceptional path be defined out of
   existence (default/clamp/no-op/aggregate-at-boundary)?
6. **Persistence & evolution** — will older/newer builds read these
   bytes? Where is the compat contract written down?
7. **Failure & replay** — persist intent → act → record outcome; walk
   the restart drill before coding it.
8. **Enforcement** — which gate protects this design; does the change
   weaken any existing one?

## Red-flag reflexes — stop and fix on sight

| You notice yourself…                                        | Red flag                                              |
| ----------------------------------------------------------- | ----------------------------------------------------- |
| writing a wrapper that just forwards args                   | pass-through method → wrong boundary or missing class |
| documenting internals in an interface comment               | impl docs contaminating the contract                  |
| "the caller will just set X before calling"                 | temporal coupling / pushed-up complexity              |
| adding a boolean/enum to pick behavior inside a function    | mode flag → two functions, not one                    |
| splitting code by WHEN steps run, not by knowledge          | temporal decomposition → group by hidden information  |
| exporting a type that reveals storage/wire internals        | leaky interface → callers now depend on the detail    |
| writing the same logic a second time "just for this case"   | change amplification / convergent duplication         |
| throwing where a default or no-op is correct                | exception proliferation → define it out of existence  |
| writing code a stranger can't parse without opening 3 files | obscurity → hidden dependency, undocumented invariant |
| persisting a shape with no version/migration story          | schema drift → old builds will read your rows someday |
| treating a local row / memory map as proof of a remote fact | hidden source of truth → name the arbiter             |
| applying an effect straight from a delivered message        | at-least-once victim → dedup / idempotency key        |

## nektar-specific calibration

- Custody, SpendGuard coverage, escrow semantics and the honesty rule
  outrank every design preference — load
  `../code-review/references/nektar.md` before auditing anything that
  touches keys, sats, or proofs.
- Declared layering is the spec: `core` root → leaf libs → `chain` →
  `anchor/verify/automaton` → `bstack` transport → `mcp/watcher` apps.
  Design dependencies to point toward stability; the automation engine
  reaches the app only through `AutomationHost`.
- Established seams to reuse, not re-invent: `market_outbox`
  (per-destination tracking), `take()` journaling, op event log,
  `nektarEnv` env reads, `faulthost` fault injection, composition-root
  broadcaster construction. A fourth variant of an existing pattern is
  convergent implementation (smell S6).

## Sources

Red-flag taxonomy, deep/shallow modules, information hiding, pass-through,
temporal decomposition, define-errors-out, design-it-twice, names/comments,
consistency, obviousness, strategic-vs-tactical: Ousterhout, _A Philosophy
of Software Design_ (2nd ed.). Reliability/simplicity/evolvability triad,
schema compat, end-to-end argument, idempotence, auditability,
operability: Kleppmann, _Designing Data-Intensive Applications_. Repo
gates: `.dependency-cruiser.cjs`, `tools/check-arch.mjs`,
`tools/check-leftovers.mjs`, `knip`, `jscpd`, oxlint. Rule IDs
cross-referenced from `../code-review/references/*`. Enforcement model:
[references/enforcement.md](references/enforcement.md).
