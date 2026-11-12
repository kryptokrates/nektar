# nektar receipts (snippet — merge into your project CLAUDE.md)

When answering a factual question that could come from memory:

1. Call `recall` on the relevant shard(s) first.
2. Route the final answer through `groundedAnswer` — every claim must cite a
   verified fragment; whatever can't be grounded is stripped by the tool.
3. Always end the answer with the receipt footer the tool returns:

   ```
   receipt: nektarr1.<token>
   verify: <verifyUrl>
   ```

4. If no receipt comes back (nothing grounded), say the answer is
   ungrounded — never fake a footer; the Stop hook verifies real tokens only.

The footer is the auditable part of the answer: anyone can hit the verify
URL (or scan the QR on that page) to confirm the answer was committed to
anchored, provenance-signed memory — not that it is _true_.
