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
# Optional passthroughs: NEKTAR_PUSH_PUBLIC, NEKTAR_FUND_WIF, NEKTAR_NETWORK,
# NEKTAR_SEED_DOMAINS, NEKTAR_ANNOUNCE,
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
if grep -qE '^NEKTAR_KEY_PASSPHRASE=[[:space:]]*$' .env; then
  set_kv NEKTAR_KEY_PASSPHRASE "$(openssl rand -hex 32)"
fi
if grep -qE '^NEKTAR_HTTP_TOKEN=[[:space:]]*$' .env; then
  set_kv NEKTAR_HTTP_TOKEN "$(openssl rand -hex 32)"
fi
[ -n "${NEKTAR_MARKET_HOSTS:-}" ] && set_kv NEKTAR_MARKET_HOSTS "$NEKTAR_MARKET_HOSTS"
[ -n "${NEKTAR_PUSH_PUBLIC:-}" ] && set_kv NEKTAR_PUSH_PUBLIC "$NEKTAR_PUSH_PUBLIC"
[ -n "${NEKTAR_FUND_WIF:-}" ] && set_kv NEKTAR_FUND_WIF "$NEKTAR_FUND_WIF"
[ -n "${NEKTAR_FUND_TXIDS:-}" ] && set_kv NEKTAR_FUND_TXIDS "$NEKTAR_FUND_TXIDS"
[ -n "${NEKTAR_INDEXER_DISCOVERY:-}" ] && set_kv NEKTAR_INDEXER_DISCOVERY "$NEKTAR_INDEXER_DISCOVERY"
[ -n "${NEKTAR_NETWORK:-}" ] && set_kv NEKTAR_NETWORK "$NEKTAR_NETWORK"
# mainnet ack gate — same refusal the runtime enforces, caught at install time
if grep -qE '^NEKTAR_NETWORK=main' .env; then
  if [ "${NEKTAR_MAINNET_CONFIRM:-}" != "I_UNDERSTAND" ]; then
    echo "refusing: NEKTAR_NETWORK=main spends REAL BSV."
    echo "re-run with NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND to acknowledge."
    exit 1
  fi
  set_kv NEKTAR_MAINNET_CONFIRM "I_UNDERSTAND"
fi
[ -n "${NEKTAR_SEED_DOMAINS:-}" ] && set_kv NEKTAR_SEED_DOMAINS "$NEKTAR_SEED_DOMAINS"
[ -n "${NEKTAR_ANNOUNCE:-}" ] && set_kv NEKTAR_ANNOUNCE "$NEKTAR_ANNOUNCE"

# 5 — up
$DOCKER compose --env-file .env up -d
echo
echo "-- agent starting; logs:  cd $DEST && $DOCKER compose logs -f agent"
echo "-- health:  curl http://localhost:8080/.well-known/nektar"
echo "-- first boot prints a fee address — fund it to activate the market"
