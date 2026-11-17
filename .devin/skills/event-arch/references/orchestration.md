# orchestration — multi-agent audit runs

How an event-arch audit fans out. Default is inline — the event surface
of a diff is usually small enough to walk in one pass. Swarm on
whole-repo/whole-subsystem runs, explicit full-audit requests, or any
scope where the event surface (waits, subscriptions, outbox rows,
streams, deliveries) is too wide to inventory and question in one pass.

## Roles

- **Parent** — the agent holding this skill. Scopes the run, owns the
  event-surface inventory, launches agents, dedupes findings, and writes
  the event-surface map + realtime score. Only the parent consolidates —
  agents never see each other's output.
- **research agent** — read-only delegated explorer (Devin:
  `subagent_explore`). Runs inventory agents, question-family auditors,
  and fresh verifiers — this is all source reading.
- **general agent** — delegated worker (Devin: `subagent_general`).
  Only needed when a verifier must run code to settle a claim.

Use the platform's equivalents, preserving three boundaries: each agent
gets exactly its assigned units, agents return structured results
instead of writing shared state, and no agent verifies its own finding.

## Coverage plan

Two stages, because the questions interrogate an inventory, not files:

**Stage 1 — inventory.** One research agent per subsystem slice
(`chain`, `automation`, `bstack`, `mcp`/`watcher`, or the scope's own
packages) returns every wait/sleep/poll/subscription/outbox-row/
fetch-loop/stream/delivery site it finds, each classified: what it waits
on, what wakes it, what proves it, what bounds it. Small scopes need one
agent. The parent merges these into the event-surface inventory — the
coverage claim.

**Stage 2 — question families.** One auditor per family, each taking the
full inventory plus source access:

- `ev` — event-loop questions 1–10 from SKILL.md. Always runs.
- `fab` — fabric questions 11–20. Runs when the scope touches
  bstack/transport/delivery (lanes, finger, tree, journal, market-wire,
  watch-shim, host fanout, artifact publish/fetch).
- `spv` — proof questions 21–28. Runs when the scope touches
  proofs/status/inclusion (observe paths, BUMP/BEEF, ChainTracker,
  tx fetch/status, "is it mined").

An inventory entry no auditor's family covers is `deferred` and
disclosed. When a `fab` or `spv` family doesn't run, say so in the
report — the questions it didn't answer are out of scope, not clean.

## Inventory agent prompt contract

1. Role preamble (two sentences): enumerate the event surface of the
   assigned slice; return exactly one JSON object.
2. The slice's packages and starting paths; peer slices one line each.
3. The classification fields (waits-on / wakes-it / proves-it /
   bounds-it) verbatim from SKILL.md step 2.
4. The structured result contract below.

## Auditor prompt contract

1. Role preamble (two sentences): walk the assigned question family over
   the inventory; return exactly one JSON object.
2. The merged event-surface inventory, verbatim.
3. The family's question table, verbatim from SKILL.md.
4. The reference files to load by path:
   [principles.md](principles.md) always; [fabric.md](fabric.md) for
   `fab`; [spv.md](spv.md) for `spv`. Never send file names alone.
5. The fix-catalog pointer: [patterns.md](patterns.md) — every finding
   names the in-repo precedent to copy.
6. The structured result contract below.

## Structured results

Inventory agent — exactly one JSON object:

```json
{
  "slice": "bstack",
  "surface": [
    {
      "id": "bstack/lanes#sub-1",
      "kind": "wait|sleep|poll|subscription|outbox-row|fetch-loop|stream|delivery",
      "where": "packages/bstack/src/lanes.ts:120",
      "waits_on": "what event/state",
      "wakes_it": "producer signal | bounded poll | nothing",
      "proves_it": "verified read/proof | trusted hint | nothing",
      "bounds_it": "ceiling value | unbounded"
    }
  ],
  "uncovered_files": ["paths suspected but not confirmed event code"]
}
```

Auditor — exactly one JSON object:

```json
{
  "family": "ev|fab|spv",
  "checked_ids": ["every inventory id walked"],
  "findings": [
    {
      "rule": "ev/lost-wakeup|fab/unnumbered-stream|spv/hint-as-verified",
      "severity": "BLOCKER|HIGH|MEDIUM",
      "inventory_ids": ["the sites involved"],
      "where": "packages/x/src/y.ts:42",
      "principle": "the violated principle",
      "evidence": "quoted code",
      "fix": "smallest move + the seam to reuse"
    }
  ]
}
```

`checked_ids` must equal the inventory minus stated skips — an id
nobody walked is a coverage hole the parent reassigns or defers.

## Verification pass

Two finding classes always go to a fresh agent, which tries to disprove
them and returns `stands`/`rejected` with evidence:

1. Every BLOCKER (`ev/unverified-status` on money paths, custody-host
   claims).
2. Every absence claim — "the producer does not signal on path X",
   "no cursor resume exists", "no proof object is carried". Absence is
   cheap to assert and expensive to be sure of; the verifier greps for
   the mechanism before the finding stands.

Auditors never verify their own findings. Rejected candidates are
dropped; the parent reports the count.

## Anti-patterns

1. Auditing without inventory — questions asked against a partial
   surface produce confident partial answers.
2. Splitting by file instead of by family — a lost wakeup needs the
   producer AND the consumer in view; file-splitting hides the pair.
3. Findings without an `inventory_ids` link — the fix maps to a surface
   entry, not a vibe.
4. Treating a skipped family as a passed family — `fab`/`spv` out of
   scope is disclosed, never scored clean.
5. Removing the poll floor in a suggested fix — wakes are hints, polls
   carry correctness; verifiers reject fixes that trade the floor away.
