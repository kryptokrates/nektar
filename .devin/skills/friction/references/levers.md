# Friction-Removal Levers — the Catalog

## Table of Contents

- [Move Levers (work off the user's clock)](#move-levers)
- [Kill Levers (dead friction)](#kill-levers)
- [Legibility Levers (load-bearing friction made visible)](#legibility-levers)
- [Conform Levers (Jakob's law)](#conform-levers)
- [Bumper Levers (recover the strays)](#bumper-levers)
- [Sequencing Levers](#sequencing-levers)
- [The Underdog Stack](#the-underdog-stack)

## Move Levers

Necessary work the user never waits on — done earlier, by the system,
or by us.

| Lever                         | Mechanic                                                                             | Repo examples                                                                       |
| ----------------------------- | ------------------------------------------------------------------------------------ | ----------------------------------------------------------------------------------- |
| **Correct default**           | A decision with a right answer becomes a default; the field exists for opt-in only   | `NEKTAR_MARKET_HOSTS` optional — plural baked-in bootstrap + dir-lane discovery     |
| **Generated secrets**         | Installer produces the secrets; the doc stops asking humans to invent randomness     | `install.sh` / `.env.example` `openssl rand -hex 32` pattern                        |
| **Pre-warmed stock**          | The system pays block-wait before the user needs the output                          | `tools/tn-warmup.mjs`, persistent `lanes.sqlite`, provision trees on boot           |
| **Pre-seeded content**        | First-run experience is non-empty because packs/records are bundled or auto-imported | `NEKTAR_SEED_DOMAINS`, dir-lane pack import at MCP boot                             |
| **Baked artifacts**           | Compile/bundle at release, not at install                                            | `bundle/`, derived ghcr images (`nektar-hub-db`, `nektar-shim`, `nektar-bbox-host`) |
| **Async load-bearing checks** | Verification runs; the user is told it's safe to proceed rather than parked          | anchored admission — carrier rides while funding proof lands                        |
| **Lazy opt-in**               | Heavy capability activates on first need, not at install                             | `log` lane warms lazily — off the deal path                                         |
| **Self-healing infra**        | Restart policies and healthchecks replace babysitting                                | `restart: unless-stopped`, db-then-hosts ordering, `depends_on: healthy`            |

## Kill Levers

Steps that exist for the implementer's convenience — the user's time
donated to our architecture.

| Lever                        | Mechanic                                                                | Repo examples                                                          |
| ---------------------------- | ----------------------------------------------------------------------- | ---------------------------------------------------------------------- |
| **Checkoutless deploy**      | Images pull; no clone, no build, no mount                               | `deploy/hub` — compose + `.env` only (shipped)                         |
| **One-command up**           | The install is one line; everything else is flags                       | `docker compose up -d` + one env file                                  |
| **Fewer required fields**    | If it has a default or can be discovered, it isn't a question           | `.env` audit: four secrets → generated; URLs → advertised not required |
| **Merged agents**            | One process does what two used to (fewer keys, fewer locks, fewer docs) | agent service carries MCP + automation in one container                |
| **No second query language** | The lookup surface is the one users already hit                         | `ls_nektar` queries — `/lookup`, `/v1/tools/*`                         |
| **Die instead of leak**      | A doomed state evicts itself rather than retrying forever               | dead-ancestry two-strike eviction; bound-fired deliveries dead-letter  |
| **Skip the unverifiable**    | If a thing can never verify, stop spending retries on it                | finger `beefGaps`/`servable` gate — gapped bundles skip, not loop      |

## Legibility Levers

Load-bearing friction stays — but it stops being silent.

| Lever                       | Mechanic                                                                               | Repo examples                                      |
| --------------------------- | -------------------------------------------------------------------------------------- | -------------------------------------------------- |
| **Named waits**             | Every blocking phase logs its name while it waits                                      | `upkeepPhase` tagging: `upkeep(finger) exceeded …` |
| **Bounded phases**          | A leg that never settles logs and abandons rather than wedging the loop                | `bound()` around poll/tick/upkeep legs (120–300s)  |
| **Progress-visible proofs** | "Proving — safe to use meanwhile" instead of a parked user                             | anchored trees serve while the BUMP lands          |
| **Honest gates**            | Mainnet requires `NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND` — named, deliberate, documented | JOIN-MAINNET gate table                            |
| **Named failure classes**   | A failure says transport vs funding-slot vs policy — the operator knows who to blame   | outbox `defer_class`, `repair-host` rows           |
| **Heartbeat under silence** | A quiet suite prints its last line so silence ≠ wedge                                  | `[suite quiet <n>s] last: …`                       |

## Conform Levers

The surface looks like what they already run — the learning curve
deletes itself.

| Lever                      | Mechanic                                                            | Repo examples                                                               |
| -------------------------- | ------------------------------------------------------------------- | --------------------------------------------------------------------------- |
| **MCP stanza + Registry**  | Standard config block + official `server.json` discovery            | `server.json`, `nektar dock`, `setup:claude`, `setup:codex`                 |
| **Native memory slot**     | Plug into the harness's own memory provider / plugin hook interface | `integrations/hermes` (`MemoryProvider`), `integrations/openclaw` (slot)    |
| **Native plugin bundle**   | One manifest bundles MCP + hooks + `agentskills.io` `SKILL.md`      | `.claude-plugin/plugin.json` + `marketplace.json` + `skills/nektar-memory/` |
| **One brain, many mouths** | Single local loopback daemon + auto-attach stdio↔HTTP bridge        | `nektar` companion HTTP + `mcp-bridge.ts` (zero `instance.lock` clashes)    |
| **HTTP + OpenAPI**         | Non-MCP clients call routes they already understand                 | `GET /v1/tools`, `POST /v1/tools/{name}`, `/v1/openapi.json`                |
| **compose shape**          | Standard docker-compose ops flow                                    | `deploy/hub/compose.yaml` + `.env`                                          |
| **bearer auth**            | Token in env, header in request — nothing new                       | `NEKTAR_HTTP_TOKEN`                                                         |
| **Ecosystem names**        | `tools`, `health`, `token`, `compose.yaml`, `helm`                  | chart at `deploy/hub/helm`                                                  |
| **WIF + address formats**  | The keys look like every other BSV key                              | testnet `m…`/`n…`, standard WIF                                             |

## Comparative Ecosystem Benchmark (Zero-Config Docking)

What leading AI agent harnesses and memory layers do to erase friction,
and the exact Nektar lever (preserving Nektar's verification & custody
boundaries):

| Peer / Harness | Their Zero-Config Lever | Nektar Equivalent | Ethical / Honesty Boundary Kept |
| --- | --- | --- | --- |
| **Hermes Agent** | `MemoryProvider` ABC (`prefetch`, `on_memory_write`, `sync_turn`, `config_schema.py`), `hermes plugins install`, `hermes memory setup/status`, `spawn_context_thread`, secret scrubbing | `integrations/hermes/` (`hermes-memory-nektar` with `plugin.yaml` + `entry_points`), auto-discovers loopback Nektar, mirrors `MEMORY.md`/`USER.md`, exposes `groundedAnswer` + market tools | Non-blocking bounded prefetch (800ms); raw turn capture opt-in; Hermes secret-scrubbing honored; truth caveat preserved |
| **OpenClaw** | Exclusive `plugins.slots.memory`, `openclaw.plugin.json`, typed `api.on("before_prompt_build" \| "agent_end")`, `openclaw doctor`, alive-check before trust | `integrations/openclaw/` (`openclaw.plugin.json`, `before_prompt_build` prefetch, `agent_end` receipt check + native tools) | Read-only fail-open prefetch; no hidden spend authority |
| **Claude Code / Codex** | `.claude-plugin/plugin.json` + `marketplace.json` bundling `.mcp.json` + hooks + `SKILL.md`; portable commands | `.claude-plugin/` + `skills/nektar-memory/SKILL.md` + `nektar hook session-start\|prefetch\|verify-receipt` (zero checkout paths) | Stop hook blocks once on failed/drifted receipt without infinite loops |
| **Supermemory Local / Ollama** | Single local daemon on a fixed port; zero-env boot; every client on the machine shares one brain | Zero-env `testnet` default + companion loopback HTTP (`:18420`) + `mcp-bridge.ts` auto-attach on stdio + `nektar dock auto` | Single-writer `instance.lock` preserved inside the daemon; no unauthenticated non-loopback bind |
| **Honcho / Hindsight / Mem0** | One-line `plugins install` / `npx connect auto`, bounded context injection, background extraction | `nektar dock auto` across 9 clients + live `recall {}` verification probe before exit | Explicit policy approval CLI-only (`nektar policy approve`) — never hidden behind auto-install |

## Bumper Levers

Users stray; the system returns them to the line without a human.

| Lever                      | Mechanic                                                           | Repo examples                                                          |
| -------------------------- | ------------------------------------------------------------------ | ---------------------------------------------------------------------- |
| **Next-command errors**    | Errors name the command that unblocks, not just the cause          | `lane stock unproven … run tools/tn-warmup.mjs or set TN_WARMUP=1`     |
| **One-call health**        | A single tool reports everything a stranger needs                  | `doctor`, `/v1/automation/preflight`, `/v1/automation/health`          |
| **Reconcile on boot**      | The system fixes its own drift instead of demanding the user do it | `reconcileLiveReserve`, finger self-heal per pass, announce keep-alive |
| **Resume, not restart**    | Interrupted work resumes where it died — re-running costs nothing  | persistent `lanes.sqlite`, journal replay, outbox durability           |
| **Verifiable checkpoints** | Each doc step ends with "you should see X"                         | JOIN-TESTNET's `/lookup` card check                                    |

## Sequencing Levers

Decisions happen when they're needed — never before.

| Lever                         | Mechanic                                                      | Repo examples                                       |
| ----------------------------- | ------------------------------------------------------------- | --------------------------------------------------- |
| **Ask late**                  | Funding key demanded when funding is needed, not at install   | agent runs read-only paths unfunded                 |
| **Progressive disclosure**    | Power knobs documented but absent from the straight line      | `NEKTAR_*` knobs live in ENVIRONMENT.md, not step 1 |
| **Tiered testnet**            | Free testnet first; mainnet asks for confirmation separately  | `NEKTAR_NETWORK` + `NEKTAR_MAINNET_CONFIRM`         |
| **Single-player value first** | Tool useful alone before network needed — "come for the tool" | memory/recall works solo; deals come later          |
| **Directory before config**   | Discovery finds peers so the user doesn't name them           | dir-lane announces + union at boot                  |

## The Underdog Stack

Ordering when choosing what to build next, per worse-is-better:

1. **Does it run without us?** (no `nektar.run` dependency, no
   checkout, no build)
2. **Does it start non-empty?** (seeded content, discovered peers,
   warmed stock)
3. **Does it conform?** (known shapes — MCP, compose, bearer, OpenAPI)
4. **Is the straight line ≤5 steps?** (count them)
5. **Are the waits named and bounded?** (no silent anything)
6. **Does the error name the next command?**
7. Only then: new capability.

A feature that adds a step loses to a feature that removes one.
