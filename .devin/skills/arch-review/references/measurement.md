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

## 1. Package inventory — the gate already computes this

`tools/.arch-baseline.json` (regenerate: `node tools/check-arch.mjs --update`)
carries per-package **files, LOC, fan-out, and instability I** — the numbers
this section used to hand-roll. Read it, don't recompute:

```bash
node -e "const b=require('./tools/.arch-baseline.json');for(const[p,m]of Object.entries(b.packages))console.log(m.files??'',m.loc??'',p,'I='+(m.instability??'?'),'edges:',m.edges)"
```

Flag: any package >5k src LOC or >40 exports is a god-package candidate
(exports proxy: `grep -c '^export' packages/*/src/index.ts`).

Expected shape: `core` I≈0 (stable root), `mcp/e2e/watcher` I≈1 (volatile
leaves). **Deviations from the declared shape are findings** — e.g. `chain`
or `log` gaining upward deps is layering decay. `check:arch` fails on any
_new_ edge until admitted via `--update`.

## 2. Real dependency edges — gate first, spec-grep to verify

`pnpm check:cruise` catches cycles/orphans/undeclared imports; `check:arch`
ratchets the cross-package edge matrix. **Known blind spot:** depcruise
resolves workspace `@nektar/x` imports into `dist/` and its own exclude
drops them — the cruise graph contains **zero cross-package edges**;
check-arch reads specifiers directly and is authoritative. To verify the
declared graph (each `package.json`'s `@nektar/*` deps) matches real
imports, count specifiers:

```bash
rg -o "from '(@nektar/[a-z-]+)" packages/*/src --no-filename | sort | uniq -c | sort -rn
```

Undeclared-but-imported edges are findings; declared-but-unused edges feed
`check:knip`/`check:deps`.

## 3. Hub / boundary leak detection

```bash
rg -l "from '@nektar/chain'" packages/*/src | sed 's|/src/.*||' | sort -u   # who imports the busiest lower layer
rg "^export \* from" packages/*/src/index.ts                               # whole-surface re-exports = ambiguous interface
```

## 4. Hotspots — churn × size (Tornhill)

No gate computes churn. Exclude generated artifacts (`Nektar*.json`
timestamp churn, `stark/verifier.js`, `*.d.ts`, `vendor/`, `*.runar.ts`) —
a regenerated file is not architectural activity:

```bash
git log --since="6 months ago" --format="@@@%H" --numstat -- packages \
  | awk '/^@@@/{c++;next} NF==3 && $3 ~ /^packages\/[^\/]+\/src\/.*\.ts$/{n[$3]++} END{for(f in n) print n[f], f}' \
  | rg -v "Nektar.*\.json|verifier\.(js|wasm)|\.d\.ts|vendor/|\.runar\.ts" \
  | sort -rn | head -25
```

Join with LOC. **Hotspot = high revisions AND high LOC** — the big-and-hot
quadrant is what matters. Package-altitude version (`--name-only` +
`rg -o "packages/[^/]+/src/" | sort | uniq -c`): a package with outsized
change share + high I is where the architecture fights the work.

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

Top pairs with **no** import edge = ambient coupling (shared schema, env
contract, state-dir layout, copied logic) — shotgun-surgery sites and prime
extraction candidates.

## 6. Cold + debt inventory — all gate-produced

```bash
pnpm report:cold 40                       # files by days-since-last-commit
cat tools/.leftover-baseline.json         # per-package debt ratchet (any/ts-ignore/TODO/console)
pnpm check:dupes                          # jscpd >5% gate
```

Cold + imported = legacy to review. Cold + unimported = removal candidates
(cross-check `check:knip`). High baseline counts in a money-path package =
concentrated risk.

## 7. Shared-state coupling (not in the import graph)

```bash
# env surface — every NEKTAR_* read is an ambient contract; count BOTH patterns
rg -o "nektarEnv\('NEKTAR_[A-Z_]+'|process\.env\.NEKTAR_[A-Z_]+" packages/*/src --no-filename | sort | uniq -c | sort -rn
# SQLite writers + shared .state/ paths — coupling that survives a package-graph rewrite
rg "better-sqlite3|new Database|new AutomationStore|new Knex" packages/*/src -l
rg -o "\.state/[a-z-]+" packages/*/src | sort -u
```

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
check whether a gate exists. Rules enforced only by convention belong in
the findings as **unenforced invariants** — the highest-value gap class.

## 9. Concept census — semantic duplication (type-4 clones)

`check:dupes` (jscpd) only sees type-1..3 clones — copy-paste with minor
edits. **Convergent implementations** (same concept, independently written,
different syntax) are invisible to it and are the usual shape of "we have
three versions of this". Count domain concepts per location — one `rg -l
… | sed 's|/src.*||' | sort | uniq -c | sort -rn` per concept:

| Concept                                        | Pattern          |
| ---------------------------------------------- | ---------------- |
| remote-call wrappers (own timeout/error shape) | `await fetch\(   | \.json\(\)` |
| tx/broadcast construction vs who should        | `new Transaction | \.addInput  | \.addOutput          | broadcast\(` |
| retry/backoff helpers                          | `attempt         | backoff     | retries`             |
| persistence openers                            | `new Database    | \.sqlite    | knex`                |
| hash/digest wrappers                           | `createHash      | sha256\(`   |
| sleep/wait helpers                             | `function sleep  | const sleep | setTimeout.*resolve` |

Same concept in 3+ locations = candidate cluster. **Compare contracts
before calling it a dupe**: list each site's signature (inputs → outputs →
failure modes). Two sites that take the same inputs and produce the same
effect are one implementation wearing two syntaxes — consolidate. Sites
with genuinely different contracts are separate concepts sharing a verb —
say so and move on. Temporal-coupling pairs (#5) are a second detector:
files that co-change without an import edge often duplicate knowledge.

## 10. Patch stratigraphy — fix-density

A file whose history is mostly fix-keyword commits is accumulating patches
on patches — the abstraction underneath is wrong or missing:

```bash
git log --since="6 months ago" --format="@@@%s" --name-only -- packages \
  | awk '/^@@@/{s=tolower($0); f=(s ~ /fix|bug|regress|patch|broke/);next}
         $1 ~ /^packages\/[^\/]+\/src\//{n[$1]++; if(f)fx[$1]++}
         END{for(k in n) if(n[k]>=5) printf "%d/%d %.0f%% %s\n", fx[k]+0, n[k], 100*fx[k]/n[k], k}' \
  | sort -rn | head -20
```

> 40% fix commits with ≥5 touches = stratified. Caveats: refactors named
> "fix" mislead; short histories give thin signal — say so.

**Infant files** (created recently, already patched — 3+ fixes within
weeks of birth = wrong contract):

```bash
for f in $(git log --since="30 days ago" --diff-filter=A --name-only --format= -- packages | rg "packages/[^/]+/src/" | sort -u); do
  [ -f "$f" ] || continue
  t=$(git log --oneline -- "$f" | wc -l); c=$(git log --format="%s" -- "$f" | rg -ci "fix|bug|regress|patch|broke" || echo 0)
  [ "${t:-0}" -gt 2 ] && echo "${c:-0}/$t $f"
done | sort -rn | head -15
```

Feed both lists into [consolidation.md](consolidation.md) — the deliverable
is the missing abstraction, not patch N+1.

## 11. Timer & wait census

Every bounded wait is latency paid forever; every unbounded one is a hang
paid once. Count per package:

```bash
for p in packages/*/src; do
  c=$(rg -c "setTimeout|setInterval|AbortSignal\.timeout|sleep\(|new Promise.*setTimeout" $p 2>/dev/null | awk -F: '{s+=$2} END{print s+0}')
  echo "$c ${p%/src}"
done | sort -rn
```

Classify each site on four axes — **bounded** (deadline present vs bare
`setTimeout(r,N)`), **event vs fixed poll** (sleep on a condition that
emits a wake = `nektar/fixed-sleep` at arch altitude), **configured vs
literal** (bare literals at 3+ sites with different values = drift), and
**stacked** (retry-in-loop inside retry-in-outbox inside retry-in-caller —
multiply the layers for the real bound). Flag fixed polls on money paths
and unbounded waits that pin a resource (`stab/hung-resource`).

## 12. SPoF & resilience inventory

For each money/durability path, name the redundancy across Release It!'s
layers: per-call (timeout) → component (bulkhead/breaker) → system
(load-shed) → ops (recycler) → recovery (failover/rebuild-from-source).

```bash
rg -ln "Broadcaster|SpendGuard|BudgetAuthority" packages/*/src tools/ | sed 's|/src.*||;s|tools/|tools|' | sort | uniq -c  # spend-path impls + who constructs
rg -ln "new Database|\.sqlite|knex" packages/*/src                                                                      # store writers — one per DB?
rg -n "broadcasterUrl|hostUrls|hosts\[" packages/*/src | head -25                                                        # single-host pins vs N>1 fanout
```

If the one impl/writer/host fails: takeover (bstack multi-host fanout is
the example), standby, rebuild — or silent stall? A derived-state store
with no rebuild path is a SPoF even with redundant readers. Feeds S9 in
smells.md.

## 13. Event & wake topology (EDA audit)

An autonomous-agent system should be **event-driven end to end** — timers
exist only as bounded ceilings and at the poll substrate. Inventory the
primitives:

```bash
rg -ln "EventEmitter|\.emit\(|waiters|\.on\('|waitFor" packages/*/src | sed 's|/src.*||' | sort | uniq -c | sort -rn
rg -n "unblock|wake|nudge|notify|signal|resolveWait|long-?poll" packages/*/src | head -30
```

Then classify **every wait site from #11** into one bucket: **retry-on-
failure** (legitimate; only duplicated impls feed #9), **event-substitute**
(a poll waiting on a state change — the missing signal is the finding, not
the poll), or **substrate** (the one place polling IS the design — exactly
one per subsystem; N substrates = scheduling sprawl).

Finally map **waiter→signaler pairs**: every producer of an awaited
condition must signal on ALL paths — success AND error. A producer that
signals only on its happy path is a lost-wakeup; a signaler with no waiter
is dead machinery.

## 14. Runtime census — failure-hunt substrate

The greps that feed `failure-modes.md`'s catalog. These find _candidates_;
each still needs the evidence standard (code path → trigger → failure path
→ blast → silence) and the challenge step before it's a finding.

```bash
# F5 fire-and-forget — every site needs a covering mechanism (lease/retry/reconcile)
rg -n "void [a-zA-Z_]|\.catch\(\(\) ?=> ?\{\}\)|\.catch\(\(\) ?=> ?(null|undefined|0|false)" packages/*/src
# F5 catch→default on money paths — failure returns a value indistinguishable from an answer
rg -n "catch[^}]*return (null|undefined|false|\[\]|0)" packages/*/src
# F6 unbounded collections — each needs an eviction/bound story
rg -n "= new (Map|Set)\(|readonly.*(Map|Set)" packages/*/src
# F2/F6 lifecycle — shutdown handling vs leaked handles
rg -n "process\.(on|once|exit)|SIGINT|SIGTERM|unhandledRejection" packages/*/src
# F4 subscriptions vs emissions — pair every consumer to a producer
rg -n "\.on\(|\.once\(|addEventListener|EventSource|new WebSocket" packages/*/src
# F3 recovery surface — where restart rebuilds vs assumes
rg -n "replay|resume|recover|reconcile|rebuild" packages/*/src
# F6/F7 connection discipline — keep-alive sockets held at exit are the 0xC0000409 class
rg -n "keep-?alive|connection:|new (WebSocket|EventSource)|Agent\(" packages/*/src
```

"Exists and is covered" is a _pass_ — record it: e.g. `void
relinquishOutput().catch(() => {})` is covered by the ledger lease, so
it's evidence the pattern is deliberate, not a finding.

## Timeboxing

A monorepo this size: inventory+graph ≈10 min, hotspots ≈5, coupling ≈5,
cold/debt ≈5, fitness audit ≈10, censuses (#9-#14) ≈25, smell pass
≈20-30, runtime failure hunt ≈30-45 (the topology map + evidence-standard
write-ups are the expensive part — do not skip the challenge step to save
time). If a step stalls, mark it "not measured" in the report — never
fabricate the number.
