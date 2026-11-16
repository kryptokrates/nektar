# nektar system audit

Where the complexity comes from: a whole-codebase audit using the arch-review, event-arch and spv-strict skills, focused on system-level drift that causes bugs, stalls and latency.

Audited at commit `4b5ba58` on 2026-11-16. The audit changed no code. Section 9 lists the stages of the streamlining program that follows from it; results are appended below as each stage lands.

## 1. The short version

The lower layers are in good shape. The package graph is clean, there are no dependency cycles, and the proof and header checks are now strict.

The trouble is concentrated in one place: **the part that handles money and deals** (`packages/mcp` and `packages/automation`). That part has five structural problems, and almost every failure of the last days traces back to one of them:

1. **Nobody owns the answer to "which coins do I have?"** At least nine places keep their own record, and they disagree.
2. **Most tests need one shared live wallet on the local chain.** That wallet is dead, so most tests cannot run. This is why testing always means waiting on a real chain.
3. **There are two separate start-up paths** (local chain and real network). The real-network one is larger and barely tested, and that is where the bugs were.
4. **Five very large pieces of code hold everything together.** Each is 750 to 1,800 lines and mixes many jobs.
5. **About ten independent timers share one wallet** with no priorities, so background chores starve deals.

Fixing these five, in order, removes most of the complexity.

## 2. What the system is

Five parts. The first three are healthy; the last two are where the drift is.

| Part         | Packages                                                                                      | What it does                                                           | Health                                  |
| ------------ | --------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------- | --------------------------------------- |
| Foundations  | `core`, `crypto`, `log`, `datalog`, `policy`, `graph`, `identity`, `zk`, `contracts`, `store` | Records, keys, proofs, contracts                                       | Good                                    |
| Chain access | `chain`, `anchor`, `verify`, `automaton`                                                      | Talk to the blockchain, check proofs and headers                       | Good after this week's fixes            |
| Transport    | `bstack`                                                                                      | Publish and fetch signed objects through hosts, using pre-paid "slots" | Mostly good; slot stock is still fiddly |
| Application  | `mcp`                                                                                         | Memory engine, HTTP and MCP server, wallet wiring, start-up            | Overloaded                              |
| Market agent | `automation`                                                                                  | Buy/sell engine, outbox, upkeep, wallet adapter                        | Overloaded                              |

Test and tooling code sits beside these: `e2e` (25 scripts, no tests), `fuzz`, `watcher`, `tools/`, `deploy/`.

## 3. The numbers

| Measure                                   | Value                                                                                                                                   |
| ----------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| Source code                               | about 45,000 lines in 20 packages                                                                                                       |
| `mcp` + `automation`                      | 18,223 lines, 40% of all source                                                                                                         |
| History                                   | 528 commits between Nov 1 and Nov 16 (about 33 a day)                                                                                   |
| Changes landing in `mcp` (source + tests) | 1,167 file touches; the next package, `e2e`, has 188                                                                                    |
| Most-changed files                        | `mcp/memory.ts` 113 revisions, `mcp/server.ts` 87, `mcp/runtime.ts` 69, `mcp/http.ts` 58                                                |
| Largest hand-written files                | `mcp/server.ts` 2,015 lines, `automation/market.ts` 2,012, `mcp/memory.ts` 1,970, `automation/runtime.ts` 1,531, `mcp/runtime.ts` 1,440 |
| Settings (`NEKTAR_*` variables)           | 162 in code, 164 documented; `mcp` alone reads 94                                                                                       |
| Timers and sleeps in source               | `chain` 38, `e2e` 33, `automation` 16, sandbox shims 13, `mcp` 12                                                                       |
| MCP tools / HTTP routes                   | 54 tools, about 37 route checks                                                                                                         |
| Architecture gate (`check:arch`)          | **Failing, 24 violations**; its baseline was reset 6 times between Nov 13 and Nov 15                                                    |

## 4. Scorecard (arch-review)

| Dimension       | Rating                      | Why                                                                                                                                                                 |
| --------------- | --------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Dependencies    | CLEAN                       | Edges follow the declared layers; the gate reports 0 cycles, including type-only ones                                                                               |
| Boundaries      | LEAKY                       | `AutomationHost` is a 24-member interface its own comment calls "deliberately wide"; `NektarMemory` exposes 112 methods; 48 `export *` lines across package indexes |
| Coupling        | TIGHT in `mcp`/`automation` | 162 settings, more than 20 state files, hooks assigned after construction (`mem.txLookup =`, `mem.proofLookup =`, `mem.watchTx =`, `mem.ledger =`)                  |
| Hotspots        | CONCENTRATED                | Four `mcp` files take most of the change                                                                                                                            |
| Debt automation | PARTIAL                     | Many gates exist, but the architecture gate is red and keeps being reset instead of obeyed                                                                          |
| Duplication     | SEMANTIC                    | Two start-up paths, four copies of the test agent builder, nine ways to ask a transaction's status                                                                  |
| Wait discipline | BOUNDED, with sprawl        | Events drive the core, but about ten separate schedulers run side by side                                                                                           |
| Redundancy      | SINGLE-POINT                | One indexer, one broadcaster per network, one shared test wallet                                                                                                    |

## 5. Findings

Ordered by how much damage each does. "Evidence" is what I measured or saw fail. "Hurts" is the effect on you.

### F1. CRITICAL — nobody owns "which coins do I have"

**What.** The knowledge of which outputs exist, which are spent and which are reserved is spread over many stores, each with its own writer:

| Store                             | Holds                                                                                  |
| --------------------------------- | -------------------------------------------------------------------------------------- |
| `wallet.sqlite` (embedded wallet) | The wallet's own spendable list                                                        |
| `reserve.json` (file reserve)     | Entries plus a "spent" list; used by legacy mode, by tests, and as a boot seed         |
| `tx_store` table                  | Raw transactions, proofs, and (new this week) which transaction spends which output    |
| `automation.sqlite` ledger        | Each wallet transaction's state (8 states), plus leases on outputs                     |
| `spend-ledger.json`               | The spend guard's daily totals                                                         |
| `lanes.sqlite`                    | Pre-paid slots per lane and whether their tree is proven                               |
| In-memory caches                  | The wallet adapter's cache, a status cache, the event layer's watch list               |
| The indexer                       | What the address holds according to an outside service                                 |
| A second store per test run       | `.state/tn-suite/<agent>/` keeps its own reserve for the same keys the live agents use |

**Evidence.**

- `markSpent` has 18 call sites, `addOwned` 21, `takeFunding`/`fundingOuts` are called from 9 files, and spent-tracking code lives in 19 files.
- Measured today: the test suite's reserve file for alice marks 4 outputs worth 1,612,467 sats as spent while the chain shows them unspent. Three tests failed on "no funding UTXO" because of it.
- This session's failures of the same kind: outputs re-imported after being spent (double-spend attempts), change from a rejected transaction selected as funding (a payment reported as sent that was not), identity chains dead since Nov 13 after two processes spent from one key, an agent reporting 6.2M sats while the chain showed 5.2M.

**Hurts.** Lost deals, rejected transactions, "no funds" while holding funds, and hours of reading databases by hand to find out which store is wrong. Each fix so far added one more reconciliation between two stores.

**Move.** One `Funds` module is the only writer. It answers four questions: what can I spend, reserve this for me, I spent this, this spend died. Everything else asks it. The embedded wallet stays as its storage; the file reserve and the scattered marks go away.

### F2. CRITICAL — tests depend on one shared live wallet

**What.** The default way to test money code is to spend real coins on the local chain from one shared test wallet.

**Evidence.**

- 33 of 42 `mcp` test files need the local chain stack; across all packages 45 test files do.
- The shared wallet is dead (its coins exist but cannot be spent, the known `txmeta` problem in `AGENTS.md`). Result on the last full run: 226 tests, 49 pass, 166 fail, 6 cancelled; 165 of the failures are the wallet. `.state/treasury.dead-20261112.json` shows it also died on Nov 12.
- There is no in-memory stand-in for the chain on the money path. There was one (`packages/chain/src/mock.ts`, 342 lines); it was deleted on Nov 12 in commit `07a9bf9` to "standardize tests on regtest".
- The live testnet suite uses the same keys as the running agents. Until this week nothing stopped both from spending at once.

**Hurts.** There has been no trustworthy full test result for days. New code is checked only by small tests on isolated helpers, so wiring mistakes surface on testnet (three did this week: the status check that could never answer "rejected", the refund default that made every offer ineligible, the missing proof on funding outputs). It is also the root of "every test waits for a block".

**Move.** Build one in-memory world (chain, broadcaster, header source, proof source, wallet) and make it the default test tier. It mines a block when the test says so, in microseconds. Keep the local stack and testnet for a small smoke set.

### F3. HIGH — two start-up paths, and the real-network one is the untested one

**What.** `mcp/src/runtime.ts` has `openRuntime` (local chain, about 300 lines) and `openLiveRuntime` (testnet/mainnet, about 750 lines). They wire the same parts differently.

**Evidence.**

- 43 statements are identical in both, out of 134 distinct statements in the local path. The file has 32 branches on wallet mode and 51 on network.
- Three wallet modes (`client`, `toolbox`, `legacy`) times four networks (`regtest`, `testnet`, `ttn`, `main`) gives twelve combinations; I count tests for roughly three.
- 45 test files use the local path. Nine touch any live-network piece, and only `market-testnet.test.ts` runs the live path end to end.
- Every bug fixed this session was in live-only code: funding import, the wallet adapter, indexer discovery, the status stream, raw-transaction lookup, the legacy refresh.

**Hurts.** Tests pass on the local chain and the same feature fails on testnet. You pay for each difference with a live debugging session.

**Move.** One start-up path that takes a `ChainPort` (broadcast, headers, proofs, raw transactions, status hints) and a `FundsPort`. Local chain, testnet and the in-memory world from F2 become three adapters. Then a test on the in-memory world exercises the same wiring that runs live.

### F4. HIGH — five giant pieces hold everything

**What.**

| Piece                                      | Size                                                                 | What is mixed inside                                                                    |
| ------------------------------------------ | -------------------------------------------------------------------- | --------------------------------------------------------------------------------------- |
| `openAutomation` (`automation/runtime.ts`) | one function, about 1,370 lines, 21 inner async functions, 132 `if`s | lanes, outbox, upkeep, tree minting, watches, watchdog, journal, provisioning, shutdown |
| `MarketEngine` (`automation/market.ts`)    | one class, about 1,770 lines, 212 `if`s                              | buyer and seller flows together: 27 states, 64 transition sites, 51 causes              |
| `NektarMemory` (`mcp/memory.ts`)           | one class, about 1,800 lines, 112 methods                            | memory, receipts, anchoring, funds, chain events, sale hooks                            |
| `dispatchTool` (`mcp/server.ts`)           | one switch, about 1,100 lines                                        | all 54 tools                                                                            |
| `openLiveRuntime` (`mcp/runtime.ts`)       | one function, about 750 lines                                        | see F3                                                                                  |

**Evidence.** The architecture gate fails on exactly these files (for example `automation/runtime.ts` 1,332 to 1,532 lines, `mcp/runtime.ts` 1,337 to 1,441). The gate's baseline was reset six times in three days, so it is recording growth, not stopping it.

**Hurts.** Nobody, human or AI, can hold one of these in their head. A change in one corner breaks another, and review cannot catch it. This is the direct cause of "I can't manage the complexity".

**Move.** Split along seams that already exist in the code, one responsibility per file, after F1 to F3 have removed the cross-cutting state:

- `openAutomation` into lanes, outbox, upkeep, watches, and a thin assembler.
- `MarketEngine` into a buyer machine, a seller machine and shared escrow steps, each with its state table written down as data.
- `dispatchTool` into one handler file per tool group.
- `NektarMemory` keeps memory and receipts; funds and chain events move out behind the F1 and F5 modules.

### F5. HIGH — nine ways to ask "what happened to this transaction"

**What.** Different parts use different words and different functions for a transaction's state.

| Vocabulary                 | Values                                                                                       |
| -------------------------- | -------------------------------------------------------------------------------------------- |
| Broadcaster strings        | `RECEIVED`, `SEEN_ON_NETWORK`, `MINED`, `REJECTED`, …                                        |
| `TxStatus` (`chain`)       | received, unconfirmed, mined, rejected, double-spent                                         |
| `txState` (host interface) | mined, seen, rejected, unknown                                                               |
| Ledger `TxState`           | prepared, exported, submission_unknown, submitted, observed, confirmed, rejected, conflicted |
| Event layer                | hint vs verified, proven vs seen                                                             |
| Tree ledger                | `proven` 0/1                                                                                 |

Functions: `txState`, `txStatus`, `arcTxStatus`, `fetchTxStatus`, `probeTxStatus`, `statusOf`, `proofLookup`, `utxoStatus`, and `fetchTx` (41 call sites).

**Evidence.** Two bugs this week came from picking the wrong one. The tree upkeep asked `txState`, which by design never says "rejected" from a status word, so a rejected provisioning transaction was never dropped. The legacy funding refresh used the broadcaster's word as a verdict, which is the thing the hint-versus-proof rule forbids.

**Hurts.** Each caller has to know which function is safe for a money decision. Getting it wrong is silent.

**Move.** One `TxFacts` answer from one module: bytes held, proof held, depth, our own verdict, and the outside hint, with the hint in a separate field that cannot be mistaken for a verdict. Delete the other entry points.

### F6. HIGH — about ten schedulers, one wallet, no priorities

**What.** Work is driven by many independent timers:

| Where                   | Schedulers                                                                                                                                        |
| ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| `automation/runtime.ts` | deals tick (250 ms hot, 1 s idle), outbox timer, upkeep every 10 s, watchdog every 60 s, watch refresh every 15 s, one long-poll per watched lane |
| `chain/events.ts`       | observer tick (5 s or 30 s) plus a poke timer                                                                                                     |
| `mcp/runtime.ts`        | confirmer intervals (two), instance heartbeat, status stream, funding discovery                                                                   |
| Sandbox shims           | one poller per watched key                                                                                                                        |

**Evidence.**

- All of them draw coins and slots from the same wallet through `takeFunding`, first come first served. On a fresh identity the identity, log and tree mints ran first and deals found "no funding slot" and "no funding UTXO" while the agent held over a million sats.
- Lane reads carry no cursor (`ingestEvents(tag, events, '')` at `automation/runtime.ts:689,697`), so each pass re-pulls the lane.
- Measured today: alice's identity lane has 0 proven slots and 2 trees waiting for a block, because that lane refills in trees of 4 with a threshold of 2.

**Hurts.** Latency (work waits for the next tick of the right timer), and stalls where background chores take what a deal needed.

**Move.** One scheduler with named jobs and one wake signal, and inside the F1 `Funds` module a simple allocation: deals first, then publishing, then upkeep. Give the identity lane the same refill rule as the others.

### F7. MEDIUM-HIGH — the system fails quietly

**What.** When a deal cannot proceed, the reason is stored in a database field or not at all.

**Evidence from this session.**

- Seller preparation failed because paid disclosure was switched off; the buyer only saw "no qualifying offer".
- Every offer was thrown out as "ineligible" because of a test default; the test printed "deal never completed" and nothing else.
- `provisionPay` returned a transaction id for a payment the network had rejected.
- A stock-out said only "no funding slot" until this week.
  In each case I found the cause by opening the SQLite files by hand.

**Hurts.** Every stall becomes an investigation.

**Move.** Every stopped operation carries a plain reason, and the `deals` tool shows it. Add one "why is this stuck" view that lists, per operation, what it waits on. Refuse to start when settings contradict each other (see F8).

### F8. MEDIUM-HIGH — 162 settings and no named setups

**What.** Behaviour is selected by environment variables: 162 of them, with wallet mode, network, tool profile and feature switches all independent.

**Evidence.** Failures caused only by settings this session: paid disclosure off, operator-only tools hidden under the agent profile, a refund window shorter than the seller's safety margin, indexer discovery on or off changing how funds are found.

**Hurts.** A working setup is a combination you have to remember. A wrong combination fails late and quietly.

**Move.** A handful of named profiles (for example `regtest-dev`, `testnet-agent`, `mainnet-agent`) that set everything, with individual variables as overrides. Check the combination at start and stop with a clear message if it cannot work.

### F9. MEDIUM — the test agent is built in many places

**What.** The code that builds an agent for a test or a script is copied.

**Evidence.** `mkagent` exists in four test files; `openAutomation` is called from 14 sites outside the product; the trading policy literal appears in 15 files; three untracked probe scripts (`packages/mcp/tn-probe.mjs`, `tn-probe2.mjs`, `tn-trees.mjs`) carry more copies. The refund-window bug lived in one copy's defaults.

**Move.** One `testAgent()` builder in a shared test-support module, used by every suite and script. Delete the copies.

### F10. MEDIUM — the working environment drifts

**What and evidence.**

- Two checkouts (Windows and WSL) that diverged once already this week.
- Agents run from the working tree's build output, so a rebuild changes what a restarted agent runs.
- Agent state lives in `.state/` inside the repository, beside more than 300 leftover logs, run folders and probe scripts.
- `e2e` holds 25 entry scripts and 5,685 lines with no tests; `packages/contracts` and `packages/anchor` hold about 20 loose probe scripts.

**Hurts.** "It worked a minute ago" problems, and no clear line between product, experiment and leftover.

**Move.** One checkout (WSL). Run agents from a separate built copy. Keep state outside the repository. Move experiments to one `scratch/` area or delete them.

### F11. MEDIUM — outside single points

**What.** A live agent depends on one indexer and one broadcaster per network. Headers now need two sources to agree, which is good, but one of the two is the broadcaster.

**Evidence.** Indexer rate limits (HTTP 429) slowed or broke funding import several times this session. Raw-transaction lookup gained a fallback this week; coin discovery still has one source.

**Move.** Make discovery optional once F1 lands (the wallet knows its own coins; discovery is only for coins sent by others, and the provisioning path already hands those over directly). Allow a second broadcaster.

### F12. MEDIUM — the map does not cover the part that breaks

**What.** `docs/ARCHITECTURE.md` has 17 sections on memory, anchoring, identity and proofs. It has no section on funds, slots, transaction state or the deal flow. `docs/AUTOMATION.md` describes operation, not structure.

**Hurts.** The only way to understand the money path is to read the giant files in F4.

**Move.** One page with four pictures: where coins live (F1), how a transaction's facts are known (F5), the deal flow as two state tables, and what runs on which schedule (F6). Write it as each stage lands, so it describes the new shape.

## 6. Event layer (event-arch)

The sixteen findings from the earlier event audit are fixed and unit-tested. What remains is structural, and it is F6:

| Topic            | State                                                                                             |
| ---------------- | ------------------------------------------------------------------------------------------------- |
| Hints vs proof   | Sound. Status words only wake; verdicts come from proofs, our own records, or the abandon horizon |
| Wake coverage    | Good on the deal path. Measured live: 5.6 s and 7.5 s from want to verified disclosure            |
| Scheduler count  | About ten (see F6). The skill's rule is one per subsystem                                         |
| Numbered streams | Only the broadcaster status stream is numbered. Lanes and the message box are re-pulled each pass |
| Fanout           | Tracked per host since this week                                                                  |
| Push layer       | The sandbox "watch" is still a poll per key behind a long-poll                                    |

## 7. Proofs (spv-strict)

| Topic                    | State                                                                                                                                                                                                                                        |
| ------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Mined claims             | Computed: bytes, merkle path, header. Headers are proof-of-work checked and need two sources to agree                                                                                                                                        |
| Rejections               | No longer taken from a status word on the live path                                                                                                                                                                                          |
| Settlement               | Waits for depth (6 on live networks)                                                                                                                                                                                                         |
| Proof travels with funds | Fixed this week; the market lane serves from a tree one hop above a proven parent                                                                                                                                                            |
| New identity             | One block, once, with `provisionPlan`/`provisionPay` (verified live with a fresh identity)                                                                                                                                                   |
| Still open               | Coin discovery trusts one indexer's list as a starting point (F11); the identity lane can still run out of proven slots (F6); the test harness and one script were only switched to two-source headers this week and have not been run since |

## 8. Root causes

Four causes explain the twelve findings:

1. **State without an owner.** Coins and transaction status are each known in many places (F1, F5, F7).
2. **Reality as the test bench.** Tests need a live chain and a shared wallet, so wiring is only proven on testnet (F2, F3, F9).
3. **Growth by accretion.** At 33 commits a day, each fix was added inside the nearest large function. The gate that should have pushed back was reset instead (F4, F8, F10, F12).
4. **Timers instead of one plan of work.** Each new chore got its own loop and took from the common wallet (F6, F11).

## 9. Stages at a glance

| Stage | What                                                     | Removes                                             | Done when                                                            |
| ----- | -------------------------------------------------------- | --------------------------------------------------- | -------------------------------------------------------------------- |
| 0     | Save this audit; reset the local chain; one baseline run |                                                     | A full run with real pass/fail numbers, or a recorded reason why not |
| 1     | In-memory test world                                     | The shared wallet as a test dependency              | Money tests run in seconds with no Docker                            |
| 2     | Drop `ttn` and `legacy`; one `Funds` module              | File reserve in product code, scattered spent marks | Coin bookkeeping lives in one place                                  |
| 3     | One `TxFacts` answer                                     | Eight other status entry points                     | One way to ask about a transaction                                   |
| 4     | One start-up path with adapters                          | Duplicate start-up code                             | Memory, local chain and testnet share the wiring                     |
| 5     | Split the five giants                                    | Size and mixing                                     | No product file above about 600 lines; gate green                    |
| 6     | One scheduler                                            | Separate timers                                     | A fresh identity trades while upkeep runs                            |
| 7     | Loud failures, named profiles                            | Silent stalls, setting guesswork                    | `deals` says why; bad settings refuse to start                       |
| 8     | Clean-up and the money-path page                         | Copies and leftovers                                | One builder, one checkout, tidy tree                                 |

## 10. What I did not measure

- `check:knip`, `check:dupes` and `check:leftovers` were not run for this audit.
- Co-change between files without an import (temporal coupling) was not measured.
- Fix density by commit keywords came out low (about 10% on the hot files). With a 16-day history and commit messages that rarely say "fix", that number says little; I did not use it.
- The "twelve combinations, about three tested" figure in F3 is my reading of the test files, not a coverage run.
- I did not audit `packages/zk`, `contracts`, `graph`, `datalog`, `identity` or `reasoning` beyond their size and edges. Nothing in the measurements pointed at them.
- The design-guardrails and code-review skills were not applied; you named arch-review, event-arch and spv-strict.
- Line counts for the five giants are from function start to the next top-level declaration, so they are approximate to within a few lines.

## 11. Decisions taken

1. Drop the `legacy` wallet mode: **yes**.
2. Drop `ttn` (Teratestnet): **yes**.
3. Reset the local regtest chain: **yes**.
4. Save this audit into the repo as `docs/SYSTEM-AUDIT.md`: **yes**.
5. Start with the in-memory test world: **yes**.
