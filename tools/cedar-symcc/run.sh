#!/bin/sh
# Runs inside rust:1-slim (see `pnpm cedar:symcc`). Fetches cvc5 1.3.1 (static),
# builds the checker, proves the instance policy and requires the mutant to fail.
set -eu
apt-get -qq update >/dev/null && apt-get -qq install -y curl unzip >/dev/null
mkdir -p /opt/cvc5 && cd /opt/cvc5
curl -sSL -o cvc5.zip https://github.com/cvc5/cvc5/releases/download/cvc5-1.3.1/cvc5-Linux-x86_64-static.zip
unzip -q -o cvc5.zip && export CVC5="$(find /opt/cvc5 -type f -name cvc5 | head -1)"
"$CVC5" --version | head -1
cd /w && cargo build -q --release
B=/tmp/t/release/cedar-symcc-check
echo "== instance policy"; $B policies/nektar.cedarschema policies/instance.cedar policies/isolation-spec.cedar
echo "== mutant policy (must be REFUTED)"
if $B policies/nektar.cedarschema policies/mutant.cedar policies/isolation-spec.cedar; then echo "MUTANT NOT CAUGHT"; exit 1; fi
echo "SYMCC GATE PASS"
