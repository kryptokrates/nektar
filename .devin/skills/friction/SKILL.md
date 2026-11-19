---
name: friction
description: >
  Find and remove adoption friction across user journeys, onboarding
  flows, join paths, APIs, setup files, and code. Use when the user
  mentions "friction", "drop-off", "abandoned signup", "onboarding",
  "time to first value", "TTFV", "time to first call", "TTFC", "too
  many steps", "make it easier", "zero-config", "one-click",
  "self-serve", "developer experience", "DX", "conversion", "why
  aren't people adopting", "reduce friction", "adoption friction",
  "underdog", or asks for a friction audit of a journey, doc, .env,
  CLI, API, or compose path. Applies the friction canon: Dooley's
  FRICTION (the audit, three currencies), The Effortless Experience
  (CES — loyalty is low effort, not delight), Nudge (defaults carry
  the path), Fogg B=MAP (ability beats motivation), Gabriel's
  worse-is-better (underdogs win on simplicity and portability),
  Product-Led Growth (straight line + bumpers, DAD test), Jakob's
  law (conform to known shapes), Postman's TTFC. For network-effects
  strategy use cold-start; for code complexity as friction use
  design-guardrails; for wait-site/event friction use event-arch.
---

# friction — adoption-friction auditor & remover

An underdog system competes against friction that is already paid: the
incumbent is installed, configured, learned. Every second of wait and
every unnecessary decision in our path is a stranger choosing the
incumbent. This skill runs a friction audit on any journey — a stranger
joining the network, a developer making a first call, an agent reaching
first value — and converts every finding into a kill / keep-legible /
move-off-the-clock decision.

## Advisory process

1. **Name the journey.** Pick exactly one: stranger→announced operator,
   clone→first `recall` with content, adapter config→first tool call,
   want→settled deal, error→named cause. Different journeys, different
   friction; never audit "the product" in the abstract.
2. **Walk it as the stranger.** Every step: timestamp it, count the
   decisions it demands, list the knowledge it assumes. Read every
   "just" in the docs as a lie detector. See
   `references/audit-playbook.md`.
3. **Triage each point.** Kill (dead friction), keep-but-legible
   (load-bearing checks), or move (necessary work shifted to defaults,
   pre-warming, or our side of the clock). The triage is section 2.
4. **Order fixes by leverage.** First-value-adjacent first; a drop-off
   at step 2 is worth ten times a drop-off at step 10. Report the
   straight-line path and what blocks it.
5. **Instrument.** Every journey gets a named first-value moment and a
   measurable time-to-reach it. Advice without the metric is a wish.

## Core Principle

**Friction = unnecessary expenditure of time, effort, or money**
(Dooley). It is countable, comparable, and — for an underdog — the
whole game. The incumbent's product can be worse and still win if our
path is harder to start than theirs is to keep. So the discipline is:
make the happy path require _zero knowledge, zero decisions, and
bounded waits_; spend the complexity on our side of the clock
(pre-building, pre-warming, pre-seeding); and never make the user wait
for work we could have done earlier.

## Scoring

**Goal: 10/10.** Rate any journey, join doc, setup file, or adoption
plan 0-10. Report the score and the specific changes that reach 10/10.

- **9-10:** Named first-value moment with a measured time-to-reach; a
  straight line with ≤5 steps; correct defaults everywhere; waits are
  named, bounded, and progress-visible; errors say what they're waiting
  on and what to do next
- **7-8:** Works end-to-end unattended, but a non-essential decision or
  an undocumented wait remains on the happy path
- **5-6:** Requires config before first value, or reading docs to
  unblock a predictable dead end
- **3-4:** Multi-step setup with assumed knowledge; first value gated
  on other people's actions (funding, approval, a second operator)
- **0-2:** Signup wall, sales call, manual approval, or required
  external account before any value is visible

A journey with no measured time-to-first-value cannot score above 6.

## Framework

### 1. The friction audit — count three currencies

**Core concept:** Friction is not a vibe; it is a ledger. Every step in
a journey spends three currencies — **time** (seconds waited), **effort**
(decisions, context switches, typing), and **knowledge** (concepts the
user must already hold). You cannot remove what you have not counted.

**Why it works:** Dooley's observation — once you wear the goggles you
see friction everywhere, and the expensive kind is never where the
builders guessed. Builders know why each step exists, so none of them
_feel_ like friction. The stranger pays them all anyway. Auditing the
actual journey — not the intended one — is the only reliable method.

**Key insights:**

- Every "just" in documentation is a lie detector: "just generate a
  WIF", "just point your DNS" — each is 10–30 minutes and assumed
  knowledge for a stranger
- The earliest friction is the most expensive: drop-off compounds, so a
  step-2 leak costs more than a step-10 leak
- Silent waits are the worst currency spend — an unnamed 10-minute wait
  reads as "broken", not "working"
- Required knowledge is friction even when it costs no time: BSV
  concepts, TLS, DNS, Docker internals are all admission fees
- Friction hides in the tail: error recovery, upgrade, and teardown
  paths are audited too — a scary exit is a reason to never start

**Applications:**

| Surface         | What to walk                                  | Currency hot spots                    |
| --------------- | --------------------------------------------- | ------------------------------------- |
| Join doc        | clone → announced → unioned                   | secrets, TLS/DNS, funding key         |
| `.env.example`  | every field: does it have a correct default?  | decisions = drop-offs                 |
| Compose/install | commands run, prompts answered, waits endured | image pulls, db warmup, lock ordering |
| Adapter attach  | config paste → first tool call                | which platforms get a one-line stanza |
| Error path      | broken thing → named cause → next command     | log legibility, searchable error text |

### 2. Kill / keep-legible / move — the triage

**Core concept:** Not all friction is bad. Every point lands in one of
three buckets, and the skill's job is sorting, not blanket removal:

- **KILL — dead friction.** Waits nobody needs, decisions that have one
  right answer, steps that exist for the implementer's convenience,
  retries that make the user do the system's job.
- **KEEP-but-legible — load-bearing friction.** Verification, consent,
  confirmation, security gates. These carry safety or honesty. Never
  removed — made legible: named, progress-visible, honest about cost.
- **MOVE — work shifted off the user's clock.** Defaults, pre-warming,
  pre-seeding, bundling, lazy/async work the system does for them. The
  step still happens; the user never waits on it.

**Why it works:** Product-Led Onboarding's DAD test keeps the helpful
friction: a step earns its place only if it **D**irects the user toward
value, **A**dds personalization or trust, or **D**elights. Everything
else is dead weight. Meanwhile load-bearing checks exist precisely
_because_ the system distrusts them — removing them is not
optimization, it is a defect.

**Key insights:**

- A decision with a correct answer is a default, not a question. Every
  field a stranger must reason about is a drop-off point
- Pre-pay the cost on our side: we compile the bundle, bake the image,
  warm the lane stock, seed the packs — the user never waits for work
  we could have done earlier
- Verification stays, the _wait_ goes: SPV checks run async; the user
  is told "proving — safe to use meanwhile" rather than being parked
- "Documented" is not "removed": a paragraph that explains a step still
  makes the user do it — a snippet, default, or pre-baked artifact
  removes it

**Ethical boundary:** Friction that protects the user, the
counterparty, or the network is load-bearing and is never smoothed
away: `NEKTAR_MAINNET_CONFIRM`, SpendGuard caps, signature and SPV
verification, refund windows, consent prompts on spends. A default
that hides a cost or skips consent is a dark pattern, not a fix.
Legibility is the move: name the gate, show its progress, explain its
reason — then make it fast.

### 3. Defaults and the straight line

**Core concept:** The path of least resistance **is** the product.
Nudge's default effect is the largest measured lever in choice
architecture (meta-analysis d≈0.68): people take whatever option
requires least effort, so the default path must be the correct path —
and the Bowling Alley keeps them on it: a straight line to first
value, with product bumpers (progress, checklists, inline hints) and
conversational bumpers (errors that name the next step).

**Why it works:** Every decision point is a tax paid before value is
proven. Sequencing is the fix — never front-load: ask for the funding
key when funding is needed, ask for public URLs when announcing is
needed, never in the first three commands. A fresh instance that boots
non-empty and answers a real query teaches the product better than any
config wizard.

**Key insights:**

- One page, one goal: the quickstart ends at first value in ≤5 steps;
  everything beyond links away, it does not gate
- Correct-by-default everywhere: `.env` fields that matter get a
  working value or a generator, not an essay
- Bumpers, not fences: checklists and next-step error text recover
  strays; validation walls that refuse boot do not
- The opt-in ladder: power knobs exist for operators who ask —
  `NEKTAR_*` env is the ladder, never the front door

**Applications:**

| Pattern              | In this repo                                                  |
| -------------------- | ------------------------------------------------------------- |
| Correct default      | `NEKTAR_MARKET_HOSTS` optional — discovery is the default     |
| Generator, not essay | `install.sh`, `.env.example` comments that _generate_ secrets |
| Straight line        | JOIN-TESTNET: up → verify card → union — three acts           |
| Bumper               | `doctor` tool, preflight gate, phase-named upkeep errors      |

### 4. Jakob's law — conform to known shapes

**Core concept:** Users arrive with mental models trained by every
tool they already use. Matching a known shape deletes the entire
learning curve; inventing a new one charges full tuition at the door.
Novel value, familiar packaging — the innovation lives inside, the
surface looks like what they already run.

**Why it works:** "One more MCP server" is a stronger pitch than "a
new protocol" because the config stanza is already in the user's
muscle memory. A compose file, a `.env`, a bearer token, an OpenAPI
route — all known shapes. Every custom shape we introduce is a new
concept we must sell, while a known shape sells itself.

**Key insights:**

- Copy the incumbent's file format and command shape exactly —
  deviation is friction with zero payoff
- Platform-native packaging beats documentation: MCP stanza for
  Claude Code, OpenAPI routes for HTTP clients, compose for ops
- Name things what the ecosystem names them: `tools`, `health`,
  `token`, `compose.yaml` — not bespoke vocabulary
- The universal adapter strategy _is_ Jakob's law: one integration
  shape every platform already knows, instead of one adapter per
  platform

### 5. Underdog dynamics — worse is better

**Core concept:** Gabriel's survival law: the simple, portable,
good-enough system spreads before the right thing finishes, and the
thing that spreads gets improved into the right thing. For an
underdog, the bar is not the incumbent's feature list — it is their
_switching cost_. Our path only needs to beat "keep what they have",
which is a much lower bar — if the first step isn't frictionless, we
never get to compete on quality at all.

**Why it works:** Adoption precedes evaluation. Strangers do not
benchmark two systems; they try the easier one and never come back.
Thin, shipped, conforming paths compound (they get used, improved,
recommended); perfect unshipped paths compound nothing.

**Key insights:**

- Ship the thin path first — a working thin adapter beats a perfect
  bespoke integration (MCP-now > the ideal universal adapter later)
- Portability is survival: image-only compose, one-file configs,
  content-addressed artifacts — anything that runs anywhere without
  a checkout or a build
- Condition users upward: zero-config first-run, power opt-ins later
  — never front-load the operator manual
- Friction beats features: a competitor's missing feature matters
  less than our missing step; remove the step

### 6. Measure it — TTFC, TTFV, CES

**Core concept:** Friction work is metric work. Every journey names
its first-value moment and measures time-to-reach it; every funnel
names its drop-off points. Without the metric, friction fixes are
guesses and regressions are invisible.

**Why it works:** Postman's API research: time-to-first-call is the
single strongest adoption predictor — providing a runnable collection
beat even excellent docs 1.7–56×. The Effortless Experience research:
low effort predicts loyalty where satisfaction does not. What gets
measured gets removed.

**Metrics per journey (this repo):**

| Journey          | First-value moment                 | Metric                        | Target   |
| ---------------- | ---------------------------------- | ----------------------------- | -------- |
| Operator (hub)   | card visible on dir lane           | clone → announce              | < 30 min |
| Fresh instance   | `recall` returns seeded content    | start → non-empty answer      | < 5 min  |
| Adapter client   | first successful tool call         | config paste → `recall`       | < 3 min  |
| Deal             | want → quote seen                  | publish → first offer (watch) | bounded  |
| Recovery         | named cause + next command in logs | error → legible cause         | instant  |
| Suite (internal) | first green verdict                | launch → first pass           | minutes  |

Instrumentation rules: log the step the user was on, not just the
error; errors name what they are waiting on and the next command; and
once per milestone, _ask the stranger_ — "how hard was that, what
surprised you" beats any dashboard (CES).

See [references/audit-playbook.md](references/audit-playbook.md) for
the audit procedure, [references/levers.md](references/levers.md) for
the removal-lever catalog, and
[references/nektar-map.md](references/nektar-map.md) for this repo's
live friction map — journeys, checkpoints, and where the wall is
today.

## Common Mistakes

| Mistake                                  | Why It Fails                                               | Fix                                                              |
| ---------------------------------------- | ---------------------------------------------------------- | ---------------------------------------------------------------- |
| Removing the check instead of the wait   | Verification is load-bearing; a skipped SPV is a defect    | Keep the check; parallelize, pre-warm, or background the wait    |
| Front-loading decisions                  | Config questions before value = abandoned installs         | Sequence: ask only when the input is actually needed             |
| "Documented, therefore removed"          | A paragraph describing a step still makes the user do it   | Snippets, defaults, generated artifacts — remove the step itself |
| Silent waits                             | Unnamed waiting reads as broken; users quit or retry wrong | Every wait gets a name, a bound, and visible progress            |
| New surface shapes                       | Custom formats charge learning-cost at the door            | Conform to MCP/compose/OpenAPI/bearer shapes users know          |
| Treating drop-off as a marketing problem | More traffic into a leaky funnel scales nothing            | Fix the earliest drop-off first; measure the funnel              |
| Gate-keeping by domain knowledge         | Requiring BSV/SPV/TLS literacy is an admission fee         | Hide the expertise; surface it only on opt-in                    |
| Friction for everyone, trust for no one  | Removing _all_ friction invites spam and abuse             | Friction for abusers, none for the good — asymmetric by design   |

## Quick Diagnostic

| Question                                                  | If No                                     | Action                                               |
| --------------------------------------------------------- | ----------------------------------------- | ---------------------------------------------------- |
| Is there a named first-value moment with a measured time? | Friction work is unmeasured               | Define the moment; instrument the clock              |
| Does the happy path need zero decisions?                  | Each decision is a drop-off               | Convert to correct defaults; sequence the rest       |
| Does every env field have a working default or generator? | Required reasoning = required abandonment | Default it, generate it, or delete the field         |
| Does first value arrive before any external dependency?   | Gated on strangers = gated forever        | Ship seeded/provable content; flintstone honestly    |
| Do errors say what they wait on + the next command?       | Silent waits read as broken               | Name the phase, bound it, print the bumper           |
| Does the surface match shapes users already know?         | Learning cost charged at the door         | Conform to MCP/compose/OpenAPI conventions           |
| Is every load-bearing check still present and legible?    | Friction removed where safety lived       | Restore the gate; make it named and progress-visible |
