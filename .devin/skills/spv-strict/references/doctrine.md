# doctrine — what SPV proves, per source

Each entry: the statement, where it is made, and what it demands here.
Paper numbers are 1BSV/Lightweb papers; "Wright" is
singulargrit.substack.com.

## D1 — Three things suffice: bytes, a merkle path, headers

_Paper 011 short version + §2; whitepaper §8; Wright, Formal Treatment
§3 "Merkle Proofs and Header Chains"._ A transaction is verified by
hashing its bytes to a txid, hashing up the path, and comparing with
the root in a header the verifier already holds. No scripts replayed
for the mined tx, no blocks, no global state.

Demands: every 'mined' claim is that computation, done by us. A status
string, a `blockheight > 0`, a "known" flag is not it.

## D2 — Headers are the only act of trust; spread it and check the work

_Paper 011 §2.1, §8 item 3, §9 limit 5._ "Choosing where your headers
come from, and checking their work, is the only act of trust in SPV."
Take them from several independent sources, accept only the chain they
agree on, and "never take them from whoever answered your question".
_Paper 008 §4:_ headers are "chained and proof-of-work checked on
arrival"; a root answered from a fallback anchor is "reported apart
from one received on the lane". _Wright, Verification Without
Enforcement:_ the SPV client verifies header-chain weight and merkle
inclusion; an eclipsed client sees only what it is fed — hence more
than one source.

Demands: PoW and linkage checked locally; at least two independent
sources that must agree; the header source is never the proof's
provider; corroborated and uncorroborated answers are distinguishable.

## D3 — Evidence travels with the transaction

_Paper 011 §2.4:_ "A verifier should not have to go looking for parents
and paths … With the headers you already hold, a BEEF is complete:
nothing else is fetched." _Paper 006:_ "no proof data is ever
stripped". _Wright, The Protocol No Gatekeeper Wants:_ the recipient
needs the transaction, its signatures and conditions, an inclusion
proof and a chain of PoW headers; "Evidence should travel with the
transaction. Verification should be local to the relationship";
sender-held proofs. _Wright, A Mechanism of Honour:_ the user
"maintains a copy of their transaction" and "the Merkle path to the
block header".

Demands: whoever holds a proof hands it on. An internal handoff that
drops a held merkle path (wallet → funding → tree mint) is the same
defect as an external one: the receiver must go back to the network
for what the sender had.

## D4 — Verifiable does not mean mined: carried ancestry is the zeroconf proof

_Paper 011 short version, §2.3, §3.2:_ "A transaction that is never
mined is still verifiable when it spends an output of one that was."
The carrier's proof is its parent's merkle path plus one script check.

Demands: an unmined transaction is provable exactly as far as its
carried ancestry reaches merkle paths. The general form is the BEEF
ancestry chain: every input's source transaction carried, each checked
(txid recomputed from bytes, input linkage, signatures where the flow
moves value), and every chain terminating at a BUMP-anchored mined
parent. One unconfirmed hop is the degenerate case, not the bound —
the bound is the ancestor-policy limit (chain length, total
unconfirmed value) the verifier sets. An ancestry that ends in unmined
transactions with uncarried inputs proves nothing past the verified
prefix.

Two risk classes, priced differently:

- **First-party ancestry** — ancestors we issued (our funding trees,
  our carrier chains, our own change). Only we can double-spend them,
  so verified linkage plus our own spend records is the whole check;
  chains may run to the policy cap.
- **Counterparty ancestry** — unconfirmed ancestors the peer issued.
  Verified ancestry proves provenance, not exclusivity: the issuer can
  still double-spend. Bound it — ancestor-chain cap, unconfirmed-value
  cap — and escalate toward mined-depth requirements as value grows
  (D9), with conflict watches (a conflicting spend's own BUMP, the
  `abandoned` event) as the trip wire.

Zero-conf acceptance is a progression decision, never a settlement
decision: verified carried ancestry advances want → offer → escrow →
disclosure; value-final state still waits for depth (D7). This is the
only legitimate "zero-conf": carried, checked ancestry within a named
policy — never an early belief, and never a wait for a block the
ancestry already made unnecessary.

## D5 — Postage is paid once, ahead of use

_Paper 011 §6:_ "One mined funding tree with M small outputs pays one
miner fee, and each output admits exactly one carrier." _Paper 009:_
"The patterns do not wait for the network." A funding tree is mined
before the carriers that spend it exist.

Demands: the block a funding tree needs is paid at provisioning or
refill, not inside a deal. A lane with no proven slot while the agent
has funds is a stocking failure.

## D6 — Proof updates are first-class

_Paper 006:_ "a payment published at send time and the same payment
republished with its refreshed BUMP once mined are two legitimate,
distinct objects. Subscribers see the payment instantly and its mined
proof" later.

Demands: act on the unproven object for what it can prove (signature,
ancestry), and attach the BUMP when it arrives. Never hold the first
until the second exists unless the step moves money.

## D7 — Depth is the security parameter, for money

_Wright, Formal Treatment §5:_ "By waiting for k confirmations, the SPV
client exponentially reduces the probability" of reversal, roughly
`(q/p)^k`. _Paper 009 limit 6:_ a reorganisation re-anchors proofs; it
does not fork state.

Demands: value-final decisions carry an explicit depth; everything else
does not wait for depth. On a reorg, proofs at or above the fork are
dropped and re-verified.

## D8 — What SPV cannot tell you

_Paper 011 §8, §9 limit 4._ (1) Whether an output is still unspent —
"SPV proves that a parent was mined, not that its output is unspent
today". (2) Whether a host is withholding — ask several and compare.
(3) Whether your headers are honest — see D2. (4) Whether the data is
true.

Demands: spentness is a watch/interest question answered by first-party
knowledge (our own spends) or an explicit spend check — never inferred
from SPV, and never from an index treated as truth. Silence from a host
is not absence.

## D9 — Risk-based escalation, not blanket waiting

_Wright, The Audit of Fools:_ an SPV client "requests targeted evidence
about the flows that matter to it. If the received proofs meet
tolerances, confidence is sufficient. If anomalies appear — missing
headers, inconsistent proofs, unexplained gaps — then escalation
occurs." _Formal Treatment §4 "Adaptive Polling Intervals":_ poll
faster during rapid growth or suspected reorgs, slower in stability.

Demands: the default path is fast and proof-carrying; waiting, deeper
proofs and extra sources are the escalation path, entered on a named
anomaly. For zeroconf specifically: first-seen acceptance plus a
conflict watch is the default risk posture for first-party chains and
small counterparty ones; what escalates to mined-depth requirements is
a named anomaly — an unverified ancestor, a conflicting spend, value
over the zeroconf cap, a chain past the ancestor bound — never the
clock alone.

## D10 — Proofs are a provider-tier product; apps take objects + headers

_Paper 006:_ application accounts receive "BEEF topics plus the
block-header lane, and never the raw transaction" stream. _Paper 007:_
merkle proofs come from a proof service that ingests subtrees.

Demands: app code never scans blocks or rebuilds proofs on a live path;
it receives them and checks them.
