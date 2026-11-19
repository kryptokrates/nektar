# nektar — Live Friction Map

The five journeys that decide whether an underdog network gets adopted,
what each pays in the three currencies today, and where the wall is.
Re-score after every friction PR. (Baseline: the user's quote — "any
competing underdog system needs less friction to adoption, rather than
more.")

## Table of Contents

- [J1 · Stranger → announced operator](#j1--stranger--announced-operator)
- [J2 · Fresh instance → non-empty first run](#j2--fresh-instance--non-empty-first-run)
- [J3 · External AI client → first tool call](#j3--external-ai-client--first-tool-call)
- [J4 · Agent → settled deal](#j4--agent--settled-deal)
- [J5 · Broken thing → named cause](#j5--broken-thing--named-cause)
- [Cross-cutting walls](#cross-cutting-walls)

## J1 · Stranger → announced operator

**First value:** their signed card visible on the dir lane +
`doctor` green. **Target:** <30 min. **Artifact:** `docs/JOIN-TESTNET.md`

- `deploy/hub/` + `deploy/agent/`.

| Step                   | Time      | Effort                               | Knowledge                  | Triage                                                                                                                        |
| ---------------------- | --------- | ------------------------------------ | -------------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| `install.sh` one-liner | 5 min     | one command, four secrets auto-gen'd | docker only                | keep — shipped                                                                                                                |
| `NEKTAR_MARKET_HOSTS`  | —         | decide                               | "what are my public URLs?" | KEEP (advertising requires it) — seed pair documented                                                                         |
| funding                | 0–10 min  | send testnet sats to printed address | none (auto-key)            | **MOVED** — `loadFeeKey` generates `fee.key.json`; `WAIT_FUNDING`+`INDEXER_DISCOVERY` hold+detect (compose passthrough added) |
| public URL/TLS         | 10–30 min | DNS+proxy                            | DNS, TLS, ports            | KEEP — inline Caddyfile now in doc (was a dead-end pointer)                                                                   |
| verify card            | 1 min     | one curl                             | —                          | keep                                                                                                                          |

**Score:** ~8/10 (was ~6 — the stranger's two hardest steps were one
broken instruction (hex≠WIF) and one dead-end pointer (no Caddyfile).
Both fixed; the WIF step deleted entirely via auto-key + wait-funding).
**Biggest remaining wall:** the grant channel is still conversational
(`TESTNET-GRANT` issue title named — needs a real endpoint or faucet).

## J2 · Fresh instance → non-empty first run

**First value:** `recall` returns real content. **Target:** <5 min.
**Artifact:** `openRuntime` boot path, dir-lane announce import.

| Step                 | Time   | Effort              | Knowledge | Triage      |
| -------------------- | ------ | ------------------- | --------- | ----------- |
| start container      | 1 min  | none                | —         | keep        |
| bootstrap host union | <1 min | none (automatic)    | —         | keep — done |
| pack import          | ~1 min | none (seed domains) | —         | keep — done |
| `recall` answers     | —      | one call            | —         | keep        |

**Score:** ~8/10 — zero-config discovery + seed import landed; the
remaining question is _what packs ship_ and whether a stranger's domain
list matches. If the first answer is "we ship nothing yet", the whole
journey fails at the value moment regardless of speed.

## J3 · External AI client → first tool call

**First value:** `recall` from Claude Code/Codex/OpenClaw/Hermes.
**Target:** <3 min. **Artifact:** MCP stdio/HTTP + `/v1` surface.

| Client                    | Path                            | Friction                                  |
| ------------------------- | ------------------------------- | ----------------------------------------- |
| Claude Code               | `setup:claude` merges config    | keep — one command                        |
| Codex                     | `setup:codex`                   | keep — one command                        |
| OpenClaw / any MCP-native | MCP stanza paste                | **MOVE → publish per-platform snippet**   |
| Hermes / function-calling | needs `/v1/tools` + HTTP bearer | **MOVE → thin shim doc + OpenAPI stanza** |
| raw HTTP                  | `/v1/tools/{name}` + bearer     | keep                                      |

**Score:** ~8/10 (was ~6 — `docs/ADAPTERS.md` now freezes the minimal
contract (remember/recall/groundedAnswer/verifyReceipt/doctor/
automation + want/sell/deals) and ships every stanza: Claude Code,
Codex, OpenClaw mcpServers JSON, Hermes HTTP-shim recipe over
`/v1/openapi.json`, raw curl).

## J4 · Agent → settled deal

**First value:** `want → quote → escrow → disclosure → settled`.
**Target:** bounded by watch+poll; p50 visible. **Artifact:** the whole
deal path (this suite's battleground).

| Phase                                | Live wall hit    | Fix landed                                                |
| ------------------------------------ | ---------------- | --------------------------------------------------------- |
| want dies before slow quote          | T2 failure       | `wantExtendMs` republish — keep-alive at collect boundary |
| mint + escrow share one UTXO         | T1/T4 starvation | `reserveOuts` fan-out — medium outputs on hand            |
| card evicted before tree proves      | announce loss    | keep-alive every 4 min inside host's 10-min window        |
| accept parks behind dead-host repair | R1 starvation    | pooled outbox pump + `deliverBoundMs`                     |
| host refuses carrier — thin ancestry | carrier refusal  | `NEKTAR_ANCHORED_LANES` + vendored bbox patch             |
| wedged leg parks whole pump          | silent wedge     | `deliverBoundMs`, `bound()` on upkeep legs                |

**Score:** ~6/10 — every class has a fix shipped or in-flight; the
score goes to 8+ when the full suite passes clean in a single run.
**The honest note:** deal friction is physics-limited (block cadence,
mining), but every _unnecessary_ wait inside that physics has been
named and bounded.

## J5 · Broken thing → named cause

**First value:** an operator can self-diagnose without us. **Target:**
error → legible cause in logs, instantly. **Artifact:** log surface +
`doctor` + phase tagging.

| Failure                | Before                            | Now                                      |
| ---------------------- | --------------------------------- | ---------------------------------------- |
| wedged upkeep          | silent `pumping` forever          | `upkeep(finger) exceeded …`              |
| dead host stalls sends | serial pump wedged                | bounded delivery, transport-class retry  |
| poisoned journal       | retries forever, 80 poisoned rows | dead-ancestry eviction + `beefGaps` skip |
| suite quiet = wedge?   | indistinguishable                 | `[suite quiet <n>s] last: <line>`        |

**Score:** ~8/10 (was ~7 — `doctor` now returns `hints[]`: each
degraded check names its unblock command — unsealed → set
MARKET_HOSTS, unfunded → address + fundingImport, anchors proving →
nothing to do).

## Cross-cutting walls

Ranked by leverage — where the next friction dollar goes:

1. **Funding acquisition** (J1) — the one journey a stranger cannot
   complete without outside action. A grant-link or faucet name in the
   doc converts a stall into a step.
2. **Lane-stock cold start** (J4, internal) — a fresh agent pays a
   block-wait before its first carrier. `NEKTAR_ANCHORED_LANES=all` is
   now the hub compose default (our `nektar-bbox-host` image carries
   the anchored-admission patch) — anchored trees serve every lane
   while their proof is in flight. Pre-proven seed stock remains the
   further lever if a fully cold start still waits.
3. **Tool contract freeze** (J3) — integrators can't pin against
   "whatever the server exposes". A named minimal contract
   (`recall`/`commit`/`verify` + `automation`) is the adapter MVP's
   actual deliverable.
4. **`doctor` → next-command mapping** (J5) — health output that names
   the unblock command per finding.
5. **The grant/faucet link** — a funding ask that isn't a conversation.
