# Claude Code — auto-verified answer receipts

Every answer your agent gives gets a one-line receipt footer; a Stop hook
checks it automatically on the fly — no trust, no discipline required.

```
receipt: nektarr1.8vK3…   verify: http://localhost:8080/v1/verify?r=nektarr1.8vK3…
```

## Setup (3 steps)

1. **Run the nektar HTTP server** (the verify endpoint lives there):

   ```sh
   NEKTAR_HTTP_PORT=8080 pnpm --filter @nektar/mcp start   # /mcp + /v1/*
   # hosted? set NEKTAR_VERIFY_BASE=https://your-host so footers/QRs point out
   ```

2. **Register the MCP server** (Claude calls the tools):

   ```sh
   claude mcp add nektar --transport http http://localhost:8080/mcp \
     --header "Authorization: Bearer <token if NEKTAR_HTTP_TOKEN set>"
   # or stdio — see docs/INTEGRATION.md
   ```

3. **Install the hooks + instruction** (auto-check on every answer):

   ```sh
   pnpm setup:claude            # merges hooks + CLAUDE.md, never clobbers
   # or: node tools/setup-claude.mjs --dir <your-project> [--dry]
   ```

   Wires two hooks into `.claude/settings.json` (absolute paths to this
   checkout): `Stop` → `integrations/claude-code/verify-receipt.mjs` and
   `SessionStart` → `integrations/claude-code/session-start.mjs`. The
   SessionStart hook calls the running instance's `doctor` tool and injects
   one line of memory context ("N records, M anchored, funded/prover status")
   at session open — silently absent when the instance isn't up.

## What the hook does

On every Stop it scans the last assistant message for `nektarr1.` tokens and
GETs `/v1/verify` for each (3s timeout, max 3, fail-open if the endpoint is
down):

- **✓ pass** → quiet (visible in verbose output)
- **✗ fail** → _blocks the stop once_ and hands the failure back to Claude,
  which must tell you which check failed — the failure can't slide by
- **no token** → silent; nothing to check

The QR on the `/v1/verify` page (`?format=html`) encodes the same check —
scan it on a phone to re-verify a receipt anywhere the server is reachable.

## The honest boundary

A verified receipt proves the answer was **committed to anchored,
provenance-signed memory and passed the grounding gate** — its cites resolve
to sealed records. It does _not_ prove the answer is **true**, that Claude
_called_ the gate before answering (a skipped `groundedAnswer` yields no
token at all — which the hook treats as "nothing to check", not as a pass),
or that the model ran the declared weights (see `modelManifest` — declared
identity, not zkML).
