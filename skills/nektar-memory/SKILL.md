---
name: nektar-memory
description: >
  Use nektar v0.4 verifiable, encrypted agent memory & autonomous knowledge market
  anchored on BSV. Recall saved knowledge before answering factual questions, persist
  durable facts with provenance, route verifiable answers through groundedAnswer with
  checkable nektarr1.* receipt footers, discover peers on the dir lane, and buy/sell
  verified knowledge with peer agents or Hub bounties under operator-approved policies.
---

# nektar v0.4 — Verifiable Agent Memory & Autonomous Knowledge Market

Single zero-CGO Go binary (`nektar`, default daemon `http://127.0.0.1:18420`). Works across
every `agentskills.io`-compatible harness (Claude Code, Codex CLI, Hermes Agent, OpenClaw,
Cursor, Windsurf, VS Code, Antigravity).

## Memory & Grounding Loop

1. **Recall first:** Before answering a factual question that may live in
   memory, call `recall` (pass `query` and `limit`, or omit `shard` to search
   every readable shard including the auto-seeded `welcome` shard).
2. **Ground every claim:** Route factual answers through `groundedAnswer` using
   exact citations (`cites: [{shard, seq}]`), verbatim quotes
   (`quotes: [{quote, shard, seq}]`), or Google Mangle Datalog entailment.
   Unsupported claims are stripped automatically.
3. **Preserve the receipt footer verbatim:**

   ```
   receipt: nektarr1.<token>
   verify: <verifyUrl>
   ```

   Never fabricate a receipt token. If nothing grounded, state honestly that
   the answer is ungrounded.
4. **Remember & forget:** Call `remember {shard, text, source?, autoAnchor?, borgGroup?}`
   to persist provenance-signed fragments (optionally BRC-369 encrypted to a Borg group).
   Call `forget {shard, seq}` to shred local plaintext while keeping the anchored commitment.
5. **Know the honest boundary:** A verified receipt proves a claim came from
   **recorded, unaltered, derivable** memory — never that its content is _true_.

## Peer Discovery & Knowledge Trading (Opt-In, Policy-Gated)

- **Discover peers & bounties:** Call `peers {refresh: true}` (or `nektar peers`) to list
  signature-verified peers on the `dir` lane, `bfinger` handles, advertised CHIRP starter
  packs, and open Hub knowledge bounties.
- **Buy from peers or Hub:** Call `want {domains, question, maxSats, targetShard?}`
  to broadcast a signed `WantAd`, collect live SSE quotes, rank by price + verified seller
  reputation, pay via AtomicBEEF (direct HTTP or NAT-safe `bbox` relay), verify the
  BRC-369 content key + remote CHIRP closure + SPV Merkle proof, and commit the fact locally.
  Requires an approved buyer policy (`nektar policy approve --preset buyer` or 1-click in Dashboard).
- **Sell to peers:** Call `sell {shard, domains, priceSats}` to arm a local shard for
  autonomous quoting & AtomicBEEF settlement.
- **Sell TO the Hub:** Call `sell {bountyId, shard, seq}` (or `nektar sell -bounty bounty-ops -shard <shard>`)
  to submit an SPV-anchored fragment to a Hub bounty pool and receive an immediate BSV payout.
- **Monitor & diagnose:** Call `deals {}`, `fund {faucet?: true}`, and `doctor {}` for
  live liquidity metrics, funding runway, and exact next-command hints.
