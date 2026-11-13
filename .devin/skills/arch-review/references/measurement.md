# Measurement playbook — gather before judging

Run these to build the evidence base. All commands are Git-Bash-compatible
from the repo root. Capture output into the report — evidence travels with
the finding.

**Zero rows is a command failure, not a finding.** Every measurement must
return data before it can be quoted as evidence — git pathspecs like
`-- "packages/*/src"` silently match nothing on some git builds (wildmatch
quirk), so the commands below use `-- packages` + awk/rg path filtering
instead. If a step returns empty, fix the command; never report "none found"
from an empty result set.

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

**Known blind spot:** depcruise resolves workspace `@nektar/x` imports through
node_modules into `dist/`, and `exclude: '(^|/)dist/'` then drops them — the
cruise graph contains **zero cross-package edges**. App packages without lib
`exports` fail as `couldNotResolve` (still an error), but resolvable lib
imports vanish silently — so `to.path` layering rules can't see them. Measure
package edges at the **specifier level** instead:

```bash
rg -o "from '(@nektar/[a-z-]+)" packages/*/src --no-filename | sort | uniq -c | sort -rn
# per-package edge matrix (what check-arch.mjs ratchets):
node tools/check-arch.mjs --update && cat tools/.arch-baseline.json | node -e \
  "const b=require('/dev/stdin');for(const[p,m]of Object.entries(b.packages))console.log(p,'→',m.edges)"
```

Compute per package:

- **Ca** (afferent): distinct packages that import it
- **Ce** (efferent): distinct packages it imports
- **I** (instability) = `Ce / (Ca + Ce)`; `Ce+Ca=0` ⇒ orphan

Expected shape: `core` I≈0 (stable root), `mcp/e2e/watcher` I≈1 (volatile
leaves). **Deviations from the declared shape are findings.** e.g. `chain` or
`log` gaining upward deps is layering decay even if dep-cruiser still passes.
`check:arch` fails on any _new_ edge until admitted via `--update`.

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

Exclude generated artifacts before measuring — `packages/contracts/Nektar*.json`
(`buildTimestamp` churn), `stark/verifier.js`, `*.d.ts` and `vendor/` pollute
the signal (a file that re-generates on every build is not architectural
activity):

```bash
git log --since="6 months ago" --format="@@@%H" --numstat -- packages \
  | awk '/^@@@/{c++;next} NF==3 && $3 ~ /^packages\/[^\/]+\/src\/.*\.ts$/{n[$3]++} END{for(f in n) print n[f], f}' \
  | rg -v "Nektar.*\.json|verifier\.(js|wasm)|\.d\.ts|vendor/|\.runar\.ts" \
  | sort -rn | head -25
```

Join with LOC (`wc -l` on each). **Hotspot = high revisions AND high LOC.**
High-churn small files are normal; big files nobody touches are cold legacy
(see #6) — the quadrant that matters is big AND hot.

Then the same at **package** altitude:

```bash
git log --since="6 months ago" --name-only --format="@@@%H" -- packages \
  | rg -o "packages/[^/]+/src/" | sort | uniq -c | sort -rn
```

A package with outsized change share + high instability (I) is where the
architecture is fighting the work.

## 5. Temporal coupling (hidden coupling)

Files that change together but have no import edge = coupling invisible to
the compiler:

```bash
git log --since="6 months ago" --name-only --format="@@@%H" -- packages \
  | awk '/^@@@/{if(c>1)for(i in s)for(j in s)if(i<j)p[i"|"j]++;delete s;c=0;next}
         $1 ~ /^packages\/[^\/]+\/src\//{s[$1]=1;c++} END{for(k in p)print p[k],k}' \
  | rg -v "Nektar.*\.json|verifier\.(js|wasm)|\.d\.ts|vendor/|\.runar\.ts" \
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
# env-var surface — every NEKTAR_* read is an ambient contract. Shipping code
# reads through @nektar/core's nektarEnv() (check:arch gates raw process.env
# reads); count BOTH patterns to map the contract:
rg -o "nektarEnv\('NEKTAR_[A-Z_]+'|process\.env\.NEKTAR_[A-Z_]+" packages/*/src --no-filename | sort | uniq -c | sort -rn
# SQLite writers — who creates/opens the same DB files
rg "better-sqlite3|new Database|new AutomationStore|new Knex" packages/*/src -l
# state-dir paths — shared filesystem contracts
rg -o "\.state/[a-z-]+" packages/*/src | sort -u
```

Two packages writing the same DB file, or a `.state/` path understood by
three packages, is coupling that survives a package-graph rewrite — map it.

## 8. Fitness-function audit

Nektar already automates (run them, quote pass/fail):

| Gate                         | What it enforces architecturally                                                                                                                           |
| ---------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `pnpm check:cruise`          | no runtime cycles, no orphans, no cross-package relative imports, unresolvable/undeclared imports                                                          |
| `pnpm check:arch`            | package-edge ratchet + per-package LOC/fanout/instability ratchet + god-file thresholds + layering matrix + custody/env grep gates + type-only cycle audit |
| `pnpm check:knip`            | dead files/deps/exports                                                                                                                                    |
| `pnpm check:dupes`           | copy-paste structure >5%                                                                                                                                   |
| `pnpm check:leftovers`       | per-package debt ratchet (only shrinks)                                                                                                                    |
| `pnpm check:deps`/`syncpack` | undeclared imports / version drift                                                                                                                         |
| `pnpm report:cold`           | legacy inventory (report-only)                                                                                                                             |

`check:cruise` and `check:arch` are complementary: cruise sees the resolved
intra-package graph (cycles, orphans, resolution), check-arch sees the
cross-package specifier graph cruise is blind to.

**The audit:** for each structural rule you _conclude_ the system needs,
check whether a gate exists. Rules enforced only by convention (e.g. "new
packages must not hold keys", "SpendGuard must see every spend") belong in
the findings as **unenforced invariants** — the highest-value gap class an
architecture review can surface.

## 9. Concept census — semantic duplication (type-4 clones)

`check:dupes` (jscpd) only sees type-1..3 clones — copy-paste with minor
edits. **Convergent implementations** (same concept, independently written,
different syntax) are invisible to it and are the usual shape of "we have
three versions of this". Count domain concepts per location:

```bash
# remote-call wrappers — each site carrying its own timeout/error shape
rg -l "await fetch\(|\.json\(\)" packages/*/src | sed 's|/src.*||' | sort | uniq -c | sort -rn
# spend/broadcast construction — who builds txs vs who should
rg -ln "new Transaction|\.addInput|\.addOutput|broadcast\(" packages/*/src | sed 's|/src.*||' | sort | uniq -c | sort -rn
# retry/backoff helpers — duplicated resilience logic
rg -l "attempt|backoff|retries" packages/*/src | sed 's|/src.*||' | sort | uniq -c | sort -rn
# persistence openers — who opens the same stores
rg -ln "new Database|\.sqlite|knex" packages/*/src | sed 's|/src.*||' | sort | uniq -c | sort -rn
# hash/digest helpers — crypto primitives re-wrapped
rg -l "createHash|sha256\(" packages/*/src | sed 's|/src.*||' | sort | uniq -c | sort -rn
# sleep/promise-wait helpers
rg -ln "function sleep|const sleep|setTimeout.*resolve" packages/*/src
```

Same concept in 3+ locations = candidate cluster. **Compare contracts
before calling it a dupe**: list each site's signature (inputs → outputs →
failure modes). Two sites that take the same inputs and produce the same
effect are one implementation wearing two syntaxes — consolidate. Sites
with genuinely different contracts are separate concepts sharing a verb —
say so and move on. Temporal-coupling pairs (#5) are a second detector:
files that co-change without an import edge often duplicate knowledge.

## 10. Patch stratigraphy — fix-density

Commits are labeled strata: a file whose history is mostly fix-keyword
commits is accumulating patches on earlier patches, which means the
abstraction underneath is wrong or missing:

```bash
git log --since="6 months ago" --format="@@@%s" --name-only -- packages \
  | awk '/^@@@/{s=tolower($0); f=(s ~ /fix|bug|regress|patch|broke/);next}
         $1 ~ /^packages\/[^\/]+\/src\//{n[$1]++; if(f)fx[$1]++}
         END{for(k in n) if(n[k]>=5) printf "%d/%d %.0f%% %s\n", fx[k]+0, n[k], 100*fx[k]/n[k], k}' \
  | sort -rn | head -20
```

>40% fix commits with ≥5 touches = stratified site. Caveats: refactor
commits named "fix" mislead the ratio — read the cluster before confirming.
Short histories (<1 month) give high-resolution but thin signal; say so.

Also check **infant files** — created recently, already patched:

```bash
for f in $(git log --since="30 days ago" --diff-filter=A --name-only --format= -- packages | rg "packages/[^/]+/src/" | sort -u); do
  [ -f "$f" ] || continue   # skip files already deleted (migrations, renames)
  t=$(git log --oneline -- "$f" | wc -l); c=$(git log --format="%s" -- "$f" | rg -ci "fix|bug|regress|patch|broke" || echo 0)
  [ "${t:-0}" -gt 2 ] && echo "${c:-0}/$t $f"
done | sort -rn | head -15
```

A file needing 3+ fixes within weeks of birth was born with the wrong
contract. Feed the top of both lists into the root-cause drill in
[consolidation.md](consolidation.md) — the deliverable is the missing
abstraction, not patch N+1.

## 11. Timer & wait census

Every bounded wait is latency the system pays forever; every unbounded one
is a hang it will eventually pay once. Census all of them:

```bash
for p in packages/*/src; do
  c=$(rg -c "setTimeout|setInterval|AbortSignal\.timeout|sleep\(|new Promise.*setTimeout" $p 2>/dev/null | awk -F: '{s+=$2} END{print s+0}')
  echo "$c ${p%/src}"
done | sort -rn
```

Then classify each site along four axes:

- **Bounded?** `AbortSignal.timeout`/explicit deadline = bounded; a bare
  `setTimeout(r, N)` promise with no sibling cancellation = unbounded.
- **Event-driven or fixed poll?** A fixed sleep waiting on a condition that
  already emits an event/wake = `nektar/fixed-sleep` raised to arch
  altitude — it wastes the interval on every iteration.
- **Configured or hardcoded?** `NEKTAR_*_MS`-style vars vs bare literals.
  Bare literals at 3+ sites with different values for the same wait =
  convention drift; none of the sites can be tuned together.
- **Stacked?** Retry-in-loop inside retry-in-outbox inside retry-in-caller
  multiplies effective attempts — multiply the layers for the real bound.

Flag: fixed polls on money paths, unbounded waits that can pin a resource
(`stab/hung-resource` is the diff-level cousin), and timer constants that
disagree with the documented conventions.

## 12. SPoF & resilience inventory

For each money/durability path, count the redundancy and name which
failure layers exist. Release It!'s layers: per-call (timeout) → component
(bulkhead / circuit-breaker) → system (load-shed / governor) → ops
(steady-state recycler) → recovery (failover / rebuild-from-source).

```bash
# spend-path implementations — how many, and who may construct them
rg -ln "Broadcaster|SpendGuard|BudgetAuthority" packages/*/src tools/ | sed 's|/src.*||;s|tools/|tools|' | sort | uniq -c
# store writers — one writer per DB or many?
rg -ln "new Database|\.sqlite|knex" packages/*/src
# host fanout — paths pinned to a single host vs N>1
rg -n "broadcasterUrl|hostUrls|hosts\[" packages/*/src | head -25
```

For each path: if its one implementation/writer/host fails, does anything
take over — active-active fanout (bstack multi-host is the existing
example), hot standby, or rebuild-from-source — or does the system degrade
to silent stall? A derived-state store with no rebuild path is a SPoF even
with redundant readers. Feeds S9 in smells.md.

## 13. Event & wake topology (EDA audit)

An autonomous-agent system should be **event-driven end to end**: chain
events, inbox deliveries, outbox drains, and producer signals drive all
work — timers exist only as bounded ceilings and at the poll substrate.
Audit whether that's true.

First, inventory the primitives — who emits, who waits, who signals:

```bash
# emission + wait points per package
rg -ln "EventEmitter|\.emit\(|waiters|\.on\('|waitFor" packages/*/src | sed 's|/src.*||' | sort | uniq -c | sort -rn
# wake/signal primitives — the producer→waiter couplings
rg -n "unblock|wake|nudge|notify|signal|resolveWait|long-?poll" packages/*/src | head -30
```

Then classify **every wait site from #11** into exactly one bucket:

- **Retry-on-failure** — backoff retrying an operation that FAILED
  (429/503/5xx, SSE reconnect). Legitimate pattern; the only finding is
  duplicated implementations (feed #9).
- **Event-substitute** — a poll/sleep waiting for a STATE CHANGE: a UTXO
  arriving, a flag clearing, a row appearing. If a producer signal exists
  (`unblockOutbox`, `ChainEvents` emit, watch-shim wake) the wait is a bug
  shaped like a loop; if no signal exists, the missing signal is the
  finding — not the poll.
- **Substrate** — the one place polling IS the design (ChainEvents' tick
  scanning `watched`, the automation idle/hot tick). Exactly one substrate
  per subsystem; N substrates = scheduling sprawl.

Finally map **waiter→signaler pairs**: for each `waiters`/`unblock`/wake
consumer, enumerate every path that can produce the awaited condition and
check it signals on ALL paths — success AND error. A producer that signals
only on its happy path is a lost-wakeup (`data/lost-wakeup` raised to arch
altitude: map it, don't grep-hunt it). The reverse check matters too:
a signaler with no waiter is dead signaling machinery.

## Timeboxing

A monorepo this size: inventory+graph ≈10 min, hotspots ≈5, coupling ≈5,
cold/debt ≈5, fitness audit ≈10, censuses (#9-#13) ≈20, smell pass
≈20-30. If a step stalls, mark it "not measured" in the report — never
fabricate the number.
