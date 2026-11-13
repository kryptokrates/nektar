# Architecture Review — nektar

**Date:** 2026-11-13 · **Scope:** full monorepo (20 packages, ~38.6k src LOC), working tree incl. uncommitted `cryptoops.ts` extraction · **HEAD:** `d0a6ee6 mcp: extract KeyOps`
**Method:** `.devin/skills/arch-review` — measurement playbook §1–§13, smell catalog (canonical 7 + S1–S10), consolidation lens. Every finding cites measured evidence; history covers only Nov 1–13 (~2 weeks, 423+ commits), so churn signals are high-resolution but thin — flagged where they drive a conclusion.

**System shape:** Declared as a layered modular monorepo — `core` root (I=0), leaf libs (`crypto/log/datalog/policy/zk/graph/identity/contracts`), `chain` at the ledger, `anchor/verify/automaton` on it, `bstack` transport, `mcp/watcher/reasoning` apps, `e2e/fuzz` harnesses. The declared shape mostly holds: the specifier graph is acyclic in the committed graph, `chain` is stable (I=0.1, fanout 1), `core` is the boring hub (I=0). The actual shape deviates in one load-bearing place: **`mcp` is a volatile hub** — 9,977 src LOC (26% of the codebase), fanout 16/16 packages, the top-4 churn files all inside it, 85 of the 127 distinct `NEKTAR_*` vars read there — and it is mid-surgery: 9 `*Ops` extractions are landing, which has temporarily introduced type-only cycles and pushed its fanout to 30.

### Scorecard

| Dimension       | Rating                                | Evidence                                                                                                                                                           |
| --------------- | ------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Dependencies    | TANGLED                               | Acyclic committed graph, but mcp→16 packages and 3 in-flight type-only cycles (`*ops ↔ memory.ts`); `check:cruise` red                                             |
| Boundaries      | LEAKY                                 | `export *` on 10+ package indexes (automation re-exports 17 modules); Ops deps expose parent `Map`s by reference; `server.ts` reaches `mem.*` 129×                 |
| Coupling        | TIGHT inside mcp; MODERATE elsewhere  | Temporal-coupling top-14 pairs all mcp-internal (memory↔server 52 co-changes)                                                                                      |
| Hotspots        | CONCENTRATED                          | memory.ts 88, server.ts 85, http.ts 57, runtime.ts 51 commits / 14 days                                                                                            |
| Debt automation | ENFORCED, currently RED               | `check:arch` (mcp.loc 9742→9917, memory.ts fanout 26→30, 1 type-cycle) + `check:cruise` (3 cycles) both failing; `check:knip`/`check:deps`/`check:leftovers` green |
| Duplication     | SEMANTIC                              | jscpd 2.46% (612 lines/147 files) syntactic; verified type-4 clusters below                                                                                        |
| Wait discipline | BOUNDED with event-substitute pockets | ~160 wait sites; 2 event-substitutes found; 6 retry dialects in one file                                                                                           |
| Redundancy      | SINGLE-POINT                          | tx-status reads pinned to one broadcaster; market envelopes do have multi-host fanout                                                                              |

---

### Findings

| #   | Sev        | Smell/rule                            | Where                                 | Evidence (measured)                                                                                                                                                                                                                    | Recommendation                                                                                                                |
| --- | ---------- | ------------------------------------- | ------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| F1  | **HIGH**   | God component + S7 patch stratigraphy | `mcp/memory.ts` + package             | 2,841 LOC, 30 imports (was 26), 88 commits/14d, 14 fix commits (16%); package 9,977 LOC = 26% of codebase, over `check:arch` ratchet                                                                                                   | Continue façade-ward (below); next move is state ownership transfer, not more method moves                                    |
| F2  | **HIGH**   | S6 convergent implementation          | `mcp/runtime.ts` + `txops.ts`         | Same `fetch {broadcaster}/tx/{txid} → txStatus` body at **3 sites** — same gorillapool fallback literal, same `AbortSignal.timeout(10_000)`, same `.ok ? .json() : null`, same `.toUpperCase()` (runtime.ts:818, :1188, txops.ts:155)  | Extract `txStatus(txid)` onto the chain-client interface (chain-observer seam)                                                |
| F3  | **HIGH**   | S6 + magic literal                    | chain + mcp                           | `arcade.gorillapool.io` redeclared at **5 sites** (mainnet.ts:24, bin.ts:62, txops.ts:155, runtime.ts:818, :1188); a _different_ default `arcade-us-1.bsvblockchain.tech` at runtime.ts:681 — two disagreeing defaults on a money path | One exported default constant owned by `chain/mainnet.ts`; all sites import it                                                |
| F4  | **HIGH**   | Cyclic dependency (type-only)         | `mcp/*ops ↔ memory.ts`                | `check:cruise` reports **3 cycles**: memory→keyops→memory, memory→identityops→memory, memory→cryptoops→memory — all via `import type { … } from './memory.js'`                                                                         | Move `NektarMemoryOptions`, `OperatorWallet`, `CallerIdentity`, `ZkClearance` to a `types.ts` leaf — the missing third module |
| F5  | **MEDIUM** | S8 timer sprawl                       | `chain/mainnet.ts`                    | **6 hand-rolled backoff dialects in one file**: `500·2ⁱ` (503×3), doubling `backoff`→`maxB` (SSE reconnect), `1200·(i+1)` (429×3), `800·2ⁱ` (429/5xx×4), fixed `350ms` (rate spacing), plus `spendguard.ts:89` wait                    | One `withRetry(fn,{baseMs,max,jitter})` + one `pace()` rate-limiter; sites declare intent                                     |
| F6  | **MEDIUM** | S9 single point of failure            | `mcp/runtime.ts`, `txops.ts`, `chain` | All tx-status reads → one broadcaster URL; market envelopes have multi-host fanout (`NEKTAR_MARKET_HOSTS`, 9 reads) but chain status reads don't; `WocTracker` alt-path exists in testkit only                                         | Second-source cross-check (WoC) on status reads, or a documented acceptance                                                   |
| F7  | **MEDIUM** | S10 event-substitute wait             | `automation/runtime.ts:1105`          | `while (busy \|\| upkeepRun !== undefined \|\| acking) await setTimeout(25)` — **unbounded** drain-spin on the teardown path; flags clear but nothing signals                                                                          | Bound the drain + completion signal; teardown hang is a production incident class                                             |
| F8  | **MEDIUM** | S1 ambient coupling                   | repo-wide                             | **127 distinct env vars** (85 read in mcp); ~20 `.state/` paths; `lanes.sqlite` seeded by `copyFileSync(automation.sqlite)` — schema shared across a package boundary via file copy                                                    | Env-var census doc; replace file-copy seeding with a real table migration                                                     |
| F9  | **MEDIUM** | Ambiguous interface                   | 10 package indexes                    | `export *` is the house pattern — automation 17 re-exports, bstack 11, identity 7, core 8; consumers can't see the promised surface; knip is the only boundary guard                                                                   | Selective exports on `chain`/`automation` first (busiest surfaces)                                                            |
| F10 | **MEDIUM** | S6 convergent impl (same-file)        | `mcp/runtime.ts` ×2 paths             | `openRuntime` (L273) vs `openLiveRuntime` (L621): `new ChainEvents` ×2, `knexMod(records.sqlite)` ×2 (L453/837), `beat`/lockfile ×2 (L311/636), `waitForFunding` ×2                                                                    | Converge `openRuntime` onto the already-parameterized `openLiveRuntime(cfg, net:'regtest')`                                   |
| F11 | **LOW**    | S10 event-substitute wait             | `mcp/runtime.ts:1025`                 | `waitForFunding`: fixed 15s poll on `utxoView.utxos.length` while `ChainEvents` exists in the same runtime                                                                                                                             | Subscribe funding-watch + bounded ceiling                                                                                     |
| F12 | **LOW**    | S6 convergent impl                    | `mcp/server.ts`                       | `callTool` switch reaches `mem.*` 129×; tool surface and memory engine co-change 52×/14d — the boundary inside mcp is mem-as-everything                                                                                                | Route tool cases through the Ops seams (they exist now); server's contract = Ops, not the god-object                          |
| F13 | **LOW**    | S8 busy-wait                          | `automation/runtime.ts:1105`          | (same site as F7 — the 25ms spin is also a CPU poll) covered by the drain fix                                                                                                                                                          | —                                                                                                                             |
| F14 | **LOW**    | S6 convergent impl                    | 8 packages                            | One raw `createHash` site per package despite `core.sha256`                                                                                                                                                                            | Normalize through core or accept as per-package crypto glue — decide, don't drift                                             |

_Note: `mcp/src/automation/*` and `packages/overlay/*` in temporal-coupling/hotspot data are pre-extraction history — both are deleted; the `packages/automation` split already landed. The `anchorops.ts` orphan error from earlier is resolved; the live cruise errors are the F4 type-cycles._

---

### Root causes

The drill (`consolidation.md` §root-cause) applied to the three stratified/duplicated sites:

- **`memory.ts` keeps breaking because it is composition root + domain engine + state owner at once.** 9 Ops objects (`txOps, anchorOps, identityOps, keyOps, cryptoOps, catalogOps, custodyOps, revocationOps, bonds`) injected — but ~14 state maps (`anchors, tails, anchorTxs, txidShard, _shardPubs, zkQueries, automata, attestationTxs, importedBundles, challenges…`) remain parent-owned and passed into deps **by mutable reference**. LOC shrank; blast radius didn't. The move: Ops own their maps; memory.ts delegates.
- **`runtime.ts` duplicates bring-up because regtest and live were written as parallel functions.** Every concern must be added twice — ChainEvents, lockfile beat, knex open, fund-wait, broadcaster fetch. The move: `openRuntime` delegates to the already-parameterized `openLiveRuntime` with `net:'regtest'` — F2/F3/F10 collapse together.
- *_The *ops↔memory type-cycles exist because the option/identity types live in the god file.*_ `NektarMemoryOptions`, `OperatorWallet`, `CallerIdentity`, `ZkClearance` are the vocabulary both directions need. The move: `types.ts` leaf — the classic "two modules needing each other's types are missing a third."
- **`mainnet.ts` backoffs sprawl because retry policy is inlined per call site.** Six dialects, incomparable constants, no shared jitter. The move: `withRetry` + `pace()` — the _policy_ is the abstraction, not each loop.
- **`server.ts` couples to everything because `callTool` is a flat switch over the mem surface.** 129 `mem.*` touches = every tool case is a memory-engine client. The Ops seams now exist; the move is cases → Ops, not mem.

---

### EDA audit (event-driven-architecture lens)

**Event topology (measured, §13):**

| Primitive                                                        | Where                       | Shape                                                                                                                   |
| ---------------------------------------------------------------- | --------------------------- | ----------------------------------------------------------------------------------------------------------------------- |
| `ChainEvents` — `waiters` map, `emit('tx'/'height'/'abandoned')` | `chain/events.ts`           | 5s tick substrate scanning `watched` + per-`waitFor` nudge loop (150ms); the one legit tx-status scheduler              |
| `unblockOutbox` pull-forward                                     | `automation/store.ts:512`   | Wake-as-hint done right: `deferOutbox` keeps a bounded 2s re-check, so a missed signal degrades to a poll, never a hang |
| Watch-shim long-polls (A2)                                       | `automation/runtime.ts:957` | Push wakes set `hotUntil` → `deals()` at once; "a wake is only ever a hint, never the load path"                        |
| Idle/hot adaptive tick (A4)                                      | `automation/runtime.ts:896` | `setTimeout` chain not fixed interval — deliberate, documented                                                          |

**Waiter→signaler check (`funding slot`):** all freeing paths signal — minted market tree (`runtime.ts:642`), proven topup (`1039`), anchored-ancestry slots (`1043`). Fallback defer is bounded at 2s. **Clean.**

**Wait classification (§13 on ~160 sites):**

- _Retry-on-failure (legitimate):_ all six `mainnet.ts` backoffs retry failed ops (503 backpressure, SSE reconnect, 429, 429/5xx, WoC rate spacing). Correct pattern — finding F5 is the six _dialects_.
- _Event-substitute (violations):_ F7 (unbounded teardown drain-spin) and F11 (15s funding poll while `ChainEvents` exists alongside).
- _Substrate (design):_ `ChainEvents` tick, `watcher.mjs` `WATCH_MS`, the two `beatTimer` heartbeats.

**Verdict:** the event _primitives_ exist and are well-designed; the violations are two waits bypassing them plus retry-dialect sprawl.

---

### mcp deep-dive

**Package**: 9,977 LOC / 27 files / fanout 16 (all packages) / I=0.941. It is the app: composition root, MCP+HTTP surface, wallet custody seam, memory engine — and 24%→26% of the codebase and growing _despite_ active extraction.

**Extraction quality (the real question):** the Ops pattern is _method-objects_, not _state-owning modules_.

| Ops file         | LOC | Deps expose                                                     |
| ---------------- | --- | --------------------------------------------------------------- |
| `keyops.ts`      | 111 | `opts`, `fundKey()` getter, `shardPubs: Map` (parent's, by ref) |
| `identityops.ts` | 89  | `challenges`/`zkSeen` Maps + `persistSecurityState()`           |
| `anchorops.ts`   | 59  | `anchors`/`tails`/`anchorTxs` Maps + `persistState()`           |
| `txops.ts`       | 176 | `anchorTxs`, `txs()`, `txidShard` Map, `attachProof`            |
| `cryptoops.ts`   | 86  | `opts` only — cleanest deps (newest extraction)                 |

Progress: `memory.ts` 2,890→2,841 LOC (committed 2,877 at KeyOps). But `mcp.loc` grew net +175 in the same window — extraction currently _adds_ wiring LOC faster than it removes methods. That trajectory is normal mid-flight; the ratchet forcing function is doing its job. The dependency inversion to fix next: Ops deps should receive **narrow interfaces they own** (`{ get/set/has }` facades or transferred maps), not the parent's raw `Map`s — today a bug in `anchorops` still writes into memory's `anchors` map.

**`server.ts` (1,961 LOC):** `TOOLS` registry + `callTool` giant switch — 129 `mem.*` accesses; top co-change pair with memory.ts (52). With Ops landed, the natural seam is cases → Ops objects; until then every new tool case keeps growing both files.

**`runtime.ts` (1,272 LOC):** two parallel ~350-line bring-up paths (`openRuntime` L273 regtest / `openLiveRuntime` L621 live) — 5+ duplicated blocks. `openLiveRuntime` is already parameterized on `net`; `openRuntime` should delegate.

**`http.ts` (850 LOC):** third member of the coupling cluster (57 commits); SSE beat interval + tool HTTP surface — co-changes because it serves the same mem object.

---

### Simplification ledger

| Opportunity                                             | Evidence                    | Est. LOC       | Move                        |
| ------------------------------------------------------- | --------------------------- | -------------- | --------------------------- |
| `*ops↔memory` type-cycles → `types.ts` leaf             | F4: 3 cruise errors         | −0 (enabler)   | Missing third module        |
| `openRuntime` → `openLiveRuntime` convergence           | F10: 5 duplicated blocks    | −150…−250      | Reseam / parameterize       |
| txStatus ×3 → chain-observer seam                       | F2: 3 sites, same contract  | −40            | Extract deep module         |
| `arcade.gorillapool.io` ×5 (+1 disagreeing) → one const | F3: 5 sites, 2 URLs         | −5 + drift fix | Shared constant in chain    |
| `mainnet.ts` 6 backoff dialects → `withRetry`+`pace()`  | F5: 6 sites, 1 file         | −30            | Centralize the wait         |
| Drain-spin → bounded + signal                           | F7: unbounded               | −3 + hang fix  | Bounded drain               |
| `records.sqlite` dual open → one factory                | F10: L453/837               | −10            | Hoist factory (part of F10) |
| Ops deps: raw `Map`s → owned/narrow interfaces          | deep-dive table             | 0 (boundary)   | Port/adapter inside mcp     |
| `callTool` cases → Ops seams                            | F12: 129 `mem.*` touches    | −0 (boundary)  | Reseam (long game)          |
| `export *` → selective on chain/automation              | F9: 28 re-exports on 2 pkgs | 0 (boundary)   | Deliberate surfaces         |

**Total headline:** the ledger's top-4 executable moves (types leaf, open-path convergence, txStatus seam, withRetry) are all cheap, all unblock ratchet red, and together they shrink the highest-churn files.

---

### Top 3 priorities

1. **Break the `*ops ↔ memory` type-cycles** — extract `types.ts` in mcp (`NektarMemoryOptions`, `OperatorWallet`, `CallerIdentity`, `ZkClearance`). Cheapest structural move; turns `check:cruise` green and unblocks the extraction wave.
2. **Converge the two `runtime.ts` open-paths** — `openRuntime` delegates to `openLiveRuntime(cfg, 'regtest')`; kills F2's twin, F3's extras, F10, and the dual beat/knex/ChainEvents blocks in one move.
3. **Chain-observer seam + one broadcaster default** — `txStatus(txid)` on the chain client + exported default URL + optional WoC cross-check; resolves F2, F3, F6 together.

### Roadmap

- **Critical** — F4 types leaf (unblocks the extraction wave + green gates)
- **Important** — F10 open-path convergence; F2/F3/F6 chain-observer seam; F7 bounded drain; F5 `withRetry`
- **Beneficial** — F9 selective exports on `chain`/`automation`; F14 `createHash`→`core.sha256`; env-var census doc (F8); Ops deps → owned interfaces (long game: F1/F12)

**ADR stubs:**

```
ADR-1: mcp types leaf   status: proposed
Context: 3 type-only cycles (*ops↔memory) failing check:cruise; every new
Ops extraction inherits the cycle because options/identity types live in memory.ts.
Decision: extract packages/mcp/src/types.ts holding NektarMemoryOptions,
OperatorWallet, CallerIdentity, ZkClearance; ops + memory both import it.
Consequences: cruise green; the *ops pattern stops leaking back into memory;
memory.ts import surface narrows. LOC: ~+80 new file, −0 net but unblocks ~−500 in moves.
Rejected: (a) inline the types per ops file — recreates the vocab in 5 dialects;
(b) move ops back — abandons the extraction.

ADR-2: single open-path in runtime.ts   status: proposed
Context: openRuntime/openLiveRuntime duplicate ChainEvents, lockfile beat,
knex open, waitForFunding, broadcaster fetch (F10, and the root of F2/F3).
Decision: openRuntime delegates to openLiveRuntime with net='regtest';
regtest-specifics become parameters, not a parallel function.
Consequences: −150…−250 LOC; every bring-up concern lands once; regtest
parity is enforced structurally.
Rejected: keep parallel paths — proven to drift (the gorillapool literal
already did).

ADR-3: chain-observer seam   status: proposed
Context: txStatus body ×3, broadcaster default ×5 with two disagreeing
URLs, status reads pinned to one broadcaster (F2/F3/F6).
Decision: txStatus(txid) on the chain-client interface; one exported
default-broadcaster const; optional WoC cross-check on terminal states.
Consequences: −40 LOC; config drift eliminated; status path gains a second
source on the money read.
Rejected: document-only convention — already failed (5 sites, 2 URLs).
```

---

### Honest paragraph

This is a two-week-old codebase mid-open-heart-surgery. The declared layering is mostly real — `core` is boring, `chain` is stable, the package graph is acyclic where committed — but the app package `mcp` is simultaneously the composition root, the memory engine, the HTTP/MCP surface, and the custody seam, and at 26% of the codebase it's where all the churn, all the ambient contracts (85/127 env vars), and all the top-four hotspots live. The Ops extractions are the right architecture and they're _working_ (memory.ts is shrinking, cryptoops has the cleanest deps yet) — but mid-flight they've produced the two classic extraction wounds: type-only cycles back into the god file, and deps that pass parent `Map`s by reference instead of transferring ownership. The load-bearing weakness is `memory.ts` at 2,841 LOC/30 imports: every feature change still opens it, and `server.ts`'s 129 `mem.*` accesses guarantee the tool layer keeps pulling it along. What hurts first if nothing changes: a money-path bug in one of the shared maps that the extractor assumed was private — the Ops pattern made the _files_ look separated while the _state_ stayed communal. The cheapest unblocking move is the `types.ts` leaf; the highest-leverage move is converging `runtime.ts`'s two open-paths, because half the semantic duplication in this report is those two functions disagreeing with each other.

---

## Appendix — raw measurements

**§1 Package inventory** (files / LOC / pkg, incl. `.mjs`):

```
27 9977 mcp | 18 6362 automation | 21 4771 e2e | 14 4443 contracts
14 3480 bstack | 12 3129 chain | 9 1639 identity | 10 1192 zk
5 1145 anchor | 6 997 graph | 10 725 core | 2 573 reasoning
6 545 log | 4 446 store | 5 387 datalog | 3 374 automaton
2 272 watcher | 2 152 verify | 1 121 policy | 2 118 crypto
```

Top exports: reasoning 30, identity 26, chain 21, automation 17.

**§2 Specifier edges** (`from '@nektar/*` counts): core 91, chain 39, anchor 16, identity 11, contracts 9, datalog 8, zk 6, store 5, mcp 5, verify 4, log 4, reasoning/graph/crypto/bstack 3, policy/automaton/automation 2.

**§3 Boundary surfaces:** `export *` sites — automation 17, bstack 11, core 8, identity 7, graph 5, datalog 4, zk 3, store 3, log 3, chain/automaton/reasoning 1 each. chain imported by: e2e 14, mcp 7, automation 5, bstack 3, anchor 3, watcher/verify/reasoning/automaton 1.

**§4 Hotspots (14d, file-level, commits):** memory.ts 88, server.ts 85, http.ts 57, runtime.ts 51, mcp/automation/runtime.ts† 34, overlay/presence.ts† 33, mcp/automation/market.ts† 31, bin.ts 29, chain/testkit.ts 22, pay.ts 20, bstack/finger.ts 20 († deleted paths — pre-extraction).
Package churn: mcp 563, e2e 159, bstack 139, chain 115, overlay† 95, zk 67, contracts 54, identity 51, anchor 51, core 33.

**§5 Temporal coupling (top pairs, 14d):** memory↔server 52, memory↔runtime 29, http↔memory 29, http↔server 28, mcp/automation/market↔runtime† 22, runtime↔server 21, http↔runtime 19, chain/index↔mcp/runtime 15.

**§6 Cold/debt:** coldest file = 12 days (everything hot; `0 of 191 untouched >180d`). Leftover baseline: `as any` mcp 24, e2e 20, anchor 17, identity 12, automation 10. jscpd: **2.46%** (612 duplicated lines / 147 files) under 5% gate.

**§7 Ambient contracts:** 127 distinct `NEKTAR_*` vars — per package: mcp 85, e2e 45, automation 22, chain 12, watcher 8, zk 7, store 6, contracts 5. Top vars: REGTEST_WIF 42, RPC_PASS 29, RPC_USER 27, NETWORK 23, PROPAGATION_URL 20. `.state/` paths: ~20 (`treasury`, `ttn-*`, `reserve*`, `watcher-*`…). sqlite writers: mcp/runtime.ts (`records.sqlite` ×2), automation/runtime.ts (`automation.sqlite` + `lanes.sqlite` via file copy), automation/toolbox.ts (`wallet.sqlite`).

**§8 Fitness gates (live):** `check:arch` ✗ (mcp.loc 9742→9917; memory.ts fanout 26→30; 1 type-cycle); `check:cruise` ✗ (3 no-circular: memory→keyops/identityops/cryptoops→memory); `check:knip` ✓; `check:deps` ✓; `check:leftovers` ✓ (93 markers, at baseline); `check:dupes` ✓ (2.46%<5%).

**§9 Concept census:** `await fetch` — e2e 15, mcp 6, store/chain/bstack/automation 2; `.json()` — e2e 15, mcp 5; `new Transaction` — e2e 11, mcp 5, chain/bstack/anchor 3, core 1; `createHash` — 1 site/pkg in zk,store,reasoning,mcp,datalog,core; `backoff` — chain 2, automation 2, anchor 1; sqlite openers — mcp 1, automation 1 (file-copy seeding).

**§10 Patch stratigraphy (fix-commits/touches):** memory.ts 14/88 (16%), server.ts 10/85 (12%), http.ts 6/57 (11%), runtime.ts 4/51 (8%), anchor/vault.ts 3/9 (33%), anchor/bond.ts 3/9 (33%), e2e runner 3/10 (30%). No file >40% with ≥5 touches; absolute fix-volume leaders are the mcp trio.

**§11 Timer census (prod packages, ~160 total incl. e2e):** chain 36, e2e 26, mcp 15, automation 14, bstack 4, zk 3, watcher 3, anchor 1. Classification: retry-on-failure (mainnet.ts ×6 dialects), event-substitute (automation:1105 unbounded spin; mcp:1025 fund-poll), substrate (ChainEvents tick, watcher WATCH_MS, beatTimers, automation A4 adaptive tick, watch-shim long-polls).

**§12 SPoF inventory:** broadcaster/spend classes: chain (mainnet, regtest, testkit, spendguard, events), mcp runtime — **all status reads hit one broadcaster URL**; multi-host exists only for market envelopes (`NEKTAR_MARKET_HOSTS`). Store writers: records.sqlite (mcp ×2 openers), automation.sqlite+lanes.sqlite (automation), wallet.sqlite (toolbox). Watch-shim N>1 for box/office; chain reads N=1 (WoC fallback exists in testkit only).

**§13 EDA primitives:** emit/wait sites per package — mcp 16, automation 8, bstack 7, chain 6, e2e 18. Wake primitives: `ChainEvents.waiters`/`emit`, `store.unblockOutbox`, watch-shim `hotUntil`, A4 adaptive tick. Waiter→signaler: `funding slot` — 3/3 freeing paths signal + bounded defer fallback (clean). Violations: F7, F11.
