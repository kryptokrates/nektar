# Claude Code — auto-verified answer receipts

Every answer your agent gives gets a one-line receipt footer; a Stop hook
checks it automatically on the fly — no trust, no discipline required.

```
receipt: mamemr1.8vK3…   verify: http://localhost:8080/v1/verify?r=mamemr1.8vK3…
```

## Setup (3 steps)

1. **Run the mamem HTTP server** (the verify endpoint lives there):

   ```sh
   MAMEM_HTTP_PORT=8080 pnpm --filter @mamem/mcp start   # /mcp + /v1/*
   # hosted? set MAMEM_VERIFY_BASE=https://your-host so footers/QRs point out
   ```

2. **Register the MCP server** (Claude calls the tools):

   ```sh
   claude mcp add mamem --transport http http://localhost:8080/mcp \
     --header "Authorization: Bearer <token if MAMEM_HTTP_TOKEN set>"
   # or stdio — see docs/INTEGRATION.md
   ```

3. **Install the hook + instruction** (auto-check on every answer):

   ```sh
   # merge this dir's settings.json into .claude/settings.json, and the
   # CLAUDE.md snippet into your project CLAUDE.md
   ```

   In this repo it is already wired — `.claude/settings.json` registers the
   `Stop` hook `integrations/claude-code/verify-receipt.mjs`.

## What the hook does

On every Stop it scans the last assistant message for `mamemr1.` tokens and
GETs `/v1/verify` for each (3s timeout, max 3, fail-open if the endpoint is
down):

- **✓ pass** → quiet (visible in verbose output)
- **✗ fail** → *blocks the stop once* and hands the failure back to Claude,
  which must tell you which check failed — the failure can't slide by
- **no token** → silent; nothing to check

The QR on the `/v1/verify` page (`?format=html`) encodes the same check —
scan it on a phone to re-verify a receipt anywhere the server is reachable.

## The honest boundary

A verified receipt proves the answer was **committed to anchored,
provenance-signed memory and passed the grounding gate** — its cites resolve
to sealed records. It does *not* prove the answer is **true**, that Claude
*called* the gate before answering (a skipped `groundedAnswer` yields no
token at all — which the hook treats as "nothing to check", not as a pass),
or that the model ran the declared weights (see `modelManifest` — declared
identity, not zkML).
