# The Friction Audit — Procedure

## Table of Contents

- [The Method](#the-method)
- [Three Currencies](#three-currencies)
- [The Walk Rules](#the-walk-rules)
- [Artifact Checklists](#artifact-checklists)
- [Recording Findings](#recording-findings)
- [The Leverage Order](#the-leverage-order)
- [Good-Friction Tests](#good-friction-tests)

## The Method

A friction audit prices one journey in three currencies, then sorts
every point of spend into kill, keep-legible, or move. It is always run
against a **named stranger** on a **named journey** — never against
"the product". The auditor's own knowledge is the enemy: you must
borrow the eyes of someone who has never seen the system.

The procedure:

1. **Name the stranger and the journey.** "A dev with Docker who has
   never heard of BSV, going from `git clone` to their card visible on
   the dir lane." The journey ends at a named first-value moment, not
   at "everything works".
2. **Walk it cold.** Execute the actual path — read only what a
   stranger would read, run only what they would run. Stopwatch on.
   Where you can't walk (no spare box), read the artifact line by line
   and price each line.
3. **Ledger every step.** For each step record: the three currencies,
   what can go wrong, and whether the system says what it's doing
   while it works.
4. **Triage.** Kill / keep-legible / move per step (see SKILL.md §2).
5. **Order by leverage.** The earliest drop-off is the most expensive —
   a 50% loss at step 2 costs more than a 90% loss at step 10.
6. **Re-walk.** An audit that doesn't end with a re-walk on the fixed
   path is a document, not a fix.

## Three Currencies

Every step in the journey spends from three wallets. Price all three —
a step can be fast and still expensive.

**Time** — wall-clock seconds the user waits or works. Includes
typing, reading, installing, downloading, and waiting on the system.
The benchmark: the incumbent's equivalent step, not zero. Waiting is
charged double when unnamed — a named 10-minute block wait reads as
progress; the same wait silent reads as broken.

**Effort** — decisions, context switches, and motor work. Each
decision costs attention and creates a bail point. A field that asks
for a value is effort; a field that asks the user to _invent_ a value
(generate a secret, pick a topology) is effort squared. Copy-paste is
cheap; composing a command is expensive; reasoning about tradeoffs is
the most expensive.

**Knowledge** — concepts the user must already hold for the step to
make sense. BSV, UTXOs, SPV, TLS termination, DNS, Docker internals,
wallet files, "what is a WIF". Knowledge assumed but unearned is the
steepest wall — the stranger can't even fail productively; they just
leave. Every concept on the required list either gets a one-line
conform (Jakob's law) or moves behind the value moment.

## The Walk Rules

- **No designer immunity.** The auditor does not get to use knowledge
  the stranger lacks. If you know the port, the stranger doesn't.
- **"Just" is a lie detector.** Every "just X" in a doc — "just set a
  public URL", "just fund the key" — is a ledger entry of 10–30
  minutes plus assumed tooling.
- **Follow the error, not the happy path.** Deliberately skip a step,
  mis-set a value, kill a container — does the system name the cause
  and the next command, or does it go quiet?
- **Count the asks.** Every prompt, field, flag, and required external
  account is one decision. Total decisions is the audit's headline
  number alongside total minutes.
- **Time the waits with names.** For every wait record whether the
  system said what it was waiting on. Unnamed waits get flagged even
  if short.
- **The exit is part of the journey.** Teardown, key rotation,
  upgrade, and uninstall friction count — a scary exit is a reason to
  never start.

## Artifact Checklists

### `.env` / config file audit

- [ ] Does every field have a correct default or a copy-paste
      generator? (A decision with a right answer is a default, not a
      question.)
- [ ] Are secrets generatable (`openssl rand -hex 32`) rather than
      sourced externally?
- [ ] Are fields sequenced — nothing required before it's needed?
- [ ] Does every comment tell the reader what to _do_, not what the
      field _is_?

### Compose / install audit

- [ ] Does it pull published images, or require a checkout/build?
- [ ] How many commands to first healthy log? Count them.
- [ ] Are waits self-healing (`restart: unless-stopped`, healthchecks)
      or does the user babysit?
- [ ] Is there exactly one documented "it works" signal?

### Join-doc audit

- [ ] Does the first section state the whole ask in one breath (box,
      time, money)?
- [ ] Is there a single straight-line path, or a decision tree?
- [ ] Does every step end with a verifiable "you should see X"?
- [ ] Is failure recovery one lookup away, legible by a stranger?

### Adapter / API & Ecosystem Docking audit

- [ ] Does bare zero-env boot (`./nektar` or `docker run` with no `-e` flags)
      reach a healthy `recall {}` without crashing?
- [ ] Does one command (`nektar dock auto`) auto-detect installed AI apps,
      merge non-clobbering config, and run a live `recall {}` probe?
- [ ] Can two AI clients (e.g. Claude Code + Hermes or Cursor) connect
      simultaneously to the same local data dir without `instance.lock`
      contention?
- [ ] Does stdio mode keep `/v1/verify` receipt links, hooks, and the
      dashboard alive (via companion loopback HTTP or shared daemon)?
- [ ] Does the harness integration climb past L0 paste into native slots
      (Hermes `MemoryProvider`, OpenClaw `plugins.slots.memory`, Claude
      `.claude-plugin`, `agentskills.io` `SKILL.md`, MCP `server.json`)?
- [ ] Ambient turn test: open a fresh session and ask a question _without
      mentioning nektar_ — does prefetch or session context arrive
      automatically, bounded and fail-open?
- [ ] Portable-path test: do hooks and plugin manifests work from a release
      artifact with zero hardcoded repo-checkout paths?
- [ ] Clean exit test: does `--uninstall` detach cleanly while leaving
      sibling MCP servers, hooks, and instructions untouched?
- [ ] Is the first tool call the one that demonstrates value (`recall`
      with content, not `health`)?
- [ ] Do errors return the action ("publish a want") not the
      mechanism ("lane slot unproven")?

### Cold-Stranger vs. Warm-CI Measurement Rule

Always log **two** numbers for every instrumented journey so CI speed never
masks cold-machine friction:

- **Warm / CI (`friction.test.ts`):** in-process or local-binary regression
  guard (milliseconds). Catches silent breakage in PRs.
- **Cold stranger (clean box / clean dir / no checkout):** wall-clock time
  from download/install to first real value, plus total decisions asked.
  This is the number that governs the 0–10 score.

## Recording Findings

One line per finding:

```
[step] | time | effort | knowledge | silence? | triage | fix
```

Example:

```
.env: NEKTAR_MARKET_HOSTS | — | 1 decision | 'what are public URLs?' | — |
  move | default to discovery; field becomes opt-in
env: KEY_PASSPHRASE | 2 min | invent a secret | openssl | — |
  move | install.sh generates all four
join: funding WIF | 20 min | wallet+faucet flow | BSV, WIF, faucet | — |
  move | auto-WIF + grant link in doc
```

## The Leverage Order

1. **First-value-adjacent friction** — whatever sits between the user
   and the aha. Nothing else matters until this is clear.
2. **Drop-off-weighted order** — multiply each step's cost by the
   fraction of users who reach it. Step-2 friction always wins.
3. **Frequency multipliers** — a step repeated per use (every deal,
   every boot) compounds; amortize it or eliminate it.
4. **Confidence** — prefer the fix you can verify by re-walking over
   the fix you believe in.

## Good-Friction Tests

Before killing a step, run it through the keepers:

- **DAD test** (Product-Led Onboarding): does the step **D**irect the
  user to the next step, **A**dd value/personalization, or
  **D**elight? Any yes → it might stay, tightened.
- **Load-bearing test**: does the step carry safety, consent, or
  verification the system is _for_? (Confirmation gates, SpendGuard
  caps, SPV checks, refund windows.) Yes → keep-legible, never remove.
- **Abuse test**: does the step price out abusers more than users?
  Rate limits, funding proofs, small stakes — friction _for abusers_,
  none for the good, is a feature (see cold-start §5). Keep — and keep
  it asymmetric.
