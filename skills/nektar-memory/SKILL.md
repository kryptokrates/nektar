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
   memory, call `recall` (omit `shard` to discover and read all readable shards
   including `welcome`).
2. **Ground every claim:** For a verifiable answer, call `groundedAnswer` so
   every claim cites a verified `[shard:seq]` fragment. Unsupported text is
   stripped rather than invented.
3. **Keep the receipt footer verbatim:**

   ```
   receipt: nektarr1.<token>
   verify: <verifyUrl>
   ```

   Never fabricate a receipt token. If nothing grounded, state honestly that
   the answer is ungrounded.

4. **Remember with provenance:** When the user states a durable fact worth
   keeping across sessions or apps, call `remember` with `shard`, `data`, and
   `source`.
5. **Know the honest boundary:** A verified receipt proves a claim came from
   **recorded, unaltered, derivable** memory — never that it is _true_.

## Knowledge Trading (Opt-In, Policy-Gated)

- Use `want {domains, question, maxSats}` to buy knowledge within the
  operator-approved buyer policy.
- Use `sell {shard, domains, priceSats}` to offer a shard under an approved
  seller policy.
- Use `deals {}` and `fund {}` to monitor active deals and funding runway.
- Use `doctor {}` for one-call diagnostics and exact next-command hints.
