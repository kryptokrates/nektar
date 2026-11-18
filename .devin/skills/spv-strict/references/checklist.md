# checklist — third-party verification, mapped to this repo

Paper 011 §7.3 fixes the order of a third-party check at eleven steps.
"Nine are arithmetic on bytes already in hand; one is the host's answer
and one asks the header sources you chose." The order is deliberate:
the name first, content before proof, refused for the first rule
broken.

| #   | Check (paper 011 §7.3)                                             | Uses               | Here                                                                        |
| --- | ------------------------------------------------------------------ | ------------------ | --------------------------------------------------------------------------- |
| 1   | the host answered exactly one output for the name asked            | host's answer      | `lookupAll` / `readItems` — several hosts, compared; `withheld` bookkeeping |
| 2   | the BEEF parses within size bounds and holds one carrier           | the BEEF           | `checkCarrier` (`MAX_CARRIER_BEEF_BYTES`), vendored `admit`                 |
| 3   | the carrier's txid is the name you asked for                       | carrier bytes      | digest-addressed lookups; `artifactHash`                                    |
| 4   | the payload follows its application's rules                        | payload            | `decodePayload`, `wantShapeError` / `offerShapeError`                       |
| 5   | the carrier is unmineable: lock time ≥ 2100, every input not final | two fields         | `mineableRefusal`                                                           |
| 6   | the unlocking script is exactly one canonical signature push       | the input          | `unlockingRefusal`                                                          |
| 7   | the locking key is derived from the identity the payload names     | identity key, lock | `carrierLockingKey(bodyPub(p)) === lockingKey`                              |
| 8   | the field signature verifies over SHA-256 of the payload           | key, payload       | `verifyBody` → `verifyWant` / `verifyOffer`                                 |
| 9   | the application's own expectations (kind, identity)                | payload            | `o.accept(p)`, net check                                                    |
| 10  | the input satisfies the funding output it spends                   | carrier, F:i       | `spendValid`, script evaluation in `admit`                                  |
| 11  | the funding tree's merkle path lands on a root in your headers     | path, headers      | `provenParent` → `merklePath.verify(txid, tracker)`                         |

"No answer from the header sources is an error, never a verdict"
(step 11). A header source that cannot answer leaves the item
unverified and retried; it is not a refusal and not an acceptance —
beefcat's exit-1 ("no verdict") is the reference behaviour: an outage
is never reported as a forgery, and a forgery never as an outage.

Two traps the steps alone do not state, both named by
lightwebinc/beefcat's `docs/verification.md` (the reference
implementation of this checklist):

- **Anchored ≠ resolvable (beefcat check 9).** A transaction that
  spends a mined coin SPV-verifies _through that coin_ even if the
  transaction itself was never broadcast — a sender could sign postage
  and never pay for it. Steps 2–11 must require the funding parent to
  carry its own BUMP in the BEEF, not merely be resolvable or
  broadcast. Here: `provenParent` demands the path; an accept that
  settles for "the parent exists on-chain" is `spv/thin-ancestry`.
- **Freshness is not proven.** A verified carrier proves the sender
  signed the data at some time — a replayed older-but-complete object
  verifies again. Freshness lives in the payload (expiry, sequence,
  challenge), not in the proof. Here: `verifyBody(p, o.now)` expiry on
  wants/offers, claim `seq` numbering, `until` windows — any accept
  path without a freshness field is the gap.

## What the checklist does not cover (paper 011 §8)

1. **Unspent-ness.** A retracted carrier served from a stale copy still
   passes steps 2–11. Seen only by a host that watches the funding tree
   or a spend check.
2. **Withholding.** Ask more than one host; refuse answers that
   disagree.
3. **Header honesty.** PoW-check, several sources, never the answerer.
4. **Truth of the content.** A signature proves who said it.
5. **Cost accounting.** The proof says the sender's key paid to exist
   on-chain; a receiver keeping no memory cannot tell whether an output
   backed an earlier stream to someone else.

## Audit questions per step

- Is step 1 really plural — do we compare hosts, or take the first?
- Are steps 2–9 run before any header lookup (content before proof)?
- Does a positive-verdict cache key on the bytes, and re-apply every
  time-dependent gate (expiry) on a hit?
- For step 11, which tracker is passed, and is it the witnessed one?
- When step 11 cannot be answered, does the item stay retryable, or is
  it dropped / accepted?
- For our own carriers: is the tree handed to `buildCarrier` proven or
  above a path that is actually present in the bytes — and when the
  chain runs past one hop, does every ancestor in the BEEF terminate at
  a BUMP within the ancestor-policy bound?

## Unconfirmed chains (zeroconf accepts)

When the accepted object is not the carrier pattern but a BEEF
ancestry chain — counterparty or first-party — the eleven steps become:

1. Every ancestor's bytes carried (txid recomputed, input linkage
   verified) — an absent ancestor is an unverified hop, not a detail.
2. Every input chain terminates at a BUMP-anchored mined parent, OR the
   unconfirmed tail is within the named policy bound for that issuer
   class (first-party → policy cap; counterparty → ancestor + value
   caps, escalation on breach).
3. The BUMPs still anchor to OUR headers (step 11 unchanged — the
   terminal parents are just further back).
4. A conflict watch is registered for counterparty chains that advance
   the deal: the trip wire is the conflicting transaction's own mined
   BUMP or the `abandoned` event, and what it triggers is escalation —
   never an automatic "rejected" verdict, since no proof of rejection
   exists.

## Mined data (the short list)

For data whose own transaction is mined: the txid, the transaction's
own merkle path, the signature, and the payload's rules (paper 011
§7.3, last paragraph). Here: `TxStore.read(wire, tracker)` and
`proofLookup`.
