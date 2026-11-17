#!/usr/bin/env bash
# nektar agent — one-command install for any bare Linux box with curl.
#
#   curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/agent/install.sh | bash
#
# Installs Docker if missing, fetches compose + env template, generates the
# two secrets, and starts the agent. The only value that cannot be generated
# is NEKTAR_MARKET_HOSTS — pass it in:
#
#   NEKTAR_MARKET_HOSTS=http://host-a:8080,http://host-b:8080 ./install.sh
#
# Optional passthroughs: NEKTAR_PUSH_PUBLIC, NEKTAR_FUND_WIF, NEKTAR_NETWORK,
# NEKTAR_AGENT_DIR (default ~/nektar-agent), NEKTAR_AGENT_BRANCH.
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
    sed -i "s|^$1=.*|$1=$2|" .env
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
[ -n "${NEKTAR_NETWORK:-}" ] && set_kv NEKTAR_NETWORK "$NEKTAR_NETWORK"

# 4 — the one value we cannot generate
if grep -q "host-a.example" .env; then
  echo
  echo "Set NEKTAR_MARKET_HOSTS in $DEST/.env to real bstack hosts, then:"
  echo "  cd $DEST && $DOCKER compose --env-file .env up -d"
  echo "(or rerun with NEKTAR_MARKET_HOSTS=... ./install.sh)"
  exit 0
fi

# 5 — up
$DOCKER compose --env-file .env up -d
echo
echo "-- agent starting; logs:  cd $DEST && $DOCKER compose logs -f agent"
echo "-- health:  curl http://localhost:8080/.well-known/nektar"
echo "-- first boot prints a fee address — fund it to activate the market"
