---
name: spv-strict
description: >
  Use to audit or design anything that answers "is this transaction real,
  mined, final, spendable" — proof handling, header sources, funding
  ancestry, BEEF/BUMP wire shapes, wallet import, funding-tree stock, and
  every place the deal path waits. Strict: every finding is BLOCKER, HIGH
  or MED, never advisory. Two goals, both mandatory — (1) nothing is
  believed that the verifier did not compute against headers it chose and
  checked, and (2) no deal-path step waits for a block that carried
  evidence or pre-proven stock would have made unnecessary. Grounded in
  1BSV/Lightweb papers 011, 009, 006, 008, 007 and Wright's SPV essays
  (singulargrit.substack.com). For the wider event layer use event-arch;
  for money-path invariants use code-review's nektar.md.
---

# spv-strict — proof discipline and zero block waits

SPV is one act of trust and then arithmetic: choose header sources,
check their work, and compute everything else from bytes in hand
(paper 011 §2.1). This skill audits both halves of that sentence:

- **Trust half** — is anything believed that was not computed? A status
  string, an indexer's answer, a header taken from whoever served the
  proof, a txid without its bytes.
- **Latency half** — is anything waited for that was already provable?
  An unmined transaction has no merkle path, so SPV cannot prove it
  before a block. The fix is never "trust it early"; it is to arrange
  that the deal path only ever needs things that are ALREADY proven:
  evidence that travels with the transaction, and stock that was mined
  ahead of use ("the patterns do not wait for the network", paper 009).
  Carried, checked ancestry is what makes an unconfirmed transaction
  "already proven" — maximal safe zeroconf serves the whole deal path
  from verified BEEF chains up to the ancestor-policy bound, and only
  value-final settlement waits for depth (D4, D7).

A finding in either half is a defect. Waiting on a block mid-deal is not
caution; it is a missing proof or missing stock.

## Operating modes

Audits run **inline** by default — a diff's claims and waits fit in one
pass. They run as a **swarm** on whole-repo runs and explicit full
audits per [references/orchestration.md](references/orchestration.md):
two inventory agents map claims and waits in parallel, trust-half and
latency-half rule-walkers work the merged maps, and a fresh verifier
challenges every candidate — there is no advisory tier, so every
finding is worth the check.

## Process

### 1. Scope

`--files`, `--pkg`, `--last [n]`, or default → the in-flight diff plus
untracked `packages/*/src`. A whole-repo run covers `packages/chain`,
`bstack`, `automation`, `mcp`, `anchor` and `deploy/bstack-sandbox`.

### 2. Inventory — two lists

In swarm mode these two lists come from parallel inventory agents and
the rule walk is delegated per half (orchestration.md). Inline mode:

**Claims.** Every place code concludes mined / proven / rejected /
spent / unspent / final / funded. For each: what object carries the
claim (BUMP, BEEF, status string, index row, host answer), which header
source anchors it, and what depth policy applies.

**Waits.** Every place the deal path can stall: sleeps, polls, `proven`
gates, "no funding slot", "not proven yet", warmups, `waitFor`. For
each: what is it waiting on, could the needed fact have been proven
before the wait began, and what stock or evidence was missing.

### 3. Walk the rules

Load [references/doctrine.md](references/doctrine.md) and
[references/rules.md](references/rules.md) always. Load
[references/checklist.md](references/checklist.md) when the scope
verifies third-party data (carriers, envelopes, counterparty wires,
imports).

### 4. Report

```
## spv-strict audit — <scope>

**Verdict: STRICT | DRIFT | BROKEN**
(STRICT = no BLOCKER/HIGH; BROKEN = any BLOCKER)

### Findings
| # | Sev | Rule | Where (file:line) | What is believed or waited for | Evidence | Fix (seam) |

### Claims map
<each mined/proven/rejected/spent/final claim: proof object · header anchor · depth>

### Waits map
<each deal-path wait: waits on · provable beforehand? · missing stock/evidence>

### Scores
- Computed, not believed: N/M claims carry a proof object checked against our headers
- Header independence: sources, PoW-checked, never the proof's provider
- Evidence travels: handoffs carrying BEEF / EF / raw / txid
- Block waits on the deal path: N (target 0) — each listed with its cause
- Zeroconf utilization: deal-path steps served by verified carried
  ancestry vs those still waiting on blocks; first-party chain lengths
  vs the ancestor-policy bound; counterparty caps present and enforced
- Stock: per lane, proven slots vs refill threshold; lanes that can reach 0
  or reach the chain-length cap before refill
```

Every finding states its evidence: the line that shows it, or the
command/output that reproduced it. A finding without evidence is a
hypothesis and is labelled as one, with the check that would settle it.

Suggest only, unless asked to fix. A fix never trades proof for speed:
if the only way to remove a wait is to believe something unproven, the
finding stays open and says so.

## Severity

- **BLOCKER** — an unproven claim moves money or durable state; or a
  header is accepted from the party whose proof it anchors.
- **HIGH** — a deal-path step can wait on a block; a lane can reach zero
  proven stock while funded; evidence held by the sender is not handed
  over; an indexer answer is used as truth.
- **MED** — the same classes off the deal path, or bounded by an
  existing floor that still makes the result correct but slow.

There is no advisory tier. If it is not worth fixing it is not a finding.

## nektar calibration

- `ChainEvents.note()` is the hint channel and `observe()`/`report()`
  the verified one; `txState` answers 'rejected' only from first-party
  truth. Any new path that promotes a status string is a BLOCKER.
- `WitnessedTracker` over PoW-checked `ChaintracksTracker` + `WocTracker`
  is the header anchor on live networks. A tracker constructed any
  other way on a live path is `spv/unanchored-header`.
- Funding trees are postage (paper 011 §6): one mined tree admits M
  carriers. `TreeLedger.take` serving zero slots while the wallet holds
  funds is `spv/unstocked-lane`, not a normal state.
- First-party unconfirmed ancestry is the repo's zeroconf ceiling: a
  chain of our own spends is verifiable to the ancestor-policy bound,
  not to a hardcoded one hop. `anchoredTree` (one unconfirmed hop above
  a mined parent) is the implemented floor — extending it toward the
  policy cap is a stocking question; every hop still requires the
  parent's merkle path IN the BEEF, checked against bytes not comments.
- Counterparty unconfirmed ancestry is bounded trust: provenance
  verified, exclusivity not. Caps (`maxUnconfirmedAncestors`,
  unconfirmed-value limits) plus conflict watches (a conflicting tx's
  own BUMP, `abandoned` events) are the trip wires; escalation to
  depth is the anomaly path, not the default.
- The vendored bbox / bfinger / blogs host codecs require the carrier's
  funding tree itself to be mined (one BUMP, two transactions). Do not
  propose weakening them; stock those lanes ahead instead.
- Settlement keeps its depth rule (`settleDepth` = `finalityDepth`).
  Depth is the security parameter; it is never a finding that money
  waits for depth. It IS a finding if anything other than money does.

## Sources

1BSV / Lightweb (1bsv.net/papers): 011 _The data carrier_; 009 _The
bstack_; 006 _The overlay object plane_; 008 _The overlay bridge_; 007
_Arcade on the multicast fabric_. Wright (singulargrit.substack.com):
_Safe Low Bandwidth SPV: A Formal Treatment_; _The Protocol No
Gatekeeper Wants_; _A Mechanism of Honour_; _The Audit of Fools_;
_Verification Without Enforcement_. Whitepaper §8. BRC-9/67 (SPV),
BRC-62/95 (BEEF), BRC-74 (BUMP). Reference implementation:
`lightwebinc/beefcat` (netcat for the BEEF data carrier, with SPV —
its `docs/verification.md` is the REFUSED-* vocabulary and documents
its own header-source limits; see rules.md). The Substack essays are
partly subscriber-only: rules cite only what the public text states.
