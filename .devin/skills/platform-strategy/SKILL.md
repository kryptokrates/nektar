---
name: platform-strategy
description: >
  Design, price, govern, position and evaluate multisided platforms and
  marketplaces — including open-source products run as platforms. Use when
  the user asks about "platform design", "marketplace design", "which side
  to subsidize", "pricing a platform", "openness vs control", "platform
  governance", "will this marketplace work", "liquidity metrics",
  "subsidy side", "money side", "who should pay", "market position",
  "category design", "frame of reference", "new category vs subsegment",
  "ignition", "critical mass", "ecosystem risk", "open source strategy",
  "open core", "open source business model", "license choice",
  "cloud provider hosting our code", "community strategy", "contributor
  funnel", "should we open source it", "fork risk", or asks whether a
  platform/marketplace idea can succeed. Covers pipeline vs platform, the
  core interaction, the 8 launch strategies, viral mechanics,
  monetization (the 4 forms of value + access/usage levers), openness
  decisions, governance (laws/norms/architecture + market failures),
  liquidity metrics, the sizzle/fizzle checklist, positioning frames, and
  the open-source sponsor-capture playbook. Grounded in Platform
  Revolution (Parker, Van Alstyne, Choudary), Matchmakers (Evans &
  Schmalensee), and Obviously Awesome (Dunford); deep case detail in
  references/. For launch-sequencing and hard-side detail, use
  cold-start; for user-journey friction audits, use friction.
---

# platform-strategy — design, price, govern & position multisided platforms

Advisory for businesses that connect two or more groups whose interaction
creates the value — marketplaces, matchmakers, payment networks, developer
ecosystems, app stores, sharing platforms. Three books, three lenses:
**Platform Revolution** teaches the _design_ (interaction, openness,
governance, metrics, monetization), **Matchmakers** teaches the
_economics_ (friction, ignition, price balancing, the bet checklist), and
**Obviously Awesome** teaches the _positioning_ (frame of reference that
makes value obvious). The books converge: a platform is not a product
with users — it is an engine for interactions, and every design decision
is judged by whether it increases valuable interactions per participant.

## Advisory process

1. **Scope the ask.** Which lens is on the table?
   - _Should this platform exist / will it work?_ → section 8 (the
     sizzle/fizzle checklist) — score before anything else.
   - _Design of a platform being built_ → sections 1–2, 5–6 (interaction,
     openness, governance).
   - _Pricing / monetization_ → section 4 (forms of value, subsidy vs
     money side).
   - _Launch plan_ → section 3 + the `cold-start` skill for the
     network-sequencing detail.
   - _Growth diagnosis / metrics_ → section 7 (liquidity first, always).
   - _Naming/positioning/"what do we call this"_ → section 9 (Dunford).
   - _Stalled/competitive pressure_ → section 10 (envelopment,
     multihoming defense, winner-take-all).
   - _Open-source strategy — should we open, what stays closed, open-core
     pricing, license/fork/community_ → section 11 first, then 4–6.
2. **Score it.** Rate against the rubric. A plan that cannot answer "who
   is the subsidy side" or "what is the core interaction" scores ≤5 —
   an OSS plan that can't answer "what do we capture if the code is
   free" scores ≤5 too.
3. **Advise.** Report the score, then ranked changes to reach 10/10 —
   concrete (named sides, named subsidies, named metrics), never "grow
   the ecosystem."
4. **Hold the ethical boundaries.** Real scarcity, disclosed subsidies,
   governance applied to the platform owner too — violations are defects.

## Scoring

**Goal: 10/10.**

- **9-10:** Named core interaction (participants + value unit + filter);
  explicit subsidy/money sides with a rationale; ignition plan with
  self-supply or zigzag; openness decisions made deliberately per group;
  governance owns fakes/fraud/quality; liquidity metrics instrumented;
  positioning frame chosen with named competitive alternative. For an
  open-source venture add: capture surface named and separate from the
  code (host/network/registry), codebase modular enough for outsiders,
  license/fork posture decided as a monetization choice
- **7-8:** Sound on interaction + pricing, but openness or governance
  is assumed rather than decided, or metrics still track totals
- **5-6:** Platform language without platform mechanics — sides named
  but no subsidy logic, no liquidity metric, positioning by adjective
- **3-4:** Single-sided product wearing platform clothes; "network
  effects" invoked but no interaction defined
- **0-2:** Launch-to-everyone, charge everyone, govern no one

## Framework

### 1. Pipeline vs Platform — the core interaction

**Core concept (Revolution ch. 1–3):** A pipeline creates value in a
linear sequence and pushes it to a consumer; a platform orchestrates
interactions between participants and captures a cut of the value
_exchanged_, not produced. The test is not "does it have users on both
sides" — plenty of pipeline products do — but "would the product collapse
if one side vanished" AND "is its unit of value the interaction itself."

Every platform runs one primitive — the **core interaction**: three
parts that must be designed together:

- **Participants** — the value creator and the value consumer in the
  exchange. One agent can play both roles on different interactions
  (Etsy sellers are also Etsy buyers — map roles, not people).
- **Value unit** — the thing exchanged that the consumer values: a
  listing, a video, a ride, an answer, a verified fact. Platforms that
  fail usually have a value-unit problem, not a growth problem — the
  unit isn't actually worth exchanging.
- **Filter** — the mechanism that delivers the right unit to the right
  consumer (search, ranking, matching, feed, pricing). The filter is
  where quality lives; a platform with bad matching is a noisy room,
  not a marketplace.

**Why it works:** Designing the interaction first forces every later
decision — pricing, openness, governance — to answer "does this
increase valuable interactions or decrease them." Skipping it produces
feature lists dressed as strategy.

**Key insights:**

- More participants ≠ more value: value lives in _completed_ core
  interactions. Count those, not signups.
- Network effects come in four flavors: same-side positive (more
  sellers → better sellers), same-side negative (more sellers →
  cannibalization/spam), cross-side positive (more sellers → better
  for buyers), cross-side negative (more sellers → harder to find
  anything). Design against the negatives, not just for the positives.
- The filter is the moat-in-progress: better matching at equal supply
  wins every time.
- Pipelines and platforms coexist deliberately — most real businesses
  are pipeline cores with platform appendages (Amazon is both). Decide
  which is which per function; ambiguity leaks value.

**Applications:**

| Context               | Application                                                                                                 | Example                                                       |
| --------------------- | ----------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------- |
| New platform spec     | Write the core interaction in one sentence: "[creator] exchanges [value unit] with [consumer] via [filter]" | "Hosts exchange stays with travelers via search+reviews"      |
| Weak growth diagnosis | Check the value unit before blaming acquisition — do completed interactions happen for users who DO match?  | A dating app with signups but no messages has a unit problem  |
| Feature review        | Score every feature on interactions-enabled-per-participant                                                 | A new seller tool that lifts listing quality lifts the filter |

### 2. Friction is the business — matchmakers create value by removing it

**Core concept (Matchmakers ch. 4):** A multisided platform earns its
existence by finding and destroying transaction friction — search costs,
trust costs, coordination costs, settlement costs — that its sides could
not overcome alone. The platform's product is not the app; it is the
_absence of the friction_. A new platform is viable only when the
friction it removes is large enough that participants will route through
the platform rather than around it.

**Why it works:** Friction size predicts platform size. Platforms that
solve small frictions get disintermediated the moment users meet once —
they route around the middleman. Platforms that solve large frictions
(being paid, being trusted, being found) keep the transaction inside.

**Key insights:**

- Rate the friction on three axes: how big, how frequent, who
  benefits when it's gone. All three must be real — friction that is
  huge but rare, or common but trivial, doesn't sustain a platform.
- The strongest platforms attack _trust_ friction: making an
  interaction safe that would otherwise be unsafe (escrow, identity,
  insurance, reputation). Trust friction is the hardest for
  participants to self-solve and the hardest for rivals to copy.
- Beware disintermediation: once matched, can the sides deal directly
  and cut you out? If yes, your design must keep the _settlement or
  trust_ leg on-platform (eBay's escrow, Airbnb's insurance) or accept
  being a lead generator.
- Creating a market that didn't exist is a different game than fixing a
  broken one — the first needs evangelism (prove the category
  deserves to exist), the second needs superiority (prove you beat the
  incumbent's flow).

**Applications:**

| Context             | Application                                                                    | Example                                                                                        |
| ------------------- | ------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------- |
| Idea screening      | Name the friction + who pays it today; if nobody pays it, there is no platform | "Agents can't hold money or prove memory" is a real friction; "users want another feed" is not |
| Feature triage      | Build trust-friction features before convenience features                      | Escrow before notifications                                                                    |
| Retention diagnosis | Users match once then leave? The platform solved the find but not the settle   | Add dispute/escrow/quality layers                                                              |

### 3. Launch — the eight strategies

**Core concept (Revolution ch. 5):** Every platform launch must solve
the chicken-or-egg problem: producers won't join without consumers,
consumers without producers. Eight named strategies, combinable:

1. **Follow-the-rabbit** — platformize a proven pipeline business
   (Amazon's marketplace on its own retail infrastructure).
2. **Piggyback** — graft onto an existing user base on a different
   platform (apps inside Facebook, PayPal on eBay, a plugin inside a
   runtime that already has your users).
3. **Seeding** — supply one side yourself with real content/inventory
   until real participants take over (Quora staff answers, YouTube's
   founder videos).
4. **Marquee** — pay or recruit the key participants whose presence
   makes everyone else come (anchor tenants, star sellers, known
   publishers).
5. **Single-side** — launch as a single-user tool that delivers value
   alone, then add the second side once the user base exists
   (OpenTable's reservation book before its diner network).
6. **Producer evangelism** — recruit producers whose output recruits
   consumers (Kickstarter: creators bring their own backers; the
   platform doesn't find your audience, you bring it).
7. **Big-bang** — spend your way to instant scale. Works only with
   enormous resources AND a product that retains; otherwise it fills
   empty rooms fast (the Google+ anti-pattern).
8. **Micromarket** — launch in a tiny market where members already
   interact, so density is achievable with founder-level effort
   (Tinder's campuses, Facebook's Harvard).

**Matchmakers adds the tactics layer:** **self-supply** (be your own
first producer), **zigzag** (push each side in alternating bursts, not
in lockstep — YouTube alternated creator tools and viewer reach),
**shaping expectations / "make 'em believe"** (sell the credible future
— Levchin's track record, marquee commitments, published roadmaps),
**exclusive supply** (content that exists only through you), and
**subsidization** (pay the scarce side to exist early — PayPal's $10).
Two refinements worth keeping: **go narrow/deep to manufacture
thickness** — OpenTable abandoned 50 thin cities for 4 deep ones; a
Seattle restaurant is worthless to an SF diner — and **critical mass is
per-market, not global** (eBay China withdrew: scale elsewhere doesn't
transfer). Matchmakers' three ignition strategies — zigzag, two-step
(secure one side, sell access to it), commitment (guarantee the other
side will exist before a side invests — Android's free license was a
commitment device for OEMs).

**Revolution adds the viral mechanics** — the four-element loop behind
organic spread (ch.5): **sender** (a user spreading _their own_ value
unit, not word-of-mouth about you) → **spreadable value unit** (the
artifact must start an external interaction or complete an incomplete
one — an unanswered question, a survey, a shared photo; confidential or
inert artifacts can't travel) → **external network** (the existing
medium it travels — Facebook for Instagram, eBay for PayPal; the host
can later throttle you, so the loop must also feed your own graph) →
**recipient** (converts if relevant, then becomes the next sender).
Pull beats push: design the spread into the interaction itself —
Instagram hit 100M users with zero marketing staff because _every use
was an ad_. Inorganic spread incentives must not be cash (Dropbox gave
storage, not dollars — cash drains at viral scale). And virality ≠
network effects: virality _acquires_, network effects _retain_ — you
need both, in that order.

**Applications:**

| Context        | Application                                                                             | Example                                                            |
| -------------- | --------------------------------------------------------------------------------------- | ------------------------------------------------------------------ |
| Pre-launch     | Pick ONE primary strategy + at most one supporting; name the self-supply inventory      | "Single-side tool + seeding: ship the tool, run 3 house producers" |
| Ignition stall | Add zigzag — stop pushing both sides simultaneously; alternate bursts per side per week | One week seller tooling, next week buyer acquisition               |
| Distribution   | Prefer piggyback over advertising when a host platform already concentrates your users  | A skill/plugin in the runtime your users already run               |

**Ethical boundary:** Seeding means real work done manually — never
fake users, fake activity, or fake scarcity. Users discover deception;
trust doesn't recover.

### 4. Monetization — whom to charge is the design decision

**Core concept (Revolution ch. 6):** Platforms monetize four forms of
access — and each form punishes the wrong charge:

- **Access to value creation** — charging producers to participate
  discourages supply. Charge here only when participation itself is
  valuable to them (hosting fees where they earn).
- **Access to the market** — a transaction cut. The most aligned model:
  the platform earns only when an interaction completes. Charge the
  side that captures the payment.
- **Access to tools** — enhanced capabilities, analytics, premium
  workflows for the professional side.
- **Curation** — enhanced visibility/matching for producers, enhanced
  filtering for consumers.

**The whom-to-charge rules:**

- **Charge the side that benefits most from the other side's presence**
  — sellers pay because buyers exist; job boards charge recruiters
  because candidates exist.
- **Subsidize the price-sensitive side** — the side with more
  alternatives or less at stake must be cheap or free; they're the
  audience you sell.
- **Charge full freight to the least price-sensitive** — enterprises,
  professionals, whoever monetizes through the platform.
- **Matchmakers' structure:** the subsidy side may pay _negative_
  price (you pay them — >100% of the money side's fee can fund the
  subsidy). Price the _pair_, not each side independently: total price
  = what makes the network ignite and persist, allocated to the side
  that can bear it. Two decisions, not one — the **level**
  (P_A + P_B) and the **structure** (each side's share; usually tilted,
  e.g. WEX fleet cards' $1.50/$0.50 split).
- **Never charge for what makes the network valuable.** Charging for
  consumption shrinks the audience producers pay to reach; charging
  for production shrinks what consumers come for.
- **The side that controls whether the interaction happens gets priced
  lowest** — cardholders decide the swipe (AmEx pays rewards), searchers
  choose the engine (free), the fleet decides which card gets used.
- **Access fees and usage fees are separate levers** — access monetizes
  the option (membership, license); usage monetizes the interaction
  (per-deal %). Pair high-access/zero-usage to maximize the activity
  the other side pays for; never charge access to a side whose mere
  presence creates positive indirect effects (merchants join AmEx free).
- **Charge at deal completion, not at entry or listing** — entry fees
  shrink supply _and_ the matching data you learn from; completion fees
  skim a transaction already valued. (Alibaba couldn't track deals →
  membership + ads; Ariba lived on one-sided software until it could
  charge both sides ~2005.)
- **Pricing is curation** — Meetup's $19 organizer fee killed 95% of
  activity but lifted successful meetups from ~2% to ~50%. Ask whether a
  price can strengthen positive network effects _and_ weed negative
  ones.
- **Free→fee transitions have rules** — never charge for what was free;
  never reduce accustomed access; create _new_ value that justifies the
  charge and control its quality. Architect for monetization
  optionality from day one: transaction fees require transaction
  capture, access fees require control of the flows. Openness choices
  _are_ monetization choices — see §11.

**Applications:**

| Context                 | Application                                                                | Example                                                                                           |
| ----------------------- | -------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------- |
| Pricing design          | Name subsidy side + money side + the taper on any early subsidy            | "Buyers free forever; sellers pay 3% of settlements; house demand tapers on a published schedule" |
| New revenue idea        | Check which form of access it taxes; reject any that taxes the scarce side | Charging consumers to browse = taxing demand                                                      |
| Free-to-paid transition | Publish the taper before collecting the subsidy                            | "Zero fees until month 6, then 2%+, schedule in the docs"                                         |

**Ethical boundary:** Subsidies are temporary incentives — publish the
taper before the subsidy, and never build livelihoods on terms you plan
to quietly degrade.

### 5. Openness — a decision per group, not a value

**Core concept (Revolution ch. 7):** "Open vs closed" isn't a binary —
it's four independent decisions, each with a different lever:

- **Manager + sponsor participation** — who runs the platform and who
  owns its tech. VHS won the standard and JVC captured little — the
  sponsor layer must retain the value-accrual surface.
- **Developer participation** — who can build on top. Open extends
  innovation (more value units, more reach); closed preserves quality
  and captures more value per interaction. Most successful platforms
  are open at the standard/protocol layer and closed at the curated
  surface.
- **Provider participation** — who can offer value units. More
  providers = more units = more consumer value, until quality control
  breaks (spam, fraud). Gate by curation mechanisms, not raw exclusion.
- **User participation** — who can consume. Fully open maximizes the
  audience producers pay to reach; gated (invite, credential)
  maximizes trust/curation at the cost of density.

**The decision rule:** open where openness multiplies value units or
reaches more consumers; close where it protects the interaction's
quality or the sponsor's capture surface. Revisit as the network
matures — the early answer and the scaled answer differ (more open
early for density, more curated later for quality).

### 6. Governance — the platform is the government

**Core concept (Revolution ch. 8; Matchmakers ch. 9):** Bad actors and
low-quality supply kill platforms — Craigslist scams, fake listings,
spam producers. Governance runs on three tools:

- **Laws** — explicit rules with stated consequences (what's allowed,
  what's banned, how violations are judged). Cheap to write, expensive
  to enforce.
- **Norms** — reputation systems, community standards, status mechanics
  that make good behavior visible and bad behavior costly. The
  strongest enforcement because it scales with participants.
- **Architecture** — the platform's mechanics making bad behavior
  impossible or unprofitable (escrow prevents non-delivery; verified
  claims prevent fake listings; caps prevent spam). Enforcement that
  requires no judgment scales best.

**What governance actually fights — the four market failures, each
with its counter:**

- **Information asymmetry** (fakes, lemons — counterfeits were a $350B
  market, larger than the drug trade) → transparency, reputation,
  quality signals, insurance.
- **Externalities** (spam, contact-selling, one participant's behavior
  taxing the rest) → behavioral rules, architecture that makes the
  externality unprofitable, rewarding public-good creation.
- **Monopoly power of a participant** (Zynga on Facebook, eBay power
  sellers) → promote competitors; a single dominant participant can
  hold the platform hostage.
- **Risk** (participants eat the downside of bad interactions) →
  insurance and risk-pooling. The recurring arc: platforms instinctively
  shed participant risk early and adopt pooling later — Airbnb's $1M
  host guarantee, card networks' fraud caps. **Pool risk for
  participants; don't just minimize your own.**

Community norms scale on **Ostrom's commons rules**: defined
boundaries, channels for participants to influence decisions,
accountable monitors, graduated sanctions, cheap dispute resolution,
nested governance tiers as scale grows. Two enforcement subtleties
from the book: give _fast, open feedback for good behavior_ but _slow,
opaque feedback for bad_ (visible punishment teaches offenders to
dodge it; trolls whose posts show only to themselves stop trolling) —
and ejection is the ultimate sanction that must exist (OpenTable
terminates at 4 no-shows/year; Facebook ran ~150 User Ops staff on
real-name/conduct enforcement).

**Matchmakers' warning:** fakes and fraudsters arrive with growth, not
after it — the governance stack must be designed before the network is
dense, not retrofitted after the scandal. And governance applies to
the platform owner too: rules the owner ignores destroy legitimacy
faster than rules never written.

### 7. Metrics — liquidity first, always

**Core concept (Revolution ch. 9):** Pipeline metrics (signups, MAU,
revenue totals) hide empty platforms. Platform health is measured by
the _interaction_:

- **Liquidity** — the probability an intent to interact gets satisfied
  within reasonable time. THE early metric: % of listings purchased, %
  of posts answered, % of wants receiving a qualifying offer, time-to-
  match. A platform can have ten thousand users and zero liquidity.
- **Interaction quality** — satisfaction per completed interaction:
  repeat-interaction rate, rating distributions, dispute rates, NPS
  per side.
- **Trust** — the cost of bad interactions: fraud rate, scam incidence,
  churn attributed to bad actors, proportion of interactions needing
  intervention.
- **Balance** — per-side health separately: producer active rate,
  consumer return rate, cross-side ratio among _active_ users (OkCupid
  throttles overrepresented male visibility; Upwork balances
  freelancers to postings), and **side-switching rate** — the share of
  consumers who become producers (Airbnb's best host source is its
  guests; a rising switch rate is the cheapest supply channel there is).
- **Interaction failure %** — initiated interactions that fall
  through: the most-overlooked metric. Retained users + falling
  interaction-success rate = a serious problem that signup charts
  will never show.

**Lifecycle staging:** early = liquidity metrics only (% of intents
getting a successful interaction inside a window — track the illiquid
cases too, the search that returns nothing). Growth = per-network
density metrics (weekly active networks, fill rate, time-to-match per
market) + balance ratios + failure %. Scale = per-side retention,
take-rate sustainability, LTV of repeat producers. Match every metric
to its lifecycle phase or you're optimizing the wrong thing.

**Metric design rules:** prefer ratios over absolutes (active/total,
interaction-success rate); keep the dashboard small — find the one or
two metrics that prove participants love and use the platform; every
metric must pass the **3-A test**: _actionable_ (guides a decision),
_accessible_ (the people who gather it understand it), _auditable_
(real, precisely defined data). Vanity metrics lie in a specific
direction — totals keep rising while interactions flatten.

### 8. The sizzle/fizzle checklist — will this platform work?

**Core concept (Matchmakers ch. 10):** Six questions that separate the
bets that ignite from the ones that fizzle. Answer all six before
committing:

1. **What's the friction, how big is it, and who benefits from solving
   it?** — must be large, named, and felt by someone who can act on it.
2. **Does the platform design actually reduce this friction, balance
   every side's interests, and do it better than alternatives
   (including doing nothing)?** — the status quo is a competitor.
3. **How hard is ignition, and is there a concrete plan for critical
   mass?** — self-supply/zigzag/subsidy named, not "users will come."
4. **Do the prices needed for ignition and growth still allow the
   platform to make money?** — the price structure must be profitable
   at density, not just launchable.
5. **How does the matchmaker work within the broader ecosystem, and
   what related risks does it face?** — dependence on infra, partners,
   or platforms you don't control is the most common silent killer;
   name the degradation strategy, not just the dependency.
6. **Is the founder ready to change design AND ignition plan quickly
   when the market answers differently than expected?** — rigidity
   fizzles more often than bad ideas.

Score the venture against all six — one unanswered question is the
fizzle path.

**The countdown and the warning signs:** launch starts the clock —
most platforms ignite within ~2 years or never (OpenTable needed ~3
plus a wholesale strategy change). Below critical mass, the "dud"
reputation compounds: users stop coming, investors thin out. The
warning signs, in order: can't land even forgiving early adopters →
marquee players won't sign (the tell that matters most) →
participant/activity growth stops accelerating → early adopters
defect → no takeoff inflection after a year or two → implosion or
capital cutoff. Each sign has a different fix — early-adopter
rejection is a design problem; marquee refusal is an expectations
problem; stalled acceleration is a density problem.

### 9. Positioning — the frame that makes value obvious

**Core concept (Dunford):** Positioning is context — the frame of
reference a customer uses to judge you. Get it wrong and your strengths
are invisible; get it right and they explain themselves. The 10 steps
compress to the decisions that matter:

- **Start from best-fit customers, not the product** — survey only the
  ecstatic minority (bought fast, refer you, act as references);
  polling everyone yields mud. No happy customers yet? Keep it loose —
  the fishing-net principle: position broadly, watch what you catch,
  tighten when a pattern emerges.
- **Competitive alternative** — ask best-fit customers what they'd do
  if you didn't exist. The honest answers are rarely rival products:
  they're "hire an intern", "script it ourselves", "the OSS thing plus
  glue", "do nothing". Rank those, not the competitors _you_ track.
- **Unique attributes** — what you have that the alternatives don't,
  run through the **provability filter**: "easy to use" and "great
  service" are opinions, not attributes — keep only what a third party,
  a standard, or data can prove. Position on **consideration
  attributes** (what wins the evaluation), not retention attributes
  (what drives renewal — prospects never stay long enough to meet
  them).
- **Value** — what those attributes _enable_ for the customer; force
  the feature→benefit→value chain ("so what?") until the output is a
  customer goal, then cluster into 1–4 value themes.
- **Target segment** — who cares _a lot_ about that value, described by
  listable characteristics (you could build a named prospect list —
  "small business" fails that test).
- **Market frame** — pick the category deliberately; three options:
  - **Head-to-head** — win the existing market outright. Requires
    beating the incumbent's network, not just features. Usually loses —
    use only when you _are_ the leader or the category has none.
  - **Big fish, small pond** — dominate a well-defined subsegment of an
    existing market whose requirements the leader doesn't serve.
    Density first; expansion later. Beware the _fake subsegment_ — a
    demographic is not an unmet need (Bic for Her).
  - **New game** — create and name a new category, prove it deserves
    to exist, define its parameters, default yourself leader. Highest
    cost (evangelism burden — you teach the problem before you sell
    the fix), highest ceiling; needs a real "why now" shift and
    pockets deep enough to survive the fast-follower who steals the
    category once it clicks.
- **(+1) Trends** — layer a trend on top of the position for relevance;
  never make the trend the position. Trend answers "why now", category
  answers "what is it" — a pink couch is still a couch.

**The tell:** lead with what you uniquely prove, not what you
generally do. "Fast marketplace" is an adjective; "the market where
every deal settles with proof" is a position.

**Reposition triggers:** ~every 6 months or on a discontinuity — a
credible competitor enters (via acquisition especially), a regulation
or tech shift changes what's possible, buyer taste rotates. The
diagnostic: ask prospects _and_ happy customers separately what you do
— a disconnect is a positioning problem. Restart at the alternatives
step, not at messaging.

### 10. Winning and losing — winner-take-all and envelopment

**Core concept (Revolution ch. 10):** A market tips to one platform
when **all four** forces align: supply economies of scale, strong
network effects, high multihoming/switching costs, and no viable niche
specialization. If any force fails, multiple platforms coexist — and
the strategy isn't "win the market" but "own the densest niche." Defend
the hard side of your densest segments: challengers cherry-pick
exactly those, and bundling buys distribution but never devotion.

Competition runs on three levels simultaneously — platform vs.
platform (the better device lost to the better ecosystem: iPhone vs.
PSP), platform vs. partner (you compete with your own extension
developers — "delicate and dangerous"), partner vs. partner.

**Envelopment** is the characteristic platform attack: an adjacent
platform serving _your_ users adds your function — Microsoft bundled
media streaming into Windows and enveloped Real Audio's 100% market.
Defense options: match the feature (directly or via a partner),
deepen your differentiating design (Vimeo survives YouTube on hosting
quality), or retreat to the niche the bundler can't serve without
self-harm.

**Multihoming is the vulnerability attackers use and defenders plug:**
low switching costs let Facebook unseat Myspace and LinkedIn unseat
Monster. Defensive levers — raise the cost of carrying a second
platform (exclusive supply, non-portable reputation, data lock-in the
participants themselves want), or restrict the flows that would let a
rival walk your users out (Apple banned Flash partly because Adobe's
tools ported iOS apps to Android; Alibaba blocked Baidu's crawlers
_while starving for traffic_ to keep ownership of its own ad revenue).
Every openness decision has a multihoming cost attached — see §11.

### 11. Open-source platform strategy — open the standard, keep the surface

**Core concept (Revolution ch. 7 + ch. 8, 11):** Open source is the
developer-participation openness decision taken to its limit — and the
books' sharpest lesson is that _winning the standard and capturing its
value are different contests_. JVC's licensing model won the format war
(VHS beat technically-superior Betamax on volume → price → titles) and
JVC itself earned only modest, non-lasting profits. Android/AOSP is the
modern version: open multi-vendor code took ~80% smartphone share, but
AOSP sends Google **zero** revenue or data — so Google recaptured value
one layer _up_: proprietary apps, Play-license gating, the Open Handset
Alliance. The rule: **when the code must stay open, capture value via
the critical services, network, and trust layer built on top of it.**

**Manager vs. sponsor** — two different roles, both must be decided:

- The **manager** organizes day-to-day producer/consumer interaction —
  sits closest to users and outside developers.
- The **sponsor** controls the architecture, the underlying IP, and the
  allocation of rights — holds the long-term leverage.
- Four models: **proprietary** (one firm both — Apple), **licensing**
  (one sponsor, many managers — Android+OEMs), **joint venture** (one
  manager, many sponsors — Orbitz's airlines), **shared** (many of both
  — Linux). Corporate-sponsored OSS is a licensing variant: your company
  sponsors, users/forkers manage. Models migrate (Visa went proprietary
  → joint venture → freestanding) — pick for the _next_ stage, and
  remember the direction is one-way: proprietary can only become more
  open; a fully shared platform can only become more closed. Choose the
  starting point you can live with permanently.

**The community is itself a multisided platform** — treat it like one:

- Sides: contributors ↔ maintainers ↔ users. Its core interaction has a
  value unit (a merged patch, an answered issue, a doc) and a filter
  (review + CI). Run §1's design on it, not just on your product.
- Its governance is §6's stack applied to the repo: laws (license,
  CONTRIBUTING, code of conduct), norms (maintainer ladder, earned
  review rights — iStockphoto's crowd-curation arc), architecture (CI
  gates, required reviews — enforcement that needs no judgment scales).
- Its ignition problem is the same chicken-egg: contributors come for
  an active project; activity comes from contributors. Self-supply
  (your own team is the first producer), marquee (known adopters), and
  producer evangelism (contributors bring their users) all apply.

**Ownership and absorption rules** — what to keep proprietary vs. open
vs. upstream:

- Own what is a **primary source of user value** or **could become a
  competing platform** (Apple ate public humiliation to replace Google
  Maps — a dependency that could siphon the monetization). For an OSS
  product: the hosted service, the network/registry, and the trust
  layer are the non-negotiable own-ables.
- **Absorb** functionality once _multiple_ third parties have built and
  adopted it (the Cisco AXP rule — absorb too early and you cannibalize
  a partner's livelihood; too late and commoditization already took the
  value). For OSS: upstream what the community has converged on; keep
  product what only you can run.
- Extension developers → your contributor ecosystem: too closed loses
  their innovation; too open invites low-quality supply that tarnishes
  the project and oversupply that kills incentive. The API/contribution
  surface is the control point — tier it (the Guardian's Keyless /
  Approved / Bespoke access model is the pattern).

**Monetization for open source** maps onto §4's forms: the code is
free → you sell _access to tools_ (developer/enterprise features),
_access to the market_ (hosting, the network only you operate),
_curation_ (verified/certified artifacts, support SLAs). The
precondition is architectural: **open source requires modularity** —
a codebase volunteers can maintain must be decomposable (MacCormack/
Baldwin); an integral blob can't be opened, only abandoned. Architect
for monetization optionality from day one — you can't retrofit
transaction capture onto a design that never touched the transaction.

**OSS-specific risks, named:**

- **Envelopment-by-hosting** — a cloud provider wraps your open code in
  their distribution and captures the service revenue (the AOSP pattern
  run against you). Defenses are §10's: own the network/registry layer
  the code can't function without, differentiate the managed service on
  quality, or adjust the license boundary — knowing that a license
  change is the sponsor's rent lever and the community will judge it as
  governance, not pricing.
- **Fork risk** is the collective-action problem: the ecosystem wants
  one standard; each firm wants a variant. Android's answer was the
  OHA + certification — a consortium structure with a compatibility
  gate keeps the standard unified while the code stays forkable.
- **Governance legitimacy is the moat** — open code is also open
  _oversight_: anyone can inspect what it does (Stallman's point —
  abuse gets exposed and corrected). For a product whose claim is
  verifiability, openness-of-code is itself a positioning attribute,
  not just a distribution choice.

**Why open at all (the books' honest case):** openness multiplies
value units and reach you could never fund; it recruits the
hard side for free (users who can self-supply, self-host, and fix);
it's a commitment device (Android's license told OEMs "invest without
trusting us"); and inspectable code is governance evidence. The costs
are real — fragmentation, weaker control, a capture surface you must
design explicitly rather than own by default. The decision is per-group
(§5), sequenced (§3), and one-directional: deliberate it like the
monetization choice it is.

## References

Deep per-book notes with named cases — consult when a section needs
evidence or a worked example:

- `references/platform-revolution.md` — openness models (proprietary /
  licensing / joint-venture / shared), manager vs. sponsor, the four
  market failures, per-stage metrics, envelopment, launch cases.
- `references/matchmakers.md` — friction taxonomy, critical-mass
  frontier, ignition strategies (zigzag / two-step / commitment),
  pricing level-vs-structure, ecosystem/beyond-the-walls, the full
  six-question checklist + warning signs.
- `references/obviously-awesome.md` — the full 10-step method, the
  three market frames with use-when criteria, trends rules, the sales
  narrative arc, reposition triggers, anti-pattern catalog.

## Common Mistakes

| Mistake                                   | Why it fails                                                   | Fix                                                                                 |
| ----------------------------------------- | -------------------------------------------------------------- | ----------------------------------------------------------------------------------- |
| Charging everyone                         | Taxes the demand that producers pay to reach                   | Name subsidy vs money side; charge the side that benefits from the other's presence |
| Launching to a market, not a micromarket  | Scattered users, no density, empty rooms                       | Constrain until the core interaction self-sustains                                  |
| Openness as a value instead of a decision | Either leaks the capture surface or strangles supply           | Decide per group: manager/sponsor, developer, provider, user                        |
| Open sourcing without a capture surface   | You win the standard, someone else banks it (JVC/VHS, AOSP)    | Own the layer above: hosted service, network/registry, trust layer                  |
| Charging for what was free                | The community reads it as a rule change, not a price change    | New value justifies the charge; publish it with the taper                           |
| Opening an unmodular codebase             | Volunteers can't maintain an integral blob; "open" = abandoned | Decompose before releasing; modularity is the OSS precondition                      |
| Signup metrics                            | Vanity totals hide zero liquidity                              | Measure interaction success: fill rate, time-to-match, repeat-interaction rate      |
| Governance after the fraud                | Quality collapse precedes the fix                              | Laws + norms + architecture designed pre-density                                    |
| Positioning as "better X"                 | Incumbent's network beats your features every time             | Big-fish-small-pond or new-game frame where your attributes decide                  |
| Ignoring the ecosystem dependency         | The platform dies when a partner/infra you don't control does  | Degradation strategy, not just dependency                                           |
| Subsidies without a published taper       | Hard-side revolt when terms quietly degrade                    | Publish the taper with the subsidy                                                  |
| Charging at entry/listing, not completion | Shrinks supply _and_ the matching data you learn from          | Skim the transaction at deal completion                                             |
| Positioning on unprovable attributes      | "Easy to use" is an opinion, not a differentiator              | Keep only attributes a third party, standard, or data can prove                     |

## Quick Diagnostic

| Question                                             | If no                             | Action                                          |
| ---------------------------------------------------- | --------------------------------- | ----------------------------------------------- |
| Can you state the core interaction in one sentence?  | It's a product, not a platform    | Write participants + value unit + filter first  |
| Can you name the subsidy side and money side?        | Pricing is unpriced               | Apply the whom-to-charge rules                  |
| Is a liquidity metric instrumented?                  | You're flying on vanity metrics   | Track fill rate + time-to-match before anything |
| Does governance exist before the fraud does?         | Retrofit will be too late         | Laws, norms, architecture in the design         |
| Is the frame of reference chosen deliberately?       | The market will choose it for you | Dunford's three options, picked on purpose      |
| Can the ecosystem kill you if it fails?              | Name the degradation path         | Fallbacks, not dependencies                     |
| (OSS) Is the capture surface separate from the code? | You donated the business          | Own the layer above: host, network, registry    |
| (OSS) Is the codebase modular enough for outsiders?  | It can't be opened, only dumped   | Decompose first; modules before license         |
