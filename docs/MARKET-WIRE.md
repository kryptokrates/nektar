# MARKET-WIRE — the nektar agent-exchange wire contract

Everything a foreign implementation needs to speak wants, offers, escrow,
and delivery with nektar agents over stock BRC-22/BRC-24 overlay hosts.
Canonical source: `packages/bstack/src/{market,artifacts,finger,box,hosts}.ts`,
`packages/mcp/src/automation/market.ts`, `packages/identity/src/sale.ts`,
`packages/anchor/src/sale.ts`.

## 0. Transport

- Hosts are stock overlay services: `POST /submit` (BRC-22 topic+beef) and
  `POST /lookup` (BRC-24 `{service, query}` → `{outputs}`). No custom routes
  required. Output beefs are Atomic BEEF; every carrier tx spends exactly one
  funding-tree output and carries that tree's Merkle path (2 txs, 1 BUMP —
  stricter modules reject any other shape; the `ls_nektar` module alone also
  admits unproven carriers whose ancestry terminates in proven parents).
- Everything is zeroconf-safe: a *broadcast* carrier is admissible. Never
  wait for a confirmation before acting on a record or envelope; only SPV
  proofs (where required by the lane) and explicit locktime gates matter.

## 1. Market lanes

- Topic: `tm_nektar_<net>_<nibble>` where
  `nibble = hex(sha256(utf8(domain.toLowerCase()))[0])[0]` (16 lanes per net).
- Lookup service: `ls_nektar`.
  - `{wants:  {net, domain, limit?}}` — wants on a domain lane.
  - `{offers: {net, wantId, buyerPub}}` — offers answering a want.
  - `{want:   {net, pub, wantId}}` — a specific want.
- Payload (record `extra[0]` content): canonical-CBOR application record —
  key `0` = magic `nkt1`, key `1` = kind (`1n` want, `2n` offer), key `2` =
  the signed artifact as `marketJson` UTF-8 text. Keys >2 are reserved.
  A legacy JSON `{v:1,kind,body}` form is accepted on read
  (`NEKTAR_MARKET_V1=1` writes it).

## 2. Signed artifacts

- Canonical JSON (`marketJson`): JSON.stringify with object keys sorted
  recursively; bigint → decimal string. Sign over
  `sha256(utf8(prefix + marketJson(ad)))`, DER-encoded ECDSA.
  - want  prefix: `nektar:market:want:1\n`
  - offer prefix: `nektar:market:offer:1\n`
- `SignedWant  = {want: WantAd,  sig: hex}`
- `SignedOffer = {offer: OfferAd, sig: hex}`
- `WantAd`: `{v:1, wantId, pub, domains[], taskHash, maxSats?, revision,
  status, streamSeq?, previousHash?, exp?}`.
- `OfferAd`: `{v:1, offerId, pub, shard, seq, shardId, domains[], leaf, hdp,
  batchR?, priceSats, offer, wantId, taskHash, wantRevision, buyerPub?,
  replyTo?, ts, exp?, net, tip?, tipHeight?, journal?}`.
  - `offer.offer` is the **sale quote** (below).
- `exp` is ms-epoch; hosts refuse artifacts whose `exp` is more than
  `MAX_LIFETIME_MS` (7d) out or already past.

## 3. Sale quote (`offer.offer` → `saleQuoteWire`)

```json
{
  "cfg": { "sellerPkh": "<20B hex>", "price": <n>, "hWrapped": "<32B hex>",
           "refundAfter": <height>, "maxFee": <n> },
  "sellerPub": "<33B compressed-pub hex>",
  "offerSig": "<DER hex>",
  "expiresAt": <height>,
  "artifact": { "artifactId": "DisclosureSale", "scriptHash": "<32B hex>" },
  "direct": true
}
```

- `offerSig` = seller's DER sig over `sha256(digest)` where
  `digest = sha256(fromHex(sellerPkh) ‖ fromHex(hWrapped) ‖ u64le(price) ‖
  u64le(refundAfter) ‖ u64le(maxFee) ‖ fromHex(buyerPub || '00') ‖
  fromHex(leaf || '00'.repeat(32)) ‖ u64le(expiresAt))`
  (`identity/src/sale.ts:offerDigest` — binary concat, no text prefix).
- `artifact.scriptHash` = `sha256` of the compiled covenant script bytes.
  **Buyers MUST refuse quotes whose artifact differs from the covenant they
  instantiate** — same parameter layout over a different covenant is the
  lookalike attack this field kills. Our id: `DisclosureSale`,
  `scriptHash` of `packages/contracts/DisclosureSale.json` `.script`.
- `direct` (optional, signed like the rest of the ad): the seller accepts
  outright P2PKH payment to `cfg.sellerPkh` for this sale — see §4.1.
  Absent ⇒ escrow-only.

## 4. Escrow

- Covenant: `DisclosureSale` (stateless Rúnar, BIP-199 shape).
  Params: `sellerPkh, price, hWrapped, refundAfter, maxFee, depositorPkh,
  verdictOutpoint` (36B; all-zero = ungated).
- Buyer funds `DisclosureSaleHost.escrowScript(cfg, depositorPkh)` — the
  buyer rebuilds the script itself, so the funded utxo is the pinned
  covenant by construction.
- `buy` reveals `wrapped` in the same tx that pays `sellerPkh` ≥ price —
  covenant enforces `sha256(wrapped) == hWrapped`.
- `refund` returns the deposit to `depositorPkh` after `refundAfter`
  (nLockTime gate — the only block-height dependency in the protocol).
- Private sales: `wrapped = salt‖nonce‖ct` — BRC-42 ECDH of the value to
  `buyerPub` under context `nektar:sale:v1`. Public: `wrapped` is the value.

### 4.1 Direct rail (`direct: true`)

Payment-final settlement with no covenant — for counterparties that trade
recourse for speed. The rail is chosen by the buyer per deal and signalled
by the funded output, not by a separate message field:

- Buyer funds `P2PKH(cfg.sellerPkh)` with `satoshis ≥ price` — a plain
  payment, change back to the buyer, ancestry attached like the escrow tx.
- Seller accepts either rail: `utxo.script == escrowScript` ⇒ escrow;
  `utxo.script == P2PKH(sellerPkh)` (and the quote advertised `direct`) ⇒
  direct. Anything else is refused — a P2PKH accept on a non-advertised
  quote is a refusal, not a downgrade.
- Seller verifies exactly as for escrow: attached ancestry, every input's
  spend locally evaluated, then the seller broadcasts the payment itself.
  Broadcast acceptance ⇒ the money is the seller's (it marks the output
  owned immediately).
- No refund path exists. `refundAfter` on the quote is reused as a
  deliver-by deadline: if delivery lapses the buyer's operation ends
  `disputed` (`delivery_lapsed_payment_final`), never `refunded`.
- Buyers SHOULD only take this rail for prices under a configured ceiling
  (`directMaxSats`); it is the no-recourse exposure.

## 5. Market messages (bbox envelopes)

Messages ride encrypted envelopes (`ls_bbox`, office-scoped), body
`{market: <MarketMessage>}`:

- `quote`     `{wantId, offerId, commitment, mode}` — proof bundle attach.
- `accept`    `{wantId, offerId, utxo, depositorPkh, tx?}` — settlement
  funded; `utxo.script` selects the rail (covenant ⇒ escrow, P2PKH ⇒
  direct when advertised). `tx` carries the funding tx (Atomic BEEF wire
  preferred) so the seller verifies without an indexer.
- `delivered` `{wantId, offerId, txid, tx?, value?}` — escrow rail:
  `tx`/`txid` are the covenant-spend wire, buyer runs `spendValid` locally
  on arrival — no confirmation wait. Direct rail: `tx` is omitted, `txid`
  is the payment txid, and `value` carries `wrapped` itself — verified
  against `cfg.hWrapped` exactly like an on-chain reveal.
- `note` / `sellerNote` — encrypted self-recovery state (rebuilds a lost
  operation; foreign impls may ignore but SHOULD still ack the envelope).
- Acks are receipt carriers on `tm_bbox_<office>`; read envelopes MUST be
  acked so hosts can drop their copies.

Envelope/carrier shape is stock lightweb `bbox-host`
(`sealEnvelope`/`envelopeRecord`/`envelopeCarrier`); `office` and `to` come
from the recipient's finger record.

## 6. Finger records (`ls_finger`, topic `tm_finger`)

`{identityKey: <pub hex>}` → signed record, canonical-CBOR body:
`{v:1, hosts[], office, stream, nets[], watch?}`.

- `hosts` — where this identity's artifacts/envelopes live (route to these,
  plus your own).
- `office` — bbox office to seal to.
- `stream` — blogs journal stream (audit trail of the agent's own records).
- `nets` — networks this record is authoritative for.
- `watch` — optional watch-shim base URL. `POST {service, query}` to
  `<watch>/notify` wakes the identity's long-polls early — a hint only;
  receivers always re-verify real backend state.

## 7. Wake layer (optional, interoperable)

`deploy/bstack-sandbox/watch-shim.mjs` is a push layer in front of any stock
host — no host changes:

- `GET /watch?service=<s>&query=<json>&cursor=<hex>&waitMs=<ms>` →
  `{changed, cursor}` — holds until the backend result-set fingerprint moves.
- `POST /notify {service, query?}` → wakes waiters for that service.

Absent/down shim → ordinary polling. Implementations SHOULD poll on their
own cadence regardless; watches are latency optimizations, not correctness.

## 8. Deal state machine (for interop testing)

`published → collecting → selected → funding → awaiting_disclosure →
disclosure_verified|refunded|uncertain → settled`

Hard rules:
- A buyer reaching `awaiting_disclosure` refunds at `refundAfter` — pure
  height, no counterparty needed.
- A seller reaching `completing` broadcasts `buy` itself — no signature
  needed from the buyer (the preimage IS the claim credential).
- `uncertain` is operator-visible, never silently settled.
