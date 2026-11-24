---
name: nektar-memory
description: >
  Use nektar verifiable, encrypted agent memory anchored on BSV. Recall saved
  knowledge before answering factual questions, persist durable facts with
  provenance, route verifiable answers through groundedAnswer with checkable
  receipt footers, and optionally buy/sell knowledge under operator-approved
  policies.
---

# nektar — Verifiable Agent Memory & Knowledge Market

Works across every `agentskills.io`-compatible harness (Claude Code, Codex CLI,
Hermes Agent, OpenClaw, Cursor, Antigravity).

## Memory & Grounding Loop

1. **Recall first:** Before answering a factual question that may live in
   memory, call `recall` (pass `query` and `limit` for fast targeted lookup, or
   omit `shard` to discover readable shards including `welcome`). Default
   `recall` returns active (non-superseded, non-forgotten) memories and works
   locally even before BSV anchoring (`verification: "local-committed"`).
2. **Ground every claim:** For a verifiable answer, call `groundedAnswer` using
   exact quotes (`cites: [{shard, seq}]`), quote-backed synthesis
   (`quotes: [{quote, shard, seq}]` with verbatim substrings from cited
   fragments), or Datalog entailment. Unsupported claims are stripped rather
   than invented.
3. **Keep the receipt footer verbatim:**

   ```
   receipt: nektarr1.<token>
   verify: <verifyUrl>
   ```

   Never fabricate a receipt token. If nothing grounded, state honestly that
   the answer is ungrounded. When unfunded, receipts are locally signed
   (`anchored: false`); funding the fee key anchors them on BSV.

4. **Remember, supersede, and forget:** Call `remember` with `shard`, `data`,
   and `source` (plus optional `supersedes: {shard, seq}` to replace an older
   fact in active views). Call `forget {shard, seq, reason?}` to append a
   tombstone hiding an outdated or wrong record from active views while keeping
   full append-only history (`includeHistory: true`).
5. **Know the honest boundary:** A verified receipt proves a claim came from
   **recorded, unaltered, derivable** memory — never that it is _true_.

## Knowledge Trading (Opt-In, Policy-Gated)

- Use `want {domains, question, maxSats}` to buy knowledge within the
  operator-approved buyer policy.
- Use `sell {shard, domains, priceSats}` to offer a shard under an approved
  seller policy.
- Use `deals {}` and `fund {}` to monitor active deals and funding runway.
- Use `doctor {}` for one-call diagnostics and exact next-command hints.
