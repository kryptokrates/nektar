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

**Score:** ~9/10 (was ~8). The grant channel is a real endpoint now:
`POST /v1/faucet {address}` — unauthenticated by design, one drip per
address + daily cap persisted in `faucet-drips.json`, opt-in via
`NEKTAR_FAUCET=1` (hard-refused on main), the spend itself still rides
SqlFunds + SpendGuard. JOIN-TESTNET names the self-serve curl; the issue
channel stays as fallback.
**Residual:** needs the hub operator to flip the flag and keep the drip
key funded — infrastructure, not code.

## J2 · Fresh instance → non-empty first run

**First value:** `recall` returns real content. **Target:** <5 min.
**Artifact:** `openRuntime` boot path, dir-lane announce import.

| Step                 | Time   | Effort              | Knowledge | Triage      |
| -------------------- | ------ | ------------------- | --------- | ----------- |
| start container      | 1 min  | none                | —         | keep        |
| bootstrap host union | <1 min | none (automatic)    | —         | keep — done |
| pack import          | ~1 min | none (seed domains) | —         | keep — done |
| `recall` answers     | —      | one call            | —         | keep        |

**Score:** ~9/10 (was ~8-with-a-hole — the hole is closed). Layer 0
(`mcp/src/welcome.ts`): a genuinely empty boot seeds a small `welcome`
shard through `mem.remember` — provenance `nektar-welcome`, one-shot via
a dataDir marker, never a boot failure, `NEKTAR_SEED_WELCOME=0` opts
out. The first `recall` now answers even with every seed host dark (the
baked `nektar.run` pair 502'd during this fix — exactly the failure
mode). Layer 2 (dir-lane pack import, `NEKTAR_SEED_DOMAINS=*` baked)
still adds live operator content when hosts answer.

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

**Score:** ~8/10 — every starvation class has a shipped fix and all 13
tests pass on live infrastructure (chunked fresh-process runs; the
single-process ceiling was the Node heap, raised to 8GB in
`test:testnet` — a literal clean single run remains to be demonstrated
on a box that can hold it). Cold-boot carriers now ride anchored trees:
`NEKTAR_ANCHORED_LANES=all` is the image/agent-compose default, so the
first carrier pays a broadcast RTT instead of a block (unpatched hosts
refuse the carrier and the lane falls back to proven-only — visible,
self-healing).
**The honest note:** deal friction is physics-limited (block cadence,
mining), but every _unnecessary_ wait inside that physics has been
named and bounded.

## J5 · Broken thing → named cause

**First value:** an operator can self-diagnose without us. **Target:**
error → legible cause in logs, instantly. **Artifact:** log surface +
`doctor` + phase tagging.

| Failure                | Before                            | Now                                                              |
| ---------------------- | --------------------------------- | ---------------------------------------------------------------- |
| wedged upkeep          | silent `pumping` forever          | `upkeep(finger) exceeded …`                                      |
| dead host stalls sends | serial pump wedged                | bounded delivery, transport-class retry                          |
| poisoned journal       | retries forever, 80 poisoned rows | dead-ancestry eviction + `beefGaps` skip                         |
| suite quiet = wedge?   | indistinguishable                 | `[suite quiet <n>s] last: <line>`                                |
| db dies, hosts loop    | `ENOTFOUND db` ×2,400 restarts    | `restart: unless-stopped` on `db` (was the only service without) |

**Score:** ~8/10 (was ~7 — `doctor` now returns `hints[]`: each
degraded check names its unblock command — unsealed → set
MARKET_HOSTS, unfunded → address + fundingImport, anchors proving →
nothing to do).

## Cross-cutting walls

Ranked by leverage — where the next friction dollar goes:

1. ~~**Funding acquisition** (J1)~~ — DONE: `POST /v1/faucet` is the
   self-serve endpoint (opt-in, capped, persisted); JOIN-TESTNET names
   the curl. Remaining: the operator keeps the drip key funded.
2. ~~**Lane-stock cold start** (J4, internal)~~ — DONE at the default:
   `NEKTAR_ANCHORED_LANES=all` is now the image + agent-compose default
   (was hub-only), so a cold agent's first carrier rides an anchored
   mint, not a block wait. Pre-proven seed stock remains the further
   lever if an unpatched-host path still waits.
3. **Tool contract freeze** (J3) — integrators can't pin against
   "whatever the server exposes". A named minimal contract
   (`recall`/`commit`/`verify` + `automation`) is the adapter MVP's
   actual deliverable.
4. **`doctor` → next-command mapping** (J5) — health output that names
   the unblock command per finding.
5. **The grant/faucet link** — a funding ask that isn't a conversation.
6. **Hidden authority** — behavior gates that live in env vars instead
   of the approvable policy surface (`NEKTAR_PAID_DISCLOSURE` was one —
   `sell` simply wasn't in the tool list and no approved policy could
   grant it). An agent can't discover them and an operator can't grant
   them without a restart and a docs dive. Rule: a capability gate that
   an operator should be able to grant belongs in the policy schema;
   env is an override, never the only lever.
