# Smell catalog — detection rules mapped to nektar evidence

Seven canonical architectural smells (Lippert/Rook, arc42) + nektar-specific
structural smells. Each: detection rule → evidence source → nektar notes.
Severity per SKILL.md; every finding needs its measured evidence attached.

## Canonical seven

### 1. Cyclic dependency
- **Rule**: package/module dependency cycles.
- **Evidence**: dep-cruiser `no-circular` gates *runtime* cycles. **Type-only
  cycles are still cycles** — tsc erases them, so grep `import type` edges
  that close a loop. Two packages that need each other's types are usually
  one package, or missing a third.
- **Nektar**: a `core ↔ chain` type cycle would hide in exactly this gap.

### 2. Unstable dependency (direction violation)
- **Rule**: a stable package (low I) depending on a volatile one (high I) —
  the arrow points wrong way.
- **Evidence**: Ca/Ce/I table from measurement #2. Compare each edge against
  the declared layering: lower-layer (`core/crypto/log/chain`) → higher-layer
  imports are enforced; check also **mid-tier** violations (`anchor→mcp`-shaped
  edges would bypass the cruise rule since it only names the top tier).
- **Nektar**: `chain` importing `bstack`-or-app-level concepts (host URLs,
  envelope formats) inverts the transport boundary — dep-cruiser only guards
  the package edge, not the *concept*.

### 3. Hub-like dependency
- **Rule**: high fan-in AND high fan-out on one component — everything flows
  through it; it's both hard to change and easy to break.
- **Evidence**: top Ca+Ce from #2; file-level fan-in/out via import counts.
- **Nektar**: `core` is the designed hub (I≈0, high Ca — correct: it should
  be boring and stable). Alarm case: `mcp` or `chain` accumulating fan-in
  while keeping high fan-out — a hub in a volatile position.

### 4. God component
- **Rule**: one component doing too much — proxy: LOC + file count + export
  surface + number of distinct concerns it serves.
- **Evidence**: #1 inventory (>5k LOC or >40 exports → candidate). Read its
  index: if consumers need unrelated slices of the same package, it's god.
- **Nektar**: check `mcp/src/memory.ts` & `runtime.ts` — if the memory engine,
  HTTP surface, wallet custody AND automation loop live in one package,
  that's god-by-accretion even if each file is clean.

### 5. Feature concentration
- **Rule**: one component implements a disproportionate share of features —
  or one feature's implementation smeared across many components.
- **Evidence**: per-package changelog share (#4 package-altitude hotspots) +
  pick one feature (e.g. "disclosure sale") and list every package it
  touches: contracts, anchor, verify, mcp, automaton, bstack… 4+ packages for
  one user-facing feature = feature smear; check each touch is *necessary*
  (boundary of responsibility) vs *incidental* (the feature reached across).
- **Nektar**: the sale flow is the canonical trace target.

### 6. Scattered functionality
- **Rule**: one responsibility split across components that must be changed
  together — temporal coupling shows this before imports do.
- **Evidence**: #5 co-change pairs with no import edge; duplicated logic from
  `check:dupes`.
- **Nektar**: watch for *chain-knowledge* scattered — BSV wire formats,
  tx construction, BEEF handling existing in both `chain` and callers
  (`mcp`, `automaton`, `bstack` reimplementing tx parsing) = the ledger
  abstraction leaking.

### 7. Ambiguous interface
- **Rule**: boundary doesn't say what it promises — `export *`, leaking
  internal types, entry points bypassing index, or interfaces serving
  multiple unrelated audiences.
- **Evidence**: `export *` inventory (#3); `knip` unused-export list;
  packages imported for a single symbol vs packages used deep.
- **Nektar**: check whether `@nektar/chain`'s index admits both "broadcaster"
  and "testkit" consumers — two audiences behind one surface = the
  interface is lying about its contract.

## Nektar-specific structural smells

### S1. Ambient coupling (env / fs / db)
- **Rule**: contracts that exist outside the module system — `NEKTAR_*` env
  vars, `.state/` files, shared SQLite tables.
- **Evidence**: #7 inventory. Every env var read in >1 package, every
  `.state/` path written by >1 component, every DB file opened by >1 writer.
- These are **invisible to dep-cruiser** — the worst class for "it passed
  the graph check so it must be decoupled".

### S2. Unenforced invariant
- **Rule**: a structural rule the system depends on that no gate checks.
- **Evidence**: fitness audit #8. Candidates in this codebase: "apps never
  hold keys" (custody), "every automated spend flows through SpendGuard",
  "envelope payloads stay under MaxContent", "broadcaster hints never drive
  money decisions". If a new package could violate it tomorrow without CI
  noticing → HIGH finding with a concrete gate proposal.

### S3. Trust-boundary smearing
- **Rule**: code that treats unverified input (broadcaster statuses, inbox
  payloads, host responses) as verified facts *at the boundary* — the
  verified/unverified distinction must be visible in the type system or
  package structure, not just in comments.
- **Evidence**: trace one untrusted input end-to-end (e.g. a pushed
  `TxStatusEvent`) — does the type change when trust does (`Observed` vs
  `Noted`)? Or is it one `status` string everywhere?

### S4. Restart-invisible state
- **Rule**: state living in process memory that the architecture assumes is
  durable — the agent-restart story is load-bearing here.
- **Evidence**: map each long-lived subsystem: op journals (SQLite),
  outbox rows (SQLite), `watched` txids (memory), event waiters (memory),
  `lastStatus` (memory). For each in-memory map: what breaks on restart?
  `ChainEvents.watched` is reconstructed by `broadcast` — good. Any map
  whose loss = double-spend risk or stuck deal = CRITICAL.

### S5. Boundary bypass — bundle/image skew
- **Rule**: code that runs in two places (repo + host bundle + docker image)
  where the versions can differ — the image boundary is invisible to the
  repo's own gates.
- **Evidence**: `bundle-mcp.mjs` externals list; compare `@bsv/sdk` versions
  in `pnpm-lock` vs the image. Any dep marked `--external` in the bundle is
  a version-skew liability — that's the SDK 2.7.1/4.0.1 incident class,
  already burned once.

## Severity calibration (architecture altitude)

| Sev | Meaning here |
|---|---|
| CRITICAL | money/durability/custody boundary structurally compromised; a component that can't change without coordinated system-wide edits |
| HIGH | measured drag — hub in a volatile spot, unenforced money invariant, ambient coupling on a stateful contract |
| MEDIUM | compounding smell — fine today, expensive at 2× contributors or 2× features |
| LOW | impurity worth noting, cheap to defer |

Not every heuristic match is a true positive — check for a **declared
rationale** (dep-cruiser comment, AGENTS.md, ADR-style comments) before
calling something a smell. Documented intentional coupling is a trade-off,
not a defect; say so in the report.
