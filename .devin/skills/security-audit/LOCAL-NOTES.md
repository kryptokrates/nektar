# Local operating notes — security-audit on nektar

Additive notes for running this vendored skill in this repo; survives skill
updates. The skill itself is authoritative for workflow; these are the
repo-specific friction points found in `run-1` (2026-11).

## Windows: validators need WSL

`tools/validate_*.cjs` open ledgers with `O_NOFOLLOW|O_NONBLOCK` — Windows
Node doesn't expose them and there is no bypass flag (it's the skill's
safe-input stance). Run under WSL from the repo root:

```bash
wsl -e bash -lc "cd /mnt/c/Users/nedzo/security-audit-skill/nektar/run-1 && node <path-to-skill>/tools/validate_ledger.cjs coverage-ledger.json"
```

Git-bash mangles `/mnt/...` paths when passed as args — wrap the whole
command in `wsl -e bash -lc "…"`.

## Large verifier records land in overflow files

Verifier agents are read-only — they can't write `decisions/` files. When a
record is big enough that the completion message truncates, the full text is
in `%TEMP%\devin.exe-overflows\*\content.txt` (newest dir). Extract records
from there rather than resuming agents; merge into `findings.json` by script
(decisions are fingerprint-sorted arrays).

## Coverage sizing for this repo

Quick profile ≈ **10 coverage units** across ~70k src LOC (more dilutes the
verifier pass). Deferred units are legitimate output — record them in the
ledger with `status: "deferred"`, `agent_id: null`, and disclose them in the
report; do not silently drop coverage.

## Docs to skip for nektar (a TS daemon, no UI)

- `MEMORY-SAFETY-AND-BINARY.md` — no native code paths
- `DESKTOP-MOBILE-AND-LOCAL-IPC.md` — no IPC surface
- `CLIENT-SIDE.md` — browser-class issues only

Core loads: `ATTACK-CLASSES.md` (canonical class names for ledger refs),
`RECONNAISSANCE.md`, `HUNTING.md`, `VALIDATION-AND-REPORTING.md` +
`report-schema.json`. The HTTP/auth/resource-exhaustion/supply-chain docs
map to real surfaces here.
