#!/usr/bin/env bash
# nektar agent — one-command install for any bare Linux box with curl.
#
#   curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/agent/install.sh | bash
#
# Installs Docker if missing, fetches compose + env template, generates the
# two secrets, and starts the agent. Unset NEKTAR_MARKET_HOSTS joins the
# baked-in seed hosts for the network — pass it only to pin your own:
#
#   NEKTAR_MARKET_HOSTS=http://host-a:8080,http://host-b:8080 ./install.sh
#
# Optional passthroughs: NEKTAR_PUBLIC_URL, NEKTAR_NETWORK, NEKTAR_SEED_DOMAINS,
# NEKTAR_AGENT_DIR (default ~/nektar-agent), NEKTAR_AGENT_BRANCH.
#
# NEKTAR_NETWORK=main additionally requires NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND
# in the environment — the install refuses to write a mainnet .env without it.
set -euo pipefail

DEST="${NEKTAR_AGENT_DIR:-$HOME/nektar-agent}"
BRANCH="${NEKTAR_AGENT_BRANCH:-master}"
RAW="https://raw.githubusercontent.com/kryptokrates/nektar/$BRANCH/deploy/agent"
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]:-$0}")" >/dev/null 2>&1 && pwd -P || true)"

echo "== nektar agent install =="

# 1 — docker
if ! command -v docker >/dev/null 2>&1; then
  echo "-- installing docker"
  curl -fsSL https://get.docker.com | sudo sh
fi
DOCKER="docker"
docker version >/dev/null 2>&1 || DOCKER="sudo docker"
$DOCKER compose version >/dev/null 2>&1 || {
  echo "docker compose plugin missing — see https://docs.docker.com/compose/install"
  exit 1
}

# 2 — deployment files (use the checkout when present, else fetch)
mkdir -p "$DEST"
if [ -f "$SCRIPT_DIR/compose.yaml" ] && [ -f "$SCRIPT_DIR/.env.example" ]; then
  cp "$SCRIPT_DIR/compose.yaml" "$DEST/compose.yaml"
  cp "$SCRIPT_DIR/.env.example" "$DEST/.env.example"
else
  curl -fsSL "$RAW/compose.yaml" -o "$DEST/compose.yaml"
  curl -fsSL "$RAW/.env.example" -o "$DEST/.env.example"
fi

# 3 — .env: copy once, fill what we can, never overwrite an existing file
cd "$DEST"
if [ ! -f .env ]; then
  cp .env.example .env
fi
set_kv() { # set_kv KEY VALUE — replace the KEY=... line in .env
  if grep -q "^$1=" .env; then
    # awk, not sed: the value is data — a sed replacement interprets '&'
    # (the whole match) and '|' (the delimiter), so a URL carrying either
    # corrupts the line into a malformed export
    awk -v k="$1" -v v="$2" 'index($0, k "=") == 1 {print k "=" v; next} {print}' .env >.env.tmp && mv .env.tmp .env
  else
    printf "%s=%s\n" "$1" "$2" >>.env
  fi
}
while [ $# -gt 0 ]; do
  case "$1" in
    --domain) NEKTAR_DOMAIN="${2#https://}"; shift 2 ;;
    --domain=*) v="${1#*=}"; NEKTAR_DOMAIN="${v#https://}"; shift ;;
    --network) NEKTAR_NETWORK="$2"; shift 2 ;;
    --network=*) NEKTAR_NETWORK="${1#*=}"; shift ;;
    --confirm) NEKTAR_MAINNET_CONFIRM="$2"; shift 2 ;;
    --confirm=*) NEKTAR_MAINNET_CONFIRM="${1#*=}"; shift ;;
    *) shift ;;
  esac
done

for k in NEKTAR_HTTP_TOKEN NEKTAR_OPERATOR_TOKEN; do
  if grep -qE "^$k=[[:space:]]*$" .env; then
    set_kv "$k" "$(openssl rand -hex 32)"
  fi
done
PROFILES=""
if [ -n "${NEKTAR_DOMAIN:-}" ]; then
  set_kv NEKTAR_DOMAIN "${NEKTAR_DOMAIN%/}"
  set_kv NEKTAR_PUBLIC_URL "https://${NEKTAR_DOMAIN%/}"
  set_kv NEKTAR_VERIFY_BASE "https://${NEKTAR_DOMAIN%/}"
  set_kv NEKTAR_TRUST_PROXY "1"
  PROFILES="--profile tls"
fi
[ -n "${NEKTAR_PUBLIC_URL:-}" ] && set_kv NEKTAR_PUBLIC_URL "$NEKTAR_PUBLIC_URL"
[ -n "${NEKTAR_MARKET_HOSTS:-}" ] && set_kv NEKTAR_MARKET_HOSTS "$NEKTAR_MARKET_HOSTS"
[ -n "${NEKTAR_SEED_DOMAINS:-}" ] && set_kv NEKTAR_SEED_DOMAINS "$NEKTAR_SEED_DOMAINS"
[ -n "${NEKTAR_NETWORK:-}" ] && set_kv NEKTAR_NETWORK "$NEKTAR_NETWORK"
[ -n "${NEKTAR_HEADERS_SOURCE:-}" ] && set_kv NEKTAR_HEADERS_SOURCE "$NEKTAR_HEADERS_SOURCE"
[ -n "${NEKTAR_SETTLE_SPEC:-}" ] && set_kv NEKTAR_SETTLE_SPEC "$NEKTAR_SETTLE_SPEC"
# mainnet ack gate — same refusal the runtime enforces, caught at install time
if grep -qE '^NEKTAR_NETWORK=main' .env; then
  if [ "${NEKTAR_MAINNET_CONFIRM:-}" != "I_UNDERSTAND" ]; then
    echo "refusing: NEKTAR_NETWORK=main spends REAL BSV."
    echo "re-run with NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND to acknowledge."
    exit 1
  fi
  set_kv NEKTAR_MAINNET_CONFIRM "I_UNDERSTAND"
  # Never carry a stale testnet header source into a mainnet .env; empty auto-selects dual-witness PoW defaults.
  if [ -z "${NEKTAR_HEADERS_SOURCE:-}" ] && grep -qE '^NEKTAR_HEADERS_SOURCE=woc:test[[:space:]]*$' .env; then
    set_kv NEKTAR_HEADERS_SOURCE ""
  fi
fi

# 5 — up (single v0.1 Go binary container + optional Caddy TLS)
# shellcheck disable=SC2086
$DOCKER compose $PROFILES --env-file .env up -d
echo
echo "-- agent starting; logs:  cd $DEST && $DOCKER compose logs -f agent"
echo "-- health:  curl http://localhost:${NEKTAR_HTTP_PORT:-18420}/v1/health"
echo "-- open dashboard (use pairing URL from logs or token in $DEST/.env)"
