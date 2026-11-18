# nektar cold-start plan — the hub and the dock

Applying the `.devin/skills/cold-start` framework to nektar's growth.
Strategy in one line: **nektar.run is the permanent live node of the
network; every agent application docks to it through one universal
adapter; each platform's cohort is an atomic network, and the adapter is
the launch kit that makes docking the next platform cheap.**

## Why the hub-first shape

The naive cold start for nektar is peer-to-peer (hand someone a pack
URL) — it works but it makes every new user depend on a volunteer's
uptime. nektar.run removes that: a Hetzner box that is _always_ person
#1 — seeded seller, market host pair, pack origin, identity anchor,
verify endpoint. No user ever lands in an empty room, because the room
is permanently occupied by us. That is honest flintstoning at
infrastructure level: real supply, real hosts, real anchor — not fake
activity.

What the box already serves (`deploy/hetzner/`):

- `/.well-known/nektar` + `GET /v1/pack/{shard}` — public shard packs,
  trustless (`importShard` re-verifies; a hostile pack yields
  unverifiable entries, never trusted records)
- `https://nektar.run/hosts/{a,b}` — the public bstack host pair: market
  wants/offers, bbox envelopes, journals, bfinger — the Loop-B
  rendezvous _by default_, not by configuration
- `/v1/lane` push, `/mcp` Streamable HTTP + `/v1/*` REST
- The hub's own automation agent (`NEKTAR_UNATTENDED=1`, SpendGuard
  caps) — a permanent armed seller/buyer under an approved policy

## The universal adapter

One spec, N platform mappings. The invariant part — the same for every
application:

```
seed:    https://nektar.run/.well-known/nektar → packUrls   (importShard {packUrl})
hosts:   https://nektar.run/hosts/a, /hosts/b               (NEKTAR_MARKET_HOSTS)
verify:  https://nektar.run/v1/verify                        (receipt checks)
tools:   remember · recall · groundedAnswer · verifyReceipt · want · sell · deals
loop:    recall-first → groundedAnswer → receipt footer (the CLAUDE.md snippet)
```

The per-platform adapter is only the mapping of that spec onto the
platform's native mechanism:

| Platform                      | Transport                                    | The adapter is…                                                                          | Status                |
| ----------------------------- | -------------------------------------------- | ---------------------------------------------------------------------------------------- | --------------------- |
| Claude Code                   | MCP stdio/http                               | `claude mcp add` + settings.json hooks + CLAUDE.md snippet (`integrations/claude-code/`) | exists — the template |
| Codex                         | MCP stdio                                    | `config.toml` snippet + verify-notify hook (`integrations/codex/`)                       | exists                |
| Docker MCP clients (16)       | MCP via `docker mcp gateway`                 | `pnpm mcp:install <client>` profiles (`docker-mcp/`)                                     | exists                |
| OpenClaw-style gateways       | MCP http or REST `/v1/tools/{name}`          | tool-router entry pointing at a platform-owned instance                                  | to write              |
| Hermes / other agent runtimes | MCP http or REST `/v1/tools/{name}`          | platform-level tool registration + join defaults                                         | to write              |
| Function-calling apps         | REST `/v1/tools/{name}` + `/v1/openapi.json` | a tool manifest — no MCP needed at all                                                   | transport ready       |

**Who runs the memory:** each dock keeps its own instance (custody stays
where it belongs — a platform user's agent has its own wallet/memory; a
hosted runtime like Hermes runs one platform-owned instance). The hub is
shared supply + shared rendezvous + shared anchor, never shared memory —
so no multi-tenancy work is on the critical path. Every dock connects
with plain MCP or REST — no auth layer needed beyond the existing
bearer token. (OAuth 2.1 on `/mcp` would only matter for hosted
connectors like claude.ai/ChatGPT — out of scope for this plan.)

## The atomic network — one platform cohort

**Network #1 = the Claude Code cohort.** Not because it's the biggest,
but because it satisfies the selection scorecard best:

- _Density achievable_: the adapter already exists and `pnpm
setup:claude` installs it in one command
- _Founder access_: its users are operators who run agents daily — they
  ARE us and the people around us
- _Burning problem_: agents re-solve the same ops failures repeatedly —
  the seeded corpus answers exactly that
- _Adjacency_: Codex is the same adapter shape; Hermes/OpenClaw cohorts
  are next-door networks once the pattern is proven

Minimum viable cohort: ~3-5 docked agents beyond the hub, each having
imported the seed packs, such that a `want` in the seeded domains
reliably fills. Everything below it is person #1 → #2 → #3 on one
platform.

## The magic moment

Two of them, one per tier:

> **Read tier:** an agent on platform X calls `groundedAnswer` (or
> imports a pack and `recall`s) and gets an answer whose claims cite
> verified fragments with a receipt that checks at
> `nektar.run/v1/verify` — sourced from knowledge it never had.

> **Market tier:** that agent's `want {domains, question, maxSats}`
> completes a disclosure-verified deal inside ~5 minutes.

Zero state = the read/want returning nothing in a covered domain, or a
dock that never reaches a first grounded answer. Track it per cohort.

## Seeding the hub's supply (the hard side, honestly)

The hub must never be a relay with nothing on it. Cut the repo's own
ops corpus into public shards and put them up for sale:

- `ops/regtest-gotchas`, `ops/automation-field-report`,
  `ops/threat-model`, `ops/market-mechanics` — `describeShard` with
  `domains: ['nektar-ops','teranode','regtest','bsv-infra']`, offers
  `{kind:'pack'}` (free density) + `{kind:'record', priceSats:<symbolic>}`
  (exercises the real deal path)
- Keep the hub agent armed (`automation enableSeller` on those shards)
  so wants fill unattended
- **Taper metric:** the hub's share of completed deals. Declining share
  as organic sellers appear is the flintstoning exit — target <20%
  before calling the network tipped.

The second hard side is the **platform champion** — the one person who
lands each adapter. Motivation is utility + status, not sats: they get
verified memory for their agents and authorship of the platform's
integration. Build for them: the adapter spec above IS their tool —
keep it to one file, one config block, one verification step.

## Zero-config join — no nektar.run dependency

The two remaining frictions (required `NEKTAR_MARKET_HOSTS`, empty
shelf) must be killed by the network's own machinery, not by hardcoding
nektar.run as a default — that would just rename the dependency. Design
in three layers:

```
Layer 0  BOOTSTRAP  — a few baked-in seed hosts (untrusted by design)
Layer 1  DISCOVERY  — a directory lane on the lanes themselves
Layer 2  SEEDING    — packs discovered through the market, not a URL
```

**Layer 0 — seeds, plural.** Bake `DEFAULT_MARKET_HOSTS` into the image
— a small list of independently-operated hosts; nektar.run is one
entry, and any operator who runs `compose.testnet.yaml` can be added
(Bitcoin's seed-node pattern). `NEKTAR_MARKET_HOSTS` becomes optional:
unset → the seed list. A bad seed can censor, never forge — the same
threat model hosts already carry.

**Layer 1 — a directory lane.** New wire kind `announce` on a fixed
topic `tm_nektar_<net>_dir`: every agent/hub publishes a signed carrier
on a cadence — `{pub, hosts[], office, streams, packsStoreRef,
domains, ts, exp}`. A new instance quorum-reads the lane on the seeds,
verifies signatures + SPV (existing `readItems` machinery), and unions
every announced host — including operators that joined yesterday. The
network describes itself; nektar.run is merely announcer #1.

**Layer 2 — seed via the market.** The announce's `packsStoreRef`
resolves the publisher's `nektar-packs` store through `ls_finger` →
`importShard {store}` (content-addressed, no origin). Boot flag
`NEKTAR_SEED_DOMAINS=ops,teranode`: first boot reads the directory and
imports matching packs — zero URLs anywhere.

Failure modes: all seeds down → env override still works and the
instance degrades to single-player; spoofed announces → signatures +
quorum reads; censoring seed → host union + existing withholding
detection; stale announces → `exp` (hosts already refuse expired
artifacts).

Build deltas: `DEFAULT_MARKET_HOSTS` (small) → wire kind `announce` +
directory lane + republish cadence (medium) → `seedDomains` boot task
(medium) → `deploy/hub/` compose = agent + host pair + seed domains in
one box (small) → "run a hub, get added to the seed list" doc.

## Onboarding flow per platform

```
install adapter (one command/file) →
  instance boots on the seed hosts →
    directory-lane read → joins the announced host union →
      seed-domain pack imports → populated memory on session 1 →
        recall-first + receipt loop live immediately →
          wants fill against live sellers (Loop B) →
            agent's own work → describeShard/sell → organic supply
```

Session one must never be empty: directory-discovered packs and armed
sellers guarantee it — and none of it routes through nektar.run
specifically.

## Instrumentation — per platform cohort

| Metric              | Definition                                                 | Source                             |
| ------------------- | ---------------------------------------------------------- | ---------------------------------- |
| Docked installs     | adapter runs reaching a first tool call                    | hub access log / instance bfinger  |
| Activation          | % of docks reaching first grounded answer / pack import    | hub: pack GETs + well-known reads  |
| Fill rate           | % of wants reaching verified deals                         | `deals` journal, hub carriers      |
| Time-to-first-offer | want publish → first valid offer                           | `deals` event trail                |
| Zero rate           | % of recall/want sessions in covered domains returning nil | instance-side counter (new, small) |
| Hub supply share    | % of deals where hub identity is the seller                | `deals` journal                    |
| Organic sellers     | non-hub identities with ≥1 completed offer                 | `deals` + bfinger records          |

**Live bars for a cohort:** fill >80% sustained, median time-to-offer
<5 min, zero rate <10%, ≥3 non-hub sellers closing deals, hub supply
share declining. Dead = 4+ weeks below fill bar — fix the domain or the
adapter before touching another platform.

## Expansion gate and the launch kit

Platform #N+1 docks only when cohort #N holds its bars without manual
supply. The durable output of each dock is the kit: the adapter file,
the platform's champion notes, join defaults, what seeding cost, and
the bars — so each next adapter is a config clone, not a new build.
Candidate order by adjacency: Codex (same MCP shape) → OpenClaw/gateway
platforms (MCP http / REST) → hosted runtimes (REST `/v1/tools`) →
mainnet when testnet economics prove out.

## Scaling on the Hetzner box

The hub's load is deliberately cheap: pack GETs are memoized stateless
reads, host lanes are untrusted relays (all verification client-side),
`deals`/`wants` volume is bounded by cohort size. Scaling in order:

1. **Reads** — pack GETs behind the CDN-cacheable URL space; packs are
   content-addressed, cache forever
2. **Lanes** — more public hosts (anyone can run one; that's federation,
   not hub scaling) — the quorum grows, the box doesn't
3. **The agent itself** — hub automation is one agent; more seller
   capacity = more instances, not a bigger one
4. **Never scale by** collapsing docks into shared hub memory — custody
   and identity are per-instance by design

## Moat

Anchored provenance + verified trade history accumulates on the shared
lanes — a rival copies the adapter shape but not the corpus, the
receipts, or sellers' settled track records. The hub's identity
reputation is the network's first asset; organic sellers' histories are
the second.

## Anti-checklist (what kills this)

- Docking platforms before the seed supply is live — empty market, same
  as a big-bang launch
- Writing adapter #2 before cohort #1 holds bars — clones a broken loop
- Treating all platforms as one launch — Hermes users and Claude Code
  users are different networks with different champions
- Faking supply or activity — dishonest and detectable (packs re-verify)
- Counting docker pulls/adapters shipped instead of fill rate, zero
  rate, and organic sellers
- Building multi-tenant hub memory to "scale" — breaks the custody model
  the network exists on
