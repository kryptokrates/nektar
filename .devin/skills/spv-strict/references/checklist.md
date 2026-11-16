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
unverified and retried; it is not a refusal and not an acceptance.

## What the checklist does not cover (paper 011 §8)

1. **Unspent-ness.** A retracted carrier served from a stale copy still
   passes steps 2–11. Seen only by a host that watches the funding tree
   or a spend check.
2. **Withholding.** Ask more than one host; refuse answers that
   disagree.
3. **Header honesty.** PoW-check, several sources, never the answerer.
4. **Truth of the content.** A signature proves who said it.

## Audit questions per step

- Is step 1 really plural — do we compare hosts, or take the first?
- Are steps 2–9 run before any header lookup (content before proof)?
- Does a positive-verdict cache key on the bytes, and re-apply every
  time-dependent gate (expiry) on a hit?
- For step 11, which tracker is passed, and is it the witnessed one?
- When step 11 cannot be answered, does the item stay retryable, or is
  it dropped / accepted?
- For our own carriers: is the tree handed to `buildCarrier` proven or
  one hop above a path that is actually present in the bytes?

## Mined data (the short list)

For data whose own transaction is mined: the txid, the transaction's
own merkle path, the signature, and the payload's rules (paper 011
§7.3, last paragraph). Here: `TxStore.read(wire, tracker)` and
`proofLookup`.
