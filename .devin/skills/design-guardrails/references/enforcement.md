# enforcement — giving structural rules teeth

The other two files are the rules; this file is how the rules stay true
after you close the editor. Three tiers, in decreasing strength:

| Tier       | Mechanism                                    | Strength                              |
| ---------- | -------------------------------------------- | ------------------------------------- |
| Mechanical | `pnpm check:*` gates, type shapes, CI        | runs every build — can't be forgotten |
| Review     | code-review/arch-review rule IDs             | catches per diff / per audit          |
| Authoring  | this skill + AGENTS.md + your own checkpoint | weakest — depends on who's typing     |

**Discipline is the weakest tier.** A rule that only lives in
documentation gets violated by every sufficiently-fast agent session.
When a rule is missed twice — or protects money/durability/custody —
promote it: add the gate, don't strengthen the wish.

## What already enforces what (the gate inventory)

| Gate                        | Catches                                                                                                                                                                                                      | Runs via                         |
| --------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | -------------------------------- |
| `.dependency-cruiser.cjs`   | runtime cycles, orphans, cross-package relative imports, lower→upper layers, devDep/src leaks                                                                                                                | `pnpm check:cruise`              |
| `tools/check-arch.mjs`      | per-package LOC/fanout/instability, god-file thresholds, **the cross-package edge matrix** (authoritative layering — depcruise can't see `@nektar/*` edges), custody/env/broadcaster-construction grep gates | `pnpm check:arch`                |
| `tools/check-leftovers.mjs` | `as any`, `@ts-ignore`, TODO markers, `console.*`, commented-out code — a debt ratchet per package                                                                                                           | `pnpm check:leftovers`           |
| jscpd                       | syntactic copy-paste >5% duplicated lines                                                                                                                                                                    | `pnpm check:dupes`               |
| knip                        | dead files, dead exports, undeclared/unused deps                                                                                                                                                             | `pnpm check:knip`                |
| `tools/check-deps.mjs`      | every import declared in the right package.json                                                                                                                                                              | `pnpm check:deps`                |
| oxlint + `--type-aware`     | correctness bugs, floating/misused promises, dead casts, await-on-sync                                                                                                                                       | `check:lint`, `check:lint:types` |
| prettier + syncpack         | formatting, dep-version pin drift                                                                                                                                                                            | `check:format`, `check:syncpack` |
| `tools/check-secrets.mjs`   | gitleaks scan — secrets/hex-key material in the tree                                                                                                                                                         | `pnpm check:secrets`             |
| tests                       | behavior per package — `pnpm --filter <pkg> test`                                                                                                                                                            | per-package                      |

`pnpm check:hygiene` runs the whole set. Full-history gitleaks +
shellcheck/actionlint/hadolint/zizmor ride in CI.

## How to add a gate — the "how do I enforce this" half

- **A forbidden edge or import pattern** → `forbidden:` rule in
  `.dependency-cruiser.cjs`. Existing shapes to copy: `no-lower-to-upper`,
  `no-automation-to-mcp`, `no-cross-package-relative-*`. Caveat:
  workspace `@nektar/x` imports resolve into dist/ — depcruise can't see
  them. Package-direction contracts belong in check-arch's edge matrix.
- **A textual invariant** ("raw X may only appear in file Y") → a grep
  gate in `tools/check-arch.mjs`. Existing examples: raw
  `process.env.NEKTAR_*` outside `core/src/env.ts` fails; raw
  `new TeranodeBroadcaster`/`new BsvBroadcaster` outside composition
  roots (mcp runtime, chain regtest/testkit) fails — every funded spend
  sits behind `SpendGuard`. This is the cheapest strong gate; prefer it
  for "construction-site" and "custody" rules.
- **A ratchet** ("count may only shrink") → extend the baseline:
  `tools/.leftover-baseline.json` or `tools/.arch-baseline.json`.
  Regenerate deliberately with `--update` — never bump a baseline to
  make a check pass; that erases the ratchet's memory.
- **A boundary contract** (persisted shapes, wire envelopes) → encode it
  in the _reader_: a decode function that owns all historical shapes and
  refuses ambiguity. A tolerant reader is the strongest schema gate —
  it runs at runtime, not just CI.
- **A design rule linters can't see** → a rule-ID row in
  `../code-review/references/{structure,data-systems,stability,nektar}.md`
  so every diff review applies it uniformly.
- **A generated artifact** → explicit allowlist entry with a comment
  naming the generator (see `(Nektar|DisclosureSale)*.json`,
  `stark/verifier.js` in `.dependency-cruiser.cjs`) — silent ignores rot.

## The promotion ladder — same move at every tier

1. **Shape the type** so the violation is unrepresentable (make illegal
   states unrepresentable — DDIA's abstractions + POSD's hiding).
2. **Gate it** — cruise rule, check-arch grep gate, or ratchet entry.
3. **Name it in review** — a rule ID the review skill applies per diff.
4. **Write it in AGENTS.md** — the convention layer; necessary but
   weakest. Always pair with tier 1–3 or accept that it WILL be missed.

SpendGuard's composition-root gate is the worked example: "every funded
spend behind the guard" was a social rule until the grep gate made
construction sites build-failing. Every new money/durability/custody
path should ship with the gate that would catch its own violation —
that's a fitness function, and it's what separates enforced patterns
from aspirations.

## Naming a missing gate in your report

When the design checkpoint surfaces an unprotected rule, say so —
this phrasing, verbatim, is a finding worth reporting:

> "New invariant `<rule>` is currently enforced only by convention —
> propose `<gate>` in `<file>` to make it mechanical."

Missing-gate tells that mean "say it, don't fix it silently":

- You relied on convention to keep a type/edge/state honest.
- Two components share a contract invisible to the module system —
  an env var read in >1 package, a `.state/` path written by >1
  component, a SQLite table opened by >1 writer (ambient coupling,
  invisible to dep-cruiser).
- A review finding repeats from an earlier review — recurring findings
  are the cheapest possible signal for promotion.
