# principles — Wright / BSV-stack model → what it demands in nektar

Each principle: the source idea, and the concrete demand it places on
code in this repo. Rule IDs referenced are `ev/*` (this skill) and the
shared `data/*`, `stab/*`, `struct/*` catalogs in
`../../code-review/references/`.

## P1 — SPV truth: verify proofs, never trust the middle

Wright's core: you don't run a full node, you hold headers + merkle
proofs and verify locally. A third party's claim about chain state —
"it's mined", "it doesn't exist" — is a **hint**, not a fact.

Demands:

- Pushed broadcaster/host statuses never directly emit truth. The repo
  already encodes this: `ChainEvents.note()` records hints for waiters
  and dedup; only `observe()` (our own reads — asset API, RPC,
  chaintracks) emits 'tx'. `ev/unverified-status` on any path where a
  hint drives a money/reveal/spend decision.
- Absence of a record is NEVER proof of nonexistence — a host
  withholding is indistinguishable from "not there". `ev/absence-as-proof`
  (the withheld-offers test class is the in-repo precedent; DDIA's
  `data/read-your-writes` is the same shape).
- Every status that matters needs a named verifier: `observe`,
  `verifyMembershipAnchoredStark`, `isValidRootForHeight`, digest check.
  If the verifier can't be named, the status is decorative.

## P2 — UTXO transitions are the events

The chain's real vocabulary: `received → unconfirmed → mined` and the
terminal failures `rejected / double-spent` (`TxStatus` in
`interfaces.ts`). A UTXO is a state machine — funding → escrow →
disclosure → settled/refunded is UTXO transitions, not wall-clock.

Demands:

- State machines key on UTXO transitions + producer signals; heights are
  _ceilings_ (the escrow `dueHeight` refund path), never the driver.
- `onStatus` push (RECEIVED→ACCEPTED→SEEN) is the fast path; the shared
  `ChainEvents` poller is the floor; `waitFor`'s deadline is the leak
  guard. Three tiers, each with a job — don't merge them.
- First-seen is actionable signal: the moment a tx propagates, internal
  state may advance (watch armed, offer expected); money-moving
  consequences still gate on verified proof. Split signal vs proof —
  never collapse them into "wait for confirm".

## P3 — Events push; polls are ceilings ("no timeout as mechanism")

The network pushes to you — subscription, not scanning. In-repo: the
watch-shim's own law, _"a wake is only ever a hint, never the load
path"_ — wake shortens latency, the bounded poll carries correctness.

Demands:

- Every wait names its producer signal, and the producer signals on
  EVERY path — success AND failure (`ev/lost-wakeup`; the
  `unblockOutbox` template: host-recovery watchdog unblocks deferred
  outbox rows the moment reachability returns).
- Timers exist only as bounded ceilings — `waitFor` deadlines, watch
  TTLs, `abandonAfterBlocks`, `maxPerTick` rotation. A timer that IS the
  mechanism (fixed poll waiting on a condition that emits) is
  `ev/timeout-as-mechanism` / `ev/poll-for-event`.
- Hot-path cadence reacts to events, not the clock: `hotUntil` keeps the
  deals loop at 250ms for 5s after inbound activity; silence decays to
  the idle cadence. New event sources feed `hotUntil`, not new timers.

## P4 — Peer-to-peer means peer-to-peer

Wallets exchange directly; a MessageBox holds encrypted payloads for the
offline peer — store-and-forward, not "wait until they're online". The
counterparty advertises _where its artifacts live_ (hosts, office,
watch shim) in its own record; resolution is lazy and cached.

Demands:

- Deliveries target the _counterparty's advertised_ endpoints
  (`hostsFor` merges our market hosts with the peer's record). Never a
  shared bus the sender controls.
- Offline ≠ error: store-and-forward via lanes/outbox — the receiver's
  polls find it later; a wake hint (`notifyPeer`, fire-and-forget 2s)
  shortens the wait but carries nothing load-bearing.
- Deferred work fires on the event it was waiting for — the deferred-ack
  pass runs when the sender record resolves (runtime.ts:205), not at the
  next tick. `ev/timeout-as-mechanism` if a fixed poll replaces this.

## P5 — Content-addressed = seamless interoperability

"SHA-256 of the bytes IS the address" — `chirp/<commit>`, `proofStore`,
pack shares: any copy self-verifies on the digest; the chain holds only
the commitment (`hash + url + expiry`).

Demands:

- Reference by digest, never by location (`ev/location-coupled`): a
  receipt carrying `{endpoint, bucket, digest}` resolves from ANY
  gateway copy — a reference that names one host is a SPoF and an
  interop break.
- New artifact shapes get a `v:`/`kind:` gate on the wire envelope and
  tolerant readers — a foreign implementation parsing our envelope must
  degrade, not die.
- Interop is proven, not claimed: shared test vectors/conformance —
  the deck's "run the same vectors, compare outputs". New wire shapes
  ship with vector coverage (`automation/conformance` is the existing
  harness pattern).

## P6 — Fanout, not single points

The network is already redundant — resubmit rather than re-route. On our
paths: publish to N hosts, track per-destination (outbox rows),
watchdog detects recovery → `unblockOutbox`, partial fanout gets a
repair sweep.

Demands:

- A single-host path on money/durability is `ev/spof` — minimum two
  independent endpoints or a documented rebuild-from-source story.
- `ev/untracked-fanout`: a write to N destinations needs per-destination
  state (who got it) — or you can't answer "did everyone see it" and
  can't repair 2-of-3.
- Faulthost exercises down/slow/withhold/corrupt for every multi-host
  path — an unproven resilience claim is a claim.

## P7 — Degrade to poll, never to failure

Every push path has a poll floor: shim down → ordinary polls; long-poll
dropped → reconnect with cursor resume; watch expired → the tick
re-arms it. Liveness comes from the floor; latency comes from the push.

Demands:

- `ev/no-fallback`: a subscription that dies silently (no cursor resume,
  no poll floor, no re-arm tick) converts a transient disconnect into a
  permanent blindness — `refreshWatches`'s 15s re-derive is the in-repo
  shape.
- Reconnects resume by cursor (`?cursor=`), never replay from zero —
  re-delivery is dedup'd at the apply edge, not skipped by position
  (`ev/no-dedup` for effects that can't tell).
- "No timeout" means no timeout _as the mechanism_ — ceilings stay as
  leak guards (watch TTL, `waitFor` deadline, drain deadline). Deleting
  a ceiling because "events cover it" re-introduces the hang the
  ceiling existed to kill.

## Posture source — Wright's essays (singulargrit.substack.com)

Craig's Substack is paywalled essays — legal/economic/institutional
pieces, not engineering specs. They ground WHY the rules exist; they do
NOT define mechanical behavior — cite them as posture, never as the
authority for a `fab/*`/`ev/*` finding (that's what papers/BRCs are
for). The recurring themes that map onto this layer:

- **"The Promise Needed an Address"** — a promise is enforceable only
  when its owner is locatable → identity→locator binding is load-bearing
  (the bfinger committed record: hosts/office/stream/watch + retraction).
- **"Truth Is Expensive. Bullshit Scales"** — claims are cheap;
  verification is everyone's problem → push verification to the edge
  (readers verify, SPV at the subscriber); a system that makes truth
  expensive to check will be fed lies.
- **"The Ledger Has Changed. Someone Still Owes You"** — infrastructure
  upgrades don't discharge obligations → persisted state outlives the
  system that wrote it (migrate lane tables, honor tombstones, keep
  wire formats readable).
- **"The Agent Has Permission. Who Pays for the Mistake?"** — delegated
  authority needs declared bounds → SpendGuard caps, budget authorities,
  signed scope in envelopes.
- **"Your Reputation Is Excellent. It Is Also Rented"** — platform-held
  standing isn't portable → records must be self-verifying and
  host-portable; a reader shouldn't need the issuing host to confirm.
- **"The Debt Was Due. The Silver Was Not"** — legal validity ≠
  existence → a claim about a UTXO isn't the UTXO; verify existence
  (proof/spendability), not the document describing it.
- **"The Obligation Survived. Who Held the Power?"** — being owed ≠
  controlling → distinguish holding a claim from holding the key that
  discharges it (ownership = unlock capability, not receipt).

Essay text is subscriber-gated — the audit may cite titles/themes but
must not quote or paraphrase bodies beyond the public previews.

## The one-line summary

Signal is instant (push/hint), truth is verified (proof/observe),
recovery is designed (floor + wake + bounded ceilings), and the wire is
boring (versioned envelopes, digest references, vector-proven). Audit
findings always point at which of the four is missing.
