# orchestration — multi-agent review runs

How a code review fans out. Default is inline — one reviewer running the
four passes in order is the right shape for most diffs. Swarm when the
user asks for a deep/comprehensive review, when the diff spans ≥3
packages or >~400 changed lines, or when a money-path change deserves
independent eyes per pass.

## Roles

- **Parent** — the agent holding this skill. Gathers the diff, loads
  [nektar.md](nektar.md) itself (it judges money findings at merge
  time), owns the `file × pass` coverage matrix, dedupes findings, runs
  the cheap verifiers (build/test/lint) itself, and writes the verdict.
- **research agent** — read-only delegated explorer (Devin:
  `subagent_explore`). Runs pass reviewers when no execution is needed.
- **general agent** — delegated worker with execution access (Devin:
  `subagent_general`). Runs fresh verifiers, and pass reviewers that
  need to run code to settle a claim.

Use the platform's equivalents, preserving three boundaries: each agent
gets exactly its assigned units, agents return structured results
instead of writing shared state, and no agent verifies its own finding.

## Coverage plan

A unit is **review pass × file group**. The passes are the four from
SKILL.md: `money` (nektar.md invariants), `correctness`, `design`,
`production`. The file groups are disjoint subsets of the changed file
list, clustered so a group is reviewable in context (a file and its
test stay together).

Default plan: one reviewer per pass covering the whole diff — 4 agents.
Large diffs: split each pass by file group — `pass × group` agents,
every group covered by every pass. The matrix is the coverage claim: a
`file × pass` cell no reviewer reached is `deferred` and disclosed, never
silently dropped. When reducing scope, drop `design`/`production` on
generated-adjacent files first; never drop `money` on a file that moves
sats, keys, escrow, SpendGuard, or proofs.

## Reviewer prompt contract

Every reviewer prompt contains, in this order:

1. Role preamble (two sentences): review the assigned files through the
   assigned pass only; return exactly one JSON object matching the
   result contract below.
2. How to get the diff: the base ref or file list, the exact
   `git diff`/`gh pr diff` command, and which files are in scope. Never
   review unchanged code unless the change alters its contract.
3. The lens files for its pass — load by path:
   `money` → [nektar.md](nektar.md) (always, plus
   [data-systems.md](data-systems.md) when persistence/messaging is
   touched); `correctness` → data-systems.md +
   [stability.md](stability.md) as triggered; `design` →
   [structure.md](structure.md); `production` → stability.md. Never
   send lens names alone.
4. The common rule catalog + severity table from SKILL.md, verbatim.
5. Its sibling passes' territory, one line: "money/correctness/design/
   production findings outside your pass go in `out_of_pass`, not your
   findings" — cross-pass observations are collected, not lost.
6. The structured result contract below.

## Structured reviewer result

Return exactly one JSON object, no surrounding prose:

```json
{
  "pass": "money|correctness|design|production",
  "checked_files": ["repo/relative/path"],
  "findings": [
    {
      "rule_id": "nektar/spendguard-gap|common/ignored-error|data/…",
      "severity": "BLOCKER|MAJOR|MINOR|NIT",
      "file": "packages/x/src/y.ts",
      "line": 42,
      "why": "why it matters here, not what is wrong",
      "fix": "the concrete change",
      "evidence": "quoted code or command output"
    }
  ],
  "out_of_pass": [
    {"file": "…", "line": 0, "observation": "belongs to another pass"}
  ],
  "done_well": ["what this pass saw done right"]
}
```

A finding needs `evidence` — quoted code or output. No evidence, no
finding. `checked_files` is the coverage claim: the parent diffs it
against the assigned group and reassigns any file left unreviewed.

## Verification pass

Every BLOCKER and MAJOR candidate goes to a fresh general agent whose
only job is to disprove it: re-derive the claim from current source,
check whether existing guards/tests/callers already prevent it, and
return `stands` or `rejected` with evidence. A reviewer never verifies
its own finding. Rejected findings are dropped from Findings; the
parent notes the count in the report ("2 candidates rejected on
verification") so the review's precision is auditable.

## Anti-patterns

1. One agent running all four passes — that is an inline review with
   extra latency, not a swarm; the value is independent pass judgment.
2. Splitting the diff mid-context — a reviewer that sees a function but
   not its caller invents findings; cluster by caller/callee.
3. Letting reviewers see each other's output before submitting —
   anchoring defeats four-pass independence.
4. Counting findings as thoroughness — a pass returning zero findings
   with a full `checked_files` list is complete coverage, not a lazy
   agent.
5. Skipping verification on money findings because the reviewer is
   confident — confidence is exactly what the fresh verifier tests.
