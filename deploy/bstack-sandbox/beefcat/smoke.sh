#!/bin/sh
# beefcat conformance smoke — the reference implementation verifies a stream
# through headers it fetches itself from sources it chooses:
#   phase 1: mint a carrier stream on a local devchain (nothing real)
#   phase 2: verify it against the devchain's own headers   -> expect VERIFIED
#   phase 3: verify it against OUR shim's headers            -> expect REFUSAL
# Phase 3 is the conformance signal: a parent BUMP that is real on devchain is
# foreign to our chain — our shim must answer the /v1/tip + /v1/root contract
# and the reference verifier must refuse what it cannot anchor. An exit 0 in
# phase 3 means either the shim lied or the verifier skipped the anchor check;
# both are failures worth failing loudly about.
# Phase 4 (BEEFCAT_NODE_RPC set): real postage through our chain — the full
# carrier workload on our node, best-effort (needs generatetoaddress).
set -u

SHIM="${BEEFCAT_SHIM:?set BEEFCAT_SHIM (e.g. http://shim:8080)}"
SMOKE_HOME=/tmp/beefcat-smoke
fail=0
say() { printf '%s\n' "$*"; }

say "== phase 1: carrier stream on a local devchain =="
devchain -listen 127.0.0.1:18080 >/tmp/devchain.log 2>&1 &
DCPID=$!
sleep 1
rm -rf "$SMOKE_HOME"
BEEFCAT_HOME="$SMOKE_HOME" beefcat init -devchain http://127.0.0.1:18080 -q || fail=1
BEEFCAT_HOME="$SMOKE_HOME" beefcat fund -n 2 -coinbase -q || fail=1
head -c 65536 /dev/urandom >/tmp/payload.bin
BEEFCAT_HOME="$SMOKE_HOME" beefcat -o /tmp/stream.bcat </tmp/payload.bin || fail=1

say "== phase 2: verify against its own headers (expect VERIFIED) =="
if BEEFCAT_HOME="$SMOKE_HOME" beefcat verify -network regtest -headers http://127.0.0.1:18080 -q /tmp/stream.bcat >/dev/null 2>&1; then
  say "ok   devchain-anchored verify: exit 0"
else
  say "FAIL devchain-anchored verify: nonzero"
  fail=1
fi

say "== phase 3: the same stream against our shim (expect refusal) =="
BEEFCAT_HOME="$SMOKE_HOME" beefcat verify -network regtest -headers "$SHIM" -q /tmp/stream.bcat >/dev/null 2>&1
code=$?
if [ "$code" -eq 0 ]; then
  say "FAIL a devchain-parented carrier VERIFIED against the shim's headers"
  fail=1
else
  say "ok   shim + reference verifier refused the foreign parent BUMP (exit $code)"
fi
kill "$DCPID" 2>/dev/null || true

if [ -n "${BEEFCAT_NODE_RPC:-}" ]; then
  say "== phase 4: real postage through our chain (best effort) =="
  REAL_HOME=/home/beefcat
  mkdir -p "$REAL_HOME"
  CHAIN_ARG="woc:test"
  if [ -n "${BEEFCAT_ASSET:-}" ]; then CHAIN_ARG="asset:${BEEFCAT_ASSET}"; fi
  BEEFCAT_HOME="$REAL_HOME" beefcat init -network regtest -headers "$SHIM" \
    -rpc "$BEEFCAT_NODE_RPC" -chain "$CHAIN_ARG" -q || fail=1
  BEEFCAT_HOME="$REAL_HOME" beefcat status || true
  if BEEFCAT_HOME="$REAL_HOME" beefcat fund -n 2 -coinbase -q; then
    say "ok   funding tree minted on our chain through $SHIM headers"
  else
    say "skip no coinbase mint on this node (generatetoaddress unavailable or pool funding needed)"
  fi
fi

if [ "$fail" -eq 0 ]; then
  say "beefcat conformance: PASS"
else
  say "beefcat conformance: FAIL"
fi
exit "$fail"
