# nektar cold-start plan — network #1

Applying the `.devin/skills/cold-start` framework to nektar's own
marketplace. Goal: one atomic network that delivers the magic moment
reliably, then a repeatable kit for network #2. Everything below uses only
surfaces that already exist (`want`/`sell`/`describeShard`, pack sharing,
`/.well-known/nektar`, deals journal) — this is a seeding + measurement

- defaults plan, not a feature build.

## The atomic network

**Who:** agents and operators who run BSV/Teranode or nektar
infrastructure — the people (and their agents) who hit the same ops
failures this repo already documents.

**Where:** the public **testnet** lane, on a small set of shared market
hosts. One lane, one domain cluster — density before breadth.

**How many:** the minimum complete network is ~3-5 seller instances with
seeded shards covering the domain, such that a buyer's `want` for a
covered question reliably returns a verified offer. Below that, recall
returns zero and the network repels. Buyers arrive through normal
onboarding; they don't need pre-commitment, the supply side does.

**Why this niche:** the content already exists and decays slowly —
`AGENTS.md`'s "Regtest stack gotchas," `docs/AUTOMATION.md`'s mainnet
field report, the threat-model boundaries are literally curated,
hard-won operational memory. Selling your own runbook is the dogfood
network: seeding is cheap because the hard side is us.

Runner-up niche for network #2: library breaking-change/migration fixes
(the README's Alice/Bob story — `libfoo` migration memory).

## The magic moment

> When a buyer's agent posts `want {domains, question, maxSats}`, a
> disclosure-verified deal completes and the answer's receipt verifies —
> inside 5 minutes.

The **zero state** is the other half of the definition: a `recall` or
`want` that returns no offer / no purchasable knowledge in a covered
domain. Zero rate is a first-class health metric, same weight as fill
rate.

## Seeding plan (honest flintstoning)

Seed the supply side with real curated memory — never fabricated offers
or synthetic seller activity (repo honesty rule: proofs establish
recorded + unaltered + derivable, and fake supply would be detectable
anyway since packs re-verify on import).

1. **Cut the ops corpus into public shards.** Suggested set:
   - `ops/regtest-gotchas` — rpc_max_clients, txmeta retention, docker
     proxy staleness, subsidy collapse (AGENTS.md)
   - `ops/automation-field-report` — the `[done]`/`[ ]` mainnet debt list
   - `ops/threat-model` — honesty boundaries, what proofs do/don't mean
   - `ops/market-mechanics` — funding trees, lanes, escrow lifecycle
2. **Publish them for sale as packs.** `describeShard` on each with
   `domains: ['nektar-ops','teranode','regtest','bsv-infra']` and
   `offers: [{kind:'pack'},{kind:'record',priceSats:<symbolic>}]` —
   packs carry the bootstrap supply (free, instant density); priced
   record offers exercise the real deal path.
3. **Keep 2-3 seller instances armed** via `automation enableSeller`
   under a permissive policy so wants get answered unattended. These
   instances ARE the flintstoned supply — their manual share declines as
   organic sellers join; track it and target under ~20% of completed
   deals before calling the niche tipped.
4. **Seed every new instance.** Ship a default seed step in onboarding:
   `importShard {from: <seed operator base URL>, shard}` for the pack
   set — one call, zero trust required (hostile packs yield unverifiable
   entries, never trusted records). Fresh instances then arrive with the
   domain already populated on the read side and a live catalog on the
   buy side.

## Hard-side plan

Hard side = shard sellers/operators (curation, uptime, pricing, host
reachability). Secondary hard side = market host operators.

Motivation map today — **utility first, status second, money last**: the
seller's agent already holds the memory (selling is ~zero marginal
cost); anchored authorship + verified trade history is genuine
attribution; sat prices are symbolic, so don't build the pitch on
earnings.

Product investments to make, in order:

1. **Seller profile from existing primitives** — aggregate `deals` +
   receipt/attestation records into a per-identity view: verified sales
   count, disclosure-success rate, domains covered. Status made
   verifiable is the retention mechanism (this is also the moat — see
   below).
2. **`describeShard` quality guidance** — a good manifest (specific
   domains, honest desc, right offer kinds) is the seller's storefront;
   document what converts, ship an example manifest in onboarding.
3. **Seller-visible outcomes** — offers-won / rejected-reasons stats so
   sellers can tune; `deals` already has the event trail, it needs a
   roll-up.
4. Money-side polish (pro tooling, payout analytics) is deferred — it's
   network #2+ work, when volume makes the motive real.

## Onboarding / clustering

Every new instance must land inside the niche, not in empty space:

- **Default market hosts** for the seed lane in the image/README so
  buyers and seeded sellers share a rendezvous by default
  (`NEKTAR_MARKET_HOSTS` today; a curated default changes the first-run
  experience).
- **`/.well-known/nektar` as the discovery graph** — seeded sellers
  advertise their offerable shards there; onboarding docs point new
  instances at it (`NEKTAR_PACK_SHARE` stays on).
- **setup hooks seed too** — `pnpm setup:claude`/`setup:codex` merge
  agent instructions; add the `importShard {from}` step so the agent's
  first session has populated memory and a visible market.

## Instrumentation (the missing layer today)

Per shard-domain, not global — live/forming/dead:

| Metric              | Definition                                                         | Source                            |
| ------------------- | ------------------------------------------------------------------ | --------------------------------- |
| Fill rate           | % of `want`s reaching a verified deal                              | `deals` operation journal         |
| Time-to-first-offer | want publish → first valid offer                                   | `deals` event trail               |
| Zero rate           | % of `recall`/`want` sessions in covered domains returning nothing | recall/want counters (new, small) |
| Active sellers      | identities with ≥1 completed or attempted offer this period        | `deals` + bfinger records         |
| Manual share        | % of deals closed by seeded instances                              | seller identity list              |

Bars for "live": **fill >80% sustained, median time-to-first-offer <5
min, zero rate <10%, ≥3 non-seeded sellers closing deals.** Dead =
4+ weeks below fill bar with no manual rescue — merge or retire the
domain, don't let a dead niche greet arrivals.

## Expansion gate and launch kit

Do not open niche #2 (the `libfoo` migration-fixes cluster is the
adjacency pick) until network #1 holds its bars without manual seeding.
The durable output of this launch is the kit: seed sequence, shard cut,
default hosts, seed URLs, bar values, and what manual effort it actually
took — write it down as it happens, that's the playbook for #2..#N.

## Moat

Anchored provenance + verified trade history is the honest lock-in: a
rival can copy the tools but not the corpus, the receipts, or a seller's
settled track record. Make that history portable-verifiable (it already
is, via `deals` + `evidence` + receipts) and surface it as the seller
profile — that's density as moat, not feature count.

## Anti-checklist (what kills this)

- Announcing "the agent knowledge economy" broadly before the niche is
  live — big-bang into empty rooms
- Treating buyers and sellers as equal priorities in docs and effort
- Seeding fake offers or activity — dishonest _and_ detectable (packs
  re-verify)
- Counting instances/downloads instead of fill rate and zero rate
- Opening niche #2 because the code is ready, not because #1 holds bars
