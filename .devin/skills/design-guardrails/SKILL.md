---
name: design-guardrails
description: >
  Use when designing or writing non-trivial code — new modules, packages,
  interfaces, storage shapes, message flows, or public APIs — and when a
  change will shape structure others must later work inside. Applies
  Ousterhout's A Philosophy of Software Design and Kleppmann's
  Designing Data-Intensive Applications as pre-flight guardrails, so
  deep structural debt is prevented rather than reviewed out later.
  For auditing a finished diff use code-review; for whole-system
  analysis use arch-review.
---

# design-guardrails — write-time structural rules

You are about to write code whose shape other code will have to live with.
The expensive defects are structural: a shallow abstraction, a leaked
decision, a persisted shape nobody can evolve, an invariant only a comment
defends. They are cheap to avoid now and expensive to extract later. This
skill exists to catch them **at authoring time**, before `check:arch` and
code-review ever see the diff.

## Operating rules

- **Strategic, not tactical.** "Working" is the floor, not the goal —
  every change should leave the design better, or at least no worse
  (POSD ch.3/16). If you find yourself asking "what is the smallest diff
  that does what I need?", stop and ask what design the system would have
  had if the feature was in scope from the start. Build that instead.
- **Complexity is incremental.** No single shortcut looks fatal; the
  debt accretes one "just this once" at a time. Treat every red flag in
  the reference files as a stop-and-fix, not a note-for-later.
- **Design it twice** for anything with a real interface or persistence
  footprint: sketch a genuinely different alternative (radically
  different, not a variant), list pros/cons, then pick — or combine
  (POSD ch.11). If both options are ugly, that's signal the abstraction
  boundary itself is wrong.
- **Interfaces first, implementations second.** Write the exported
  signature and its contract (what callers may assume, what's hidden)
  before the body. If you can't write the interface comment without
  describing internals, the module is too shallow or leaking.
- **Enforcement beats memory.** Every rule in this skill has a "how it's
  enforced" hook — a mechanical gate, a review rule ID, or a named
  discipline. If you create a new structural rule, also create its
  enforcement (see [references/enforcement.md](references/enforcement.md)).
  An unenforced invariant is a future incident.

## The design checkpoint — run before writing

Answer these explicitly (in your head for small work, in the task plan
for anything that adds a package, a persisted shape, a wire format, or a
public surface):

1. **Module shape** — what is this module's job, and is its interface
   much simpler than its implementation (deep) or nearly the same
   (shallow)? If shallow: inline it, merge it, or move complexity down.
2. **What does it hide?** Name the design decision(s) the module keeps
   from callers: data layout, protocol staging, error policy, timing,
   key custody. If you can't name one, ask why the module exists.
3. **Who must know what?** List every fact a caller must hold to use
   this correctly. Each one is cognitive load or a leaked decision —
   eliminate it or push it inside.
4. **Blast radius** — which files change if this decision changes? If
   more than one, the decision lives in too many places (information
   leakage → change amplification).
5. **Error semantics** — can the exceptional condition be defined out of
   existence (default, clamp, no-op, aggregate once at boundary)? Can it
   be masked inside the module? Design so there's less to throw.
6. **Persistence & evolution** — will anything this writes be read by
   older or newer builds (SQLite rows, journals, `*.json` payloads,
   wire envelopes, contract artifacts)? If so, what is the compat
   contract — and where is it written down?
7. **Failure & replay** — for any write/act sequence: persist intent →
   act → record outcome. Walk the restart drill before coding it.
8. **Enforcement** — which existing gate protects this design
   (cruise/check-arch/review rule), and does the change weaken it?

## While writing — the red-flag reflexes

Stop and fix on sight (details + rule IDs in the reference files):

| You notice yourself…                                        | Stop: it's a red flag for…                              |
| ----------------------------------------------------------- | ------------------------------------------------------- |
| writing a wrapper that just forwards args                    | pass-through method → wrong boundary or missing class   |
| documenting internals in an interface comment                | implementation docs contaminating contract              |
| "the caller will just set X before calling"                  | temporal coupling / pushed-up complexity                |
| adding a boolean/enum to pick behavior inside a function     | mode flag → two functions, not one                      |
| splitting code by WHEN steps run, not by knowledge           | temporal decomposition → group by hidden information    |
| exporting a type that reveals storage/wire internals         | leaky interface → callers now depend on the detail      |
| writing the same logic a second time "just for this case"    | change amplification / convergent duplication           |
| throwing where a default or no-op is correct                 | exception proliferation → define it out of existence    |
| writing code a stranger can't parse without opening 3 files  | obscurity → hidden dependency, undocumented invariant   |
| persisting a shape with no version/migration story           | schema drift → old builds will read your rows someday   |
| treating a local row / memory map as proof of a remote fact  | hidden source of truth → name the arbiter               |
| applying an effect straight from a delivered message         | at-least-once victim → dedup / idempotency key          |

The full catalog — with the Ousterhout red-flag names and the DDIA
system-level rules — lives in:

- [references/module-design.md](references/module-design.md) — depth,
  hiding, coupling, error semantics, naming, comments, obviousness.
- [references/system-design.md](references/system-design.md) — evolvable
  data shapes, end-to-end correctness, durability ordering, auditability,
  operability (what keeps the system fixable in production).

## After writing — cheap verification before "done"

- Re-read only the **public surface** you added (index/exports/signatures)
  and ask: could I say what this module does from the interface alone,
  without opening internals? If not, the interface lies.
- Run the blast-radius test: list files a reader must open to verify one
  call path through your change. >~3 for a routine call → the boundary
  is wrong or dependencies are obscured.
- Run the gates that measure what you just shaped:
  `pnpm --filter <pkg> build`, `pnpm check:cruise` (no cycles/orphans/
  layering violations — the edge matrix lives in `tools/check-arch.mjs`),
  `pnpm check:arch` (LOC/fanout/god-file baselines + custody/env/layering
  grep gates), `pnpm check:knip` (dead exports/files). For money paths:
  `pnpm --filter <pkg> test`.
- If the change introduces a new structural rule future code must obey,
  add its enforcement in the same change — a check-arch grep gate, a
  cruise forbidden-rule, a review rule ID, or an ADR-style comment at the
  boundary. See [references/enforcement.md](references/enforcement.md).

## nektar-specific calibration

- Custody, SpendGuard coverage, escrow semantics and the honesty rule
  outrank every design preference — load
  `../code-review/references/nektar.md` before designing anything that
  touches keys, sats, or proofs.
- The declared layering is the spec: `core` root → leaf libs → `chain` →
  `anchor/verify/automaton` → `bstack` transport → `mcp/watcher` apps.
  Dep-cruiser can't see `@nektar/*` edges (they resolve into dist/) —
  check-arch's edge matrix is authoritative. Design new dependencies to
  point toward stability, and keep the automation engine reachable only
  through `AutomationHost`.
- Established seams to reuse rather than re-invent: `market_outbox`
  (per-destination tracking), `take()` journaling, op event log,
  `nektarEnv` env reads, `faulthost` fault injection, composition-root
  broadcaster construction. Reusing a seam is strategic; writing a
  fourth variant is convergent implementation (S6).

## Sources

Red-flag taxonomy, deep/shallow modules, information hiding, pass-through,
temporal decomposition, define-errors-out, design-it-twice, names/comments,
consistency, obviousness, strategic-vs-tactical: Ousterhout, _A Philosophy
of Software Design_ (2nd ed.). Reliability/simplicity/evolvability triad,
schema compat, end-to-end argument, idempotence, auditability,
operability: Kleppmann, _Designing Data-Intensive Applications_. Repo
gates: `.dependency-cruiser.cjs`, `tools/check-arch.mjs`,
`tools/check-leftovers.mjs`, `knip`, `jscpd`, oxlint. Review rule IDs
cross-referenced from `../code-review/references/*`.
