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

| Step                   | Time      | Effort                                                     | Knowledge       | Triage                                                                                                                                 |
| ---------------------- | --------- | ---------------------------------------------------------- | --------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| `install.sh` one-liner | 5 min     | one command (`NEKTAR_PUBLIC_URL`), four secrets auto-gen'd | docker only     | **MOVED** — `NEKTAR_PUBLIC_URL` derives market/push/watch URLs automatically; compose forwards all `.env.example` keys (CI-tested)     |
| funding                | 0–10 min  | send testnet sats or curl `/v1/faucet`                     | none (auto-key) | **MOVED** — `loadFeeKey` generates `fee.key.json`; boot log & `doctor` print faucet curl; `WAIT_FUNDING`+`INDEXER_DISCOVERY` forwarded |
| public URL/TLS         | 10–30 min | DNS+proxy                                                  | DNS, TLS, ports | KEEP-LEGIBLE — inline Caddyfile proxies `/hosts/*`, `/watch/*`, AND fallback `localhost:8080` (`/mcp`, `/v1/*`, `/.well-known/*`)      |
| verify card            | 1 min     | one curl                                                   | —               | keep                                                                                                                                   |

**Score:** 8/10 (re-audited from unmeasured ~9/10; parity gate measured at
4.4ms in `packages/mcp/test/friction.test.ts`; live VPS end-to-end join time
remains to be clocked on a fresh third-party host). Every `.env.example` key
in `deploy/hub` and `deploy/agent` is now forwarded in `compose.yaml` and
CI-asserted; `NEKTAR_PUBLIC_URL` collapses 4 URL vars into 1; Caddyfile now
routes `/mcp` & `/v1/*`; `bin.ts` warns when `NEKTAR_FAUCET=1` runs against
the default 20k/day SpendGuard cap.
**Residual:** live VPS stopwatch measurement + operator keeping the drip key
funded.

## J2 · Fresh instance → non-empty first run

**First value:** `recall` returns real content. **Target:** <5 min.
**Measured (in-process HTTP + welcome seed, `friction.test.ts`):** **889ms**
(boot→`ready` and `first-recall (TTFV)` are now logged by `bin.ts`).
**Artifact:** `openRuntime` boot path, `welcome.ts`, `recall` omitted-shard discovery, `tools/restate-up.mjs`, `tools/init.mjs`.

| Step                       | Time   | Effort                 | Knowledge | Triage                                                                                                                                  |
| -------------------------- | ------ | ---------------------- | --------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| start container            | 15–60s | one command            | docker    | KEEP-LEGIBLE — `Dockerfile` defaults to 10-tool `agent` profile; `README.md` notes first-boot sidecar pull                              |
| welcome + pack seed        | <1s    | none (automatic)       | —         | keep — Layer 0 (`welcome`) + Layer 2 (`NEKTAR_SEED_DOMAINS=*`)                                                                          |
| `recall {}` answers        | <1s    | one call, zero args    | —         | **KILLED** — `shard` is optional on `recall` (omitted returns `shards` + items; wrong shard hints names); `doctor` exposes `shardNames` |
| source dev (`init`/`demo`) | <60s   | `pnpm restate:up` once | —         | **MOVED** — `pnpm restate:up` installs the SHA256-verified sidecar into `packages/mcp/restate/`; `init.mjs` surfaces stderr on failure  |

**Score:** 9/10 (in-process TTFV measured at 889ms; container cold-pull
remains network-bound and named in `README.md`).

## J3 · External AI client → first tool call

**First value:** `recall` from Claude Code/Codex/OpenClaw/Hermes/curl.
**Target:** <3 min. **Measured (`friction.test.ts`):** **889ms** (HTTP
`POST /v1/tools/recall` with `{"shard":"welcome"}` and `{}`); CLI setup
scripts (`setup-claude.mjs` + `setup-codex.mjs`) verified in **1.6s**.
**Artifact:** MCP stdio/HTTP + `/v1` surface + `docs/ADAPTERS.md` + `tools/setup-*.mjs`.

| Client                    | Path                                      | Friction                                                                                 |
| ------------------------- | ----------------------------------------- | ---------------------------------------------------------------------------------------- |
| Claude Code               | `setup:claude` merges `.mcp.json` + hooks | **KILLED** — now registers MCP server + hooks + `CLAUDE.md`, supports `--port`/`--token` |
| Codex                     | `setup:codex` merges config               | **KILLED** — supports `--port`/`--url`/`--token` and reads `.env` defaults               |
| OpenClaw / any MCP-native | MCP stanza paste                          | keep — 10-tool `agent` profile consistent across README, JOIN-AGENT, INTEGRATION         |
| Hermes / function-calling | `/v1/tools` + `/v1/openapi.json`          | keep                                                                                     |
| raw HTTP                  | `POST /v1/tools/recall`                   | **KILLED** — `ADAPTERS.md` contract & curl fixed (`{"shard":"welcome"}` / `{}`)          |

**Score:** 9/10 (all docs aligned on 10 agent tools vs operator opt-in;
`ADAPTERS.md` curl payload regression-tested in `friction.test.ts`).

## J4 · Agent → settled deal

**First value:** policy approved → `want → quote → escrow → disclosure → settled`.
**Target:** bounded by watch+poll; p50 visible. **Measured policy setup
(`friction.test.ts`):** **578ms** (`nektar policy init` + `approve --preset both`

- `list` + `revoke` without Restate or git checkout).
  **Artifact:** `packages/mcp/src/policy-cli.ts`, `docs/MARKET.md`, deal path.

| Phase                                | Live wall hit                | Fix landed                                                                                |
| ------------------------------------ | ---------------------------- | ----------------------------------------------------------------------------------------- |
| no-build policy approval             | required git + `pnpm`        | `nektar policy approve --preset buyer\|seller\|both` built into `bin.ts` (Docker + host)  |
| README "Trading guide" link          | pointed to internal debt log | retargeted to `docs/MARKET.md`; `.devin/automation.policy.example.json` fixed (`testnet`) |
| want dies before slow quote          | T2 failure                   | `wantExtendMs` republish — keep-alive at collect boundary                                 |
| mint + escrow share one UTXO         | T1/T4 starvation             | `reserveOuts` fan-out — medium outputs on hand                                            |
| card evicted before tree proves      | announce loss                | keep-alive every 4 min inside host's 10-min window                                        |
| accept parks behind dead-host repair | R1 starvation                | pooled outbox pump + `deliverBoundMs`                                                     |
| host refuses carrier — thin ancestry | carrier refusal              | `NEKTAR_ANCHORED_LANES` + vendored bbox patch                                             |
| wedged leg parks whole pump          | silent wedge                 | `deliverBoundMs`, `bound()` on upkeep legs                                                |

**Score:** 8.5/10 — no-build policy approval is now a single command with
built-in presets (keeping explicit human approval and `I_UNDERSTAND` mainnet
gate); all 13 live-infrastructure deal tests pass on chunked runs.
**The honest note:** deal friction is physics-limited (block cadence,
mining), and every _unnecessary_ wait and setup cliff inside that physics
is now named, bounded, or eliminated.

## J5 · Broken thing → named cause

**First value:** an operator can self-diagnose without us. **Target:**
error → legible cause in logs, instantly. **Measured (`friction.test.ts`):**
**504ms** (`doctor` standalone/transport/faucet hint checks); **1.6s**
(`tools/doctor.mjs` Restate + lock diagnostics).
**Artifact:** log surface + `doctor` + `tools/doctor.mjs` + `tools/init.mjs` + phase tagging.

| Failure                                   | Before                                   | Now                                                                                  |
| ----------------------------------------- | ---------------------------------------- | ------------------------------------------------------------------------------------ |
| `init` alive-check fails                  | stderr discarded; generic "run doctor"   | prints stderr tail + exact `pnpm restate:up` unblock when `RESTATE_REQUIRED`         |
| `doctor.mjs` / `automation.mjs` unset dir | `ERR_INVALID_ARG_TYPE` crash             | defaults to `.nektar`; `doctor.mjs` probes Restate (`:19070`/`:9070`/sidecar binary) |
| solo `doctor` output                      | `bootstrap: 'unsealed'` + HTTP-only hint | `bootstrap: 'standalone'`, `shardNames[]`, transport-aware `fundingImport` + faucet  |
| wedged upkeep                             | silent `pumping` forever                 | `upkeep(finger) exceeded …`                                                          |
| dead host stalls sends                    | serial pump wedged                       | bounded delivery, transport-class retry                                              |
| poisoned journal                          | retries forever, 80 poisoned rows        | dead-ancestry eviction + `beefGaps` skip                                             |
| suite quiet = wedge?                      | indistinguishable                        | `[suite quiet <n>s] last: <line>`                                                    |
| db dies, hosts loop                       | `ENOTFOUND db` ×2,400 restarts           | `restart: unless-stopped` on `db`                                                    |

**Score:** 9/10 — every known silent cliff (`init` stderr, unset
`NEKTAR_DATA_DIR`, missing Restate check, misleading standalone `unsealed`
hint, silent compose env drops) is fixed and regression-tested.

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
