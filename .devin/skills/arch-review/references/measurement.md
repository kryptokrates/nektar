# Measurement playbook — gather before judging

Run these to build the evidence base. All commands are Git-Bash-compatible
from the repo root. Capture output into the report — evidence travels with
the finding.

## 1. Package inventory

```bash
for p in packages/*/src; do
  pkg=${p%/src}
  echo "$(find $p -name '*.ts' | wc -l) $(find $p -name '*.ts' -exec cat {} + | wc -l) ${pkg#packages/}"
done | sort -rn -k2
```

Produces: files, LOC, package. Also count exports per package as a surface proxy:

```bash
for p in packages/*/src/index.ts; do
  echo "$(grep -c '^export' $p 2>/dev/null) ${p%/*}"
done | sort -rn
```

Flag: any package >5k src LOC or >40 exports is a god-package candidate.

## 2. Real dependency edges (package level)

Declared edges come from each `package.json` (`@nektar/*` entries). Verify the
declared graph matches imports, and catch undeclared/sneaked edges:

```bash
pnpm exec depcruise --output-type err packages/*/src --config .dependency-cruiser.cjs
# machine-readable for your own aggregation:
pnpm exec depcruise --output-type json packages/*/src > /tmp/depmap.json
```

Aggregate package-to-package edges by mapping each resolved path to its
`packages/<name>/` prefix. Compute per package:
- **Ca** (afferent): distinct packages that import it
- **Ce** (efferent): distinct packages it imports
- **I** (instability) = `Ce / (Ca + Ce)`; `Ce+Ca=0` ⇒ orphan

Expected shape (from `.dependency-cruiser.cjs`): `core` I≈0 (stable root),
`mcp/e2e/fuzz/watcher` I≈1 (volatile leaves). **Deviations from the declared
shape are findings.** e.g. `chain` or `log` gaining upward deps is layering
decay even if dep-cruiser still passes.

## 3. Hub / boundary leak detection

```bash
# who imports what from chain — the busiest lower-layer package
rg -l "from '@nektar/chain'" packages/*/src | sed 's|/src/.*||' | sort -u
# index surface: does a package re-export internals it shouldn't?
rg "^export \* from" packages/*/src/index.ts
```

An `export *` that re-exposes another module's whole surface is an
ambiguous-interface smell (consumers can't tell what the package promises).

## 4. Hotspots — churn × size (Tornhill)

```bash
git log --since="6 months ago" --format="@@@%H" --numstat -- "packages/*/src/*.ts" \
  | awk '/^@@@/{c++;next} NF==3{n[$3]++} END{for(f in n) print n[f], f}' \
  | sort -rn | head -25
```

Join with LOC (`wc -l` on each). **Hotspot = high revisions AND high LOC.**
High-churn small files are normal; big files nobody touches are cold legacy
(see #6) — the quadrant that matters is big AND hot.

Then the same at **package** altitude:

```bash
git log --since="6 months ago" --name-only --format="@@@%H" -- "packages/*/src" \
  | rg -o "packages/[^/]+" | sort | uniq -c | sort -rn
```

A package with outsized change share + high instability (I) is where the
architecture is fighting the work.

## 5. Temporal coupling (hidden coupling)

Files that change together but have no import edge = coupling invisible to
the compiler:

```bash
git log --since="6 months ago" --name-only --format="@@@%H" -- "packages/*/src" \
  | awk '/^@@@/{if(c>1)for(i in s)for(j in s)if(i<j)p[i"|"j]++;delete s;c=0;next}
         {s[$1]=1;c++} END{for(k in p)print p[k],k}' \
  | sort -rn | head -20
```

Check each top pair: does a real import edge exist between them? Pairs that
co-change constantly with **no** import = ambient coupling (shared schema,
env contract, state-dir layout, or copy-pasted logic). These are shotgun-
surgery sites and prime extraction candidates.

## 6. Cold + debt inventory

```bash
pnpm report:cold 40          # files by days-since-last-commit
cat tools/.leftover-baseline.json   # per-package debt ratchet: any/ts-ignore/TODO/console
pnpm exec jscpd packages/*/src --silent 2>/dev/null | tail -15   # dupes (also gated)
```

Cold files that ARE imported = legacy to review. Cold files nothing imports
= removal candidates (cross-check `check:knip`). A package whose baseline
counts are high AND is in the money path = concentrated risk.

## 7. Shared-state coupling (not in the import graph)

```bash
# env-var surface — every NEKTAR_* read is an ambient contract
rg -o "process\.env\.NEKTAR_[A-Z_]+" packages/*/src --no-filename | sort | uniq -c | sort -rn
# SQLite writers — who creates/opens the same DB files
rg "better-sqlite3|new Database" packages/*/src -l
# state-dir paths — shared filesystem contracts
rg -o "\.state/[a-z-]+" packages/*/src | sort -u
```

Two packages writing the same DB file, or a `.state/` path understood by
three packages, is coupling that survives a package-graph rewrite — map it.

## 8. Fitness-function audit

Nektar already automates (run them, quote pass/fail):

| Gate | What it enforces architecturally |
|---|---|
| `pnpm check:cruise` | no runtime cycles, no orphans, layering rules (lower→upper, bstack→mcp), no cross-package relative imports |
| `pnpm check:knip` | dead files/deps/exports |
| `pnpm check:dupes` | copy-paste structure >5% |
| `pnpm check:leftovers` | per-package debt ratchet (only shrinks) |
| `pnpm check:deps`/`syncpack` | undeclared imports / version drift |
| `pnpm report:cold` | legacy inventory (report-only) |

**The audit:** for each structural rule you *conclude* the system needs,
check whether a gate exists. Rules enforced only by convention (e.g. "new
packages must not hold keys", "SpendGuard must see every spend") belong in
the findings as **unenforced invariants** — the highest-value gap class an
architecture review can surface.

## Timeboxing

A monorepo this size: inventory+graph ≈10 min, hotspots ≈5, coupling ≈5,
cold/debt ≈5, fitness audit ≈10, smell pass ≈20-30. If a step stalls, mark
it "not measured" in the report — never fabricate the number.
