# Nektar v0.1 Cold-Start Playbook — Atomic Networks, Hard Side & Tipping Bars

Applying Andrew Chen's _The Cold Start Problem_ framework (`.devin/skills/cold-start`) to Nektar v0.1's pure-Go single-binary launch.

**Strategy in one sentence:** `nektar up` gives every single user immediate standalone value in < 25 seconds ("come for the verifiable memory tool"), while `nektar.run` acts as honest Person #1 (curated starter CHIRP packs + transparent tapered Hub bounties + Backbone Spine), letting us tip **one agent-platform cohort at a time** against mechanical liquidity bars before expanding.

---

## 1. Atomic Network #1 — Claude Code Operators

We launch to **one named atomic network**, not "all AI developers":

- **Who:** Daily Claude Code operators debugging infrastructure, BSV/crypto, and agent workflows.
- **Why Cohort #1:** Highest density achievable (full L3 native hooks + embedded `SKILL.md` + `nektar up` 25s install), immediate burning pain (agents re-solving the same ops failures without verifiable recall), and direct adjacency to Codex CLI (Cohort #2) and Hermes/OpenClaw (Cohort #3).
- **Derived Atomic Size:** **5–10 active docked agents** across 3 seed domains (`ops`, `bsv`, `agents`) with at least **3 non-hub organic sellers** so a `want` receives a competing verified offer in **< 60 seconds**.

### Two-Tier Magic Moment & Zero States
- **Read-Tier Magic Moment (Single-Player Wedge, n = 1):** When a newly docked Claude Code agent runs `remember` / `recall` / `groundedAnswer`, it produces a verified `nektarr1.*` receipt + checkable `verify:` URL within **30 seconds** of install (verified live during `nektar up`).
- **Market-Tier Magic Moment (Network Proof):** When a funded agent publishes `want {domains, question, maxSats}`, an atomic BRC-369 disclosed deal settles with SPV proof in **< 60 seconds** (live BSV Mainnet benchmark: `p50 = 1.93s`).
- **Zero States (Tracked via `GET /v1/coldstart/health`):**
  - Read zero: `recall {}` returning 0 items (eliminated by `welcome` shard + default Layer-2 `NEKTAR_SEED_DOMAINS=ops,bsv,agents`).
  - Market zero: `want` receiving 0 offers in a covered domain (`zeroRate` bar: **< 10%**).

---

## 2. Solving the Hard Side (Knowledge Sellers & Hub Operators)

Every knowledge market has two hard sides who do real work:

| Hard Side | Dominant Motivation | What Nektar Ships for Them | Time-to-First-Value Target |
|---|---|---|---|
| **Knowledge Sellers** (agents curating verified facts) | Money (instant BSV sats) + Utility (better local memory) | `nektar earn` guided bounty flow, instant AtomicBEEF Hub bounty payouts, 24/7 autonomous `sell` quoting under SpendGuard caps, seller completion track record | **< 3 minutes** from anchored fact to first earned sats |
| **Public Hub Operators** (Leaf / Spine relay runners) | Money (4-valve BRC-105 hybrid fees) + Status (`bfinger` identity) | 60-second `nektar hub up --native` (zero Docker) or Docker + Caddy auto-HTTPS, live earnings ledger, transparent Spine peering | **< 60 seconds** from VPS command to earning Hub |

### Honest Flintstoning & Published Subsidy Taper Schedule
Hubs seed real, useful starter packs (`ops/nektar-guide`, `ops/bsv-spv`, `ops/agent-grounding`, explicitly labeled `hub:curated` and `isHub: true` — **never spoofed as independent peers**) plus open domain bounties (`bounty-ops`, `bounty-bsv`, `bounty-agents`).

**Published Bounty Taper Schedule** (exposed live on `GET /hub/v1/terms` and `GET /v1/coldstart/health`):

| Phase | Trigger | Hub Bounty Reward | Exit Rule |
|---|---|---|---|
| **Phase 1 — Launch Seeding** | Fills `1–25` per domain | **100 sats** / SPV-anchored fact | Published upfront; never silently cut |
| **Phase 2 — Step-Down** | Fills `26–50` per domain | **50 sats** / SPV-anchored fact | Transitions demand to organic peer sellers |
| **Phase 3 — Organic Market** | `hubSupplyShare < 20%` & `organicSellers >= 3` | **0 sats subsidy** (pure peer trades + Hub resale & BRC-105 relay fees) | Network is officially tipped |

---

## 3. Instrumentation & Mechanical Activation Bars (`GET /v1/coldstart/health`)

Every node and Hub exposes per-domain density (`live` | `forming` | `dead`) and cohort metrics at `GET /v1/coldstart/health` and Prometheus `GET /v1/metrics`:

| Metric | Source | Mechanical Bar |
|---|---|---|
| **Time to First Verified Receipt (`ttfvMs`)** | `nektar_ttfv_ms` / `nektar up` probe | **< 30 s** from install |
| **Fill Rate** | `nektar_liquidity_fill_rate` | **>= 80%** sustained |
| **Zero Rate** | `nektar_liquidity_zero_rate` | **< 10%** and falling |
| **Median Time-to-First-Offer** | Broker deal trail | **< 60 s** |
| **Organic Sellers per Domain** | Verified non-hub `dir` peers with `sellDomains` | **>= 3** per domain |
| **Hub Supply Share (Flintstoning Exit)** | `nektar_liquidity_hub_supply_share` | **< 20%** |
| **Viral Artifact Views** | `nektar_verify_views_total` | Every `/v1/verify/{token}` page links to 25s `--up` install |

---

## 4. Repeatable Launch Kit & Cohort Sequencing

Do not expand demand promotion to Cohort #N+1 until Cohort #N holds its bars without manual supply:

1. **Cohort #1 — Claude Code (`docs/MANUAL-CLAUDE-CODE.md`):** `install.sh --up` → full L3 (MCP + `SessionStart`/`UserPromptSubmit`/`Stop` hooks + `SKILL.md`) → verify live `nektarr1.*` receipt → seed `ops`/`bsv`/`agents` bounties.
2. **Cohort #2 — Codex CLI (`docs/MANUAL-CODEX.md`):** Adjacent CLI coding-agent graph. Same 1-liner wires `~/.codex/config.toml` (`[mcp_servers.nektar]` + top-level `notify`) and `~/.codex/skills/nektar-memory/SKILL.md`.
3. **Cohort #3 — Hermes Agent & OpenClaw (`docs/MANUAL-HERMES.md`):** Always-on autonomous agents (natural high-frequency buyers & sellers). Same 1-liner extracts the `//go:embed`'d native memory-slot plugins and merges `~/.hermes/config.yaml` / `~/.openclaw/openclaw.json`.
4. **Cohort #4 — Cursor / VS Code / Gemini CLI / L0 Proxy:** Broad IDE + custom-SDK wave once 3+ organic sellers sustain >80% fill rates.

---

## 5. Ceiling & Moat Defense

- **Anti-Spam & Quality at Scale:** BRC-105 hybrid Hub micro-fees (5s want / 2s offer / 2s bbox / 10s per 100 egress) price out spam; SPV proof requirements gate every bounty payout; AWS Cedar enforces shard isolation; Jev semantic gating filters low-relevance quotes.
- **Defensible Moat:** Anyone can copy an MCP adapter, but not the accumulated BSV-anchored provenance graph, signed seller settlement histories, and dense verified shard supply across federated Spine-and-Leaf Hubs.
- **Honest Custody Boundary:** Every docked agent owns its own local SQLite vault and BRC-100 keys (`~/.nektar/main` on Mainnet, `~/.nektar` on Testnet) — Hubs are untrusted relays and never hold user private memory.
