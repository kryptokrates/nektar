# orchestration — multi-agent audit runs

How an spv-strict audit fans out. Default is inline — a diff's claims
and waits fit in one pass. Swarm on whole-repo runs (the default
whole-repo scope is `chain`, `bstack`, `automation`, `mcp`, `anchor`,
`deploy/bstack-sandbox`), explicit full-audit requests, or any scope
where the claims and waits inventories are too wide for one pass.

## Roles

- **Parent** — the agent holding this skill. Scopes the run, owns the
  claims and waits maps, launches agents, dedupes findings, and writes
  the verdict + scores. Only the parent consolidates — agents never see
  each other's output.
- **research agent** — read-only delegated explorer (Devin:
  `subagent_explore`). Runs inventory agents, rule-walkers, and fresh
  verifiers — this is all source reading.
- **general agent** — delegated worker (Devin: `subagent_general`).
  Only needed when a verifier must run code to settle a claim.

Use the platform's equivalents, preserving three boundaries: each agent
gets exactly its assigned units, agents return structured results
instead of writing shared state, and no agent verifies its own finding.

## Coverage plan

Two stages, mirroring the skill's two halves.

**Stage 1 — inventory, two parallel research agents:**

- **Claims agent** — every place code concludes mined / proven /
  rejected / spent / unspent / final / funded. Per claim: what object
  carries it (BUMP, BEEF, status string, index row, host answer), which
  header source anchors it, what depth policy applies.
- **Waits agent** — every place the deal path can stall: sleeps, polls,
  `proven` gates, "no funding slot", "not proven yet", warmups,
  `waitFor`. Per wait: what it waits on, could the fact have been proven
  before the wait began, what stock or evidence was missing.

For whole-repo runs, split each inventory by package cluster. The parent
merges into the claims map and waits map — together they are the
coverage claim.

**Stage 2 — rule-walkers:**

- **Trust-half auditor** — walks the claims map against
  [doctrine.md](doctrine.md) + [rules.md](rules.md): is anything
  believed that was not computed against headers we chose and checked?
- **Latency-half auditor** — walks the waits map against the same files:
  is anything waited for that was already provable — evidence that could
  have traveled, stock that could have been mined ahead?

Both load [checklist.md](checklist.md) when the scope verifies
third-party data (carriers, envelopes, counterparty wires, imports). For
whole-repo runs, each half may split further by cluster — a walker never
takes half a claim set of one package and leaves the rest.

## Prompt contracts

Inventory agents — role preamble; the assigned cluster and starting
paths; the claim/wait field definitions verbatim from SKILL.md step 2;
the result contract. Rule-walkers — role preamble; the merged map
verbatim; the doctrine + rules files to load by path (never names
alone); peer-half territory in one line (a trust observation found by
the latency walker goes in `other_half`, not dropped); the result
contract.

## Structured results

Inventory agent — exactly one JSON object:

```json
{
  "kind": "claims|waits",
  "cluster": "chain",
  "entries": [
    {
      "id": "chain/txstore#mined-1",
      "where": "packages/chain/src/txstore.ts:88",
      "claim": "mined|proven|rejected|spent|unspent|final|funded",
      "carried_by": "BUMP|BEEF|status-string|index-row|host-answer",
      "header_anchor": "which tracker/source",
      "depth_policy": "value or none",
      "waits_on": "for waits: the awaited fact",
      "provable_beforehand": "for waits: yes/no + what evidence/stock was missing"
    }
  ]
}
```

Rule-walker — exactly one JSON object:

```json
{
  "half": "trust|latency",
  "checked_ids": ["every inventory id walked"],
  "findings": [
    {
      "rule": "spv/hint-as-verified|spv/unstocked-lane|…",
      "severity": "BLOCKER|HIGH|MED",
      "inventory_ids": ["the entries involved"],
      "where": "packages/x/src/y.ts:42",
      "believed_or_waited": "what is believed / what is waited for",
      "evidence": "quoted code or command output",
      "fix": "the seam — evidence that travels, or stock ahead of use"
    }
  ],
  "other_half": [{"inventory_id": "…", "observation": "belongs to the other half"}]
}
```

`checked_ids` must equal the map minus stated skips — an entry nobody
walked is a coverage hole the parent reassigns or discloses.

## Verification pass

Every candidate finding goes to a fresh agent — this skill has no
advisory tier, so every finding is worth the check. The verifier tries
to disprove it: does the code actually carry a proof object the walker
missed? Does a floor already bound the wait? Returns `stands` or
`rejected` with evidence. Walkers never verify their own findings.
Rejected candidates are dropped; the parent reports the count — a strict
audit that never rejects anything is a red flag, not a clean bill.

A verifier also rejects any fix that trades proof for speed — if the
only way to remove a wait is to believe something unproven, the finding
stands with that stated, per SKILL.md.

## Anti-patterns

1. Walking claims without the inventory — a "no unproven claims" verdict
   over a partial claims map is worthless; the map is the audit.
2. Splitting a claim from its anchor — the trust question is
   claim + header source together; never assign them to different
   walkers.
3. Severity drift — the verifier enforces BLOCKER/HIGH/MED only; an
   auditor proposing "advisory" entries has misunderstood the skill.
4. Rejection claims without grep-level proof — "nothing verifies this"
   needs the search that shows nothing verifies it.
5. Skipping verification because the finding is obvious — obvious
   BLOCKERs are exactly where a wrong audit does the most damage.
