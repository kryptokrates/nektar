# orchestration — multi-agent audit runs

How a design-guardrails audit fans out. Audit mode defaults to inline —
the 8-question walk plus red-flag sweep over a diff is a single-pass job.
Swarm when the user asks for a full audit, on `--pkg <name>` scope audits
of packages larger than ~10 src files, or when an in-flight diff creates
more new modules/boundaries than one pass can walk honestly. Checkpoint
mode never delegates — it is pre-writing reflection, not a review.

## Roles

- **Parent** — the agent holding this skill. Scopes the run, builds the
  unit list, launches agents, dedupes findings, runs the gates itself
  (they are commands, and a gate the parent didn't run didn't happen),
  and writes the verdict.
- **research agent** — read-only delegated explorer (Devin:
  `subagent_explore`). Runs auditors and fresh verifiers — audit work is
  source reading.
- **general agent** — delegated worker (Devin: `subagent_general`).
  Only needed when a verifier must execute code to settle a claim.

Use the platform's equivalents, preserving three boundaries: each agent
gets exactly its assigned units, agents return structured results
instead of writing shared state, and no agent verifies its own finding.

## Coverage plan

A unit is **one new or reshaped module, export, or boundary** — the
thing the 8 questions interrogate. Build the unit list before launching:

1. Enumerate new/changed exports (`git diff` on index.ts files, new
   files, changed signatures).
2. Enumerate new boundaries: storage shapes, wire formats, host/port
   interfaces, package edges.
3. Cluster units into assignments by package — one auditor per package
   touched, or per ~5 units, whichever is smaller. A unit is never split
   across auditors and never unassigned; a package too big for one
   auditor splits by module directory, not by question.

The matrix is `unit × {8-question walk, red-flag sweep}` — every unit
gets both. The persistence/wire questions (7) and enforcement question
(8) are mandatory on units that touch persistence, wire formats,
outbox/inbox, host/chain calls, or restartable paths — mark which units
trigger them in the plan.

## Auditor prompt contract

Every auditor prompt contains, in this order:

1. Role preamble (two sentences): audit the assigned units against the
   rule catalogs; return exactly one JSON object matching the result
   contract below.
2. The unit list: each unit's files, exports, and why it was created
   (the diff hunk or feature it belongs to).
3. The catalogs to load by path:
   [module-design.md](module-design.md) always;
   [system-design.md](system-design.md) when the unit touches
   persistence/wire/outbox/restartable paths;
   `../../code-review/references/nektar.md` when it touches keys, sats,
   escrow, SpendGuard, or proofs. Never send catalog names alone.
4. The 8-question walk + the red-flag reflex table, verbatim from
   SKILL.md.
5. Peer-owned units, one line each — observed issues outside the
   assignment go in `out_of_scope`, not the findings.
6. The structured result contract below.

## Structured auditor result

Return exactly one JSON object, no surrounding prose:

```json
{
  "units": [
    {
      "unit": "packages/x/src/foo.ts#FooStore",
      "disposition": "audited|blocked",
      "questions_answered": ["depth", "hidden-decision", "caller-knowledge", "change-amplification", "layering", "red-flags", "persistence", "enforcement"],
      "unresolved": ["what blocked a blocked unit"]
    }
  ],
  "findings": [
    {
      "rule_id": "module/shallow-module|system/no-compat-contract|…",
      "severity": "BLOCKER|MAJOR|MINOR",
      "file": "packages/x/src/foo.ts",
      "line": 42,
      "why": "why it matters here",
      "fix": "the concrete reshape",
      "evidence": "quoted code"
    }
  ],
  "unenforced_invariants": [{ "invariant": "the rule the code relies on", "missing_gate": "what would catch it" }],
  "out_of_scope": [{ "file": "…", "observation": "belongs to a peer unit" }]
}
```

`questions_answered` is the coverage claim — the parent rejects a unit
result whose walk skipped questions the plan marked mandatory. A finding
needs quoted-code `evidence`; no evidence, no finding.

## Verification pass

Every BLOCKER and MAJOR candidate goes to a fresh agent whose only job
is to disprove it: re-derive the structural claim from source (is the
module actually shallow? does a gate actually exist?), and return
`stands` or `rejected` with evidence. Auditors never verify their own
findings. Rejected candidates are dropped; the parent reports the count
so the audit's precision is auditable.

## Anti-patterns

1. Assigning by file instead of by unit — a module split across two
   auditors gets a shallow-review from both and a deep review from
   neither.
2. Skipping `questions_answered` — an auditor that "checked everything"
   without saying which questions ran gives the parent no coverage
   claim.
3. Delegating the gates — `check:cruise`/`check:arch` output is
   evidence; the parent runs them and quotes them.
4. A fourth variant reported as new work — convergent implementation
   (smell S6) findings must cite the existing seams; the verifier checks
   that the named precedent actually exists.
