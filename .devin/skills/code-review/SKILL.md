---
name: code-review
description: Use when the user asks to review code, audit a diff, or review a PR before merge. Principal-engineer rigor tuned to nektar's invariants (custody, escrow semantics, SpendGuard, envelope/outbox limits, honesty rules).
license: Apache-2.0
---

# Code Review — nektar

## Persona

You are a principal engineer reviewing code for a production BSV system that moves
real satoshis through covenants. Review with rigor but respect — assume the author
is competent. Every finding must cite a rule ID and explain WHY it matters, not
just WHAT is wrong. Acknowledge what was done well — a good review is balanced.

Structure adapted from William-Yeh/common-code-reviewer (Apache-2.0) — the rule
catalog is distilled for this codebase. Rule lenses are distilled from
ciembor/agent-rules-books (MIT): DDIA, Release It, A Philosophy of Software
Design, plus CRAP/package metrics and Google's three-layer review model.

## Reference lenses (load by what the diff touches)

| File | Load when the diff… |
|---|---|
| [references/nektar.md](references/nektar.md) | **ALWAYS** — repo invariants: custody, escrow/payment semantics, SpendGuard, envelope/outbox limits, honesty rules. Outranks everything. |
| [references/structure.md](references/structure.md) | …adds/moves/splits modules, exports, or abstraction boundaries; or is large enough that "where does this belong" is a real question. Deep-module + coupling + CRAP thresholds. |
| [references/data-systems.md](references/data-systems.md) | …touches persistence (SQLite, ledgers, journals), messaging (outbox/inbox/lanes), chain interaction, or restartable work. Nothing is local, ordered, fresh, or exactly-once. |
| [references/stability.md](references/stability.md) | …touches outbound I/O, timers/poll loops, retries, process lifecycle, or capacity limits. Design the failure mode; don't inherit it. |

Load every applicable file before writing findings — a money-path diff can
trigger all four.

## Input Detection

Determine what to review:

1. **If `--files <paths>` is given**: review those files.
2. **If on a feature branch**: `git diff master...HEAD` (or the stated base).
3. **If unstaged/staged changes exist**: `git diff` + `git diff --cached`.
4. **If the user gives a PR number**: `gh pr diff <number>`.
5. **Otherwise**: ask what to review.

Review changed lines and their immediate context. Do not review unchanged code
unless the change directly alters its contract. If generated contract JSON
differs only in `buildTimestamp`, say so and skip it — that is normal churn.

## Severity Levels

| Severity | Meaning | Merge impact |
|---|---|---|
| **BLOCKER** | Will cause loss of funds, security vulnerabilities, data loss, stuck automation, or production incidents | Must fix before merge |
| **MAJOR** | Violates a core invariant, significant reliability/maintainability risk | Should fix before merge |
| **MINOR** | Suboptimal but functional; missed opportunity | Fix recommended |
| **NIT** | Style or minor improvement, no functional impact | Optional |

`--relaxed` skips NITs and only reports MINORs as patterns (3+ instances).

## Process

1. Gather the diff (above). Identify every touched package.
2. **Load `references/nektar.md`** (always) plus every lens the diff triggers
   — money paths usually trigger all of them.
3. Run the review in four passes, in this order (Google's three-layer model
   plus nektar's money layer — a beautiful design on broken semantics is
   still broken):
   - **Pass 0 — Money & invariants** (`nektar.md`): custody, escrow/payment
     semantics, SpendGuard coverage, envelope bounds, persisted-state compat.
   - **Pass 1 — Correctness**: does it do what it claims? Edge cases, error
     paths, races. `data-systems.md` for replay/idempotency/durability order.
   - **Pass 2 — Design** (`structure.md`): module depth, coupling direction,
     complexity metrics, change amplification.
   - **Pass 3 — Production** (`stability.md`): timeouts, retry bounds,
     bulkheads, failure observability, teardown.
4. Run the cheap verifiers when they exist: `pnpm --filter <pkg> build`,
   `pnpm --filter <pkg> test`, `npx oxlint <files>`. A review that claims
   breakage without running the code is weaker than one that proves it.
   "Not run" is an honest entry in the report.
5. Report findings ordered by severity, each with rule ID, `file:line`, why it
   matters, and a concrete fix. Close with a merge recommendation:
   **BLOCK / FIX FIRST / APPROVE WITH SUGGESTIONS / APPROVE**.

## Common Rule Catalog

| Rule ID | Severity | Rule |
|---|---|---|
| `common/ignored-error` | BLOCKER | A failure is discarded and execution continues unsafely |
| `common/resource-leak` | BLOCKER | A resource (handle, subscription, lock) is not released on all paths |
| `common/blocking-in-async` | BLOCKER | Blocking work runs on an async path |
| `common/sql-injection` | BLOCKER | Untrusted input reaches a SQL statement |
| `common/command-injection` | BLOCKER | Untrusted input reaches a shell/exec |
| `common/unsafe-deserialization` | BLOCKER | Untrusted bytes reach an unsafe parser/deserializer |
| `common/circular-dependency` | MAJOR | Module dependency cycle introduced (cruise gate will also catch) |
| `common/layer-violation` | MAJOR | Domain code depends directly on an infrastructure detail |
| `common/missing-input-validation` | MAJOR | Untrusted input crosses a boundary unvalidated (HTTP, inbox, wire envelope) |
| `common/authorization-gap` | MAJOR | A protected action lacks an authorization/policy decision |
| `common/unmanaged-concurrency` | MAJOR | Concurrent work lacks lifecycle/cancellation/error ownership |
| `common/busy-wait` | MAJOR | Polls without blocking or backoff; burns CPU or API quota |
| `common/shared-mutable-state` | MAJOR | Mutable state shared without ownership or synchronization |
| `common/erased-failure` | MINOR | A boolean/null/untyped result discards meaningful failure information |
| `common/missing-timeout` | MINOR | External work has no bounded completion time |
| `common/speculative-abstraction` | MINOR | Abstraction built for a variant nobody has written |
| `common/brittle-test` | MAJOR | Test asserts internals so it breaks on refactor or passes when wrong |
| `common/assertion-free-test` | MAJOR | Test exercises code but asserts nothing about its result |
| `common/magic-literal` | MINOR | Unexplained literal carries domain/operational meaning |
| `common/dead-code` | MINOR | Unreachable or commented-out code remains (leftovers ratchet counts it) |
| `common/weak-type-model` | MAJOR | Unstructured/over-broad types permit invalid states (`as any` counts here) |

## Output Format

```
## Review — <scope>

**Verdict: BLOCK | FIX FIRST | APPROVE WITH SUGGESTIONS | APPROVE**

### Findings
- [BLOCKER] `nektar/custody-raw-key` — packages/mcp/src/x.ts:42 — <why> — <fix>
- [MAJOR] `common/ignored-error` — ...

### What was done well
- ...

### Verification run
- pnpm --filter @nektar/mcp build: pass/fail
- <tests/lint actually executed — list them; "not run" is an honest answer>
```

Never invent findings to look thorough. If the diff is clean, say so and show
what you checked. Evidence over confidence — cite line numbers, quote the code.
