#!/usr/bin/env bash
# nektar hub — one-command install: agent + host pair + watch shims + db.
#
#   curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub/install.sh | bash
#
# Installs Docker if missing, fetches compose + env template, generates the
# four secrets, and starts the hub. One thing it cannot know — your hub's
# PUBLIC base URL (used to derive your announced host/push/watch URLs):
#
#   NEKTAR_PUBLIC_URL=https://hub.example ./install.sh
#
# (Or override NEKTAR_MARKET_HOSTS / NEKTAR_PUSH_PUBLIC / NEKTAR_WATCH_URLS /
# NEKTAR_WATCH_PUBLIC individually.)
# If unset, the install brings the hub up on localhost defaults with
# NEKTAR_ANNOUNCE=0 — a smoke check that advertises nothing unreachable.
# Set NEKTAR_PUBLIC_URL (or NEKTAR_MARKET_HOSTS) and re-run to publish.
#
# Optional passthroughs: NEKTAR_PUBLIC_URL, NEKTAR_MARKET_HOSTS,
# NEKTAR_PUSH_PUBLIC, NEKTAR_WATCH_URLS, NEKTAR_WATCH_PUBLIC,
# NEKTAR_FUND_WIF, NEKTAR_FUND_TXIDS, NEKTAR_WAIT_FUNDING,
# NEKTAR_INDEXER_DISCOVERY, NEKTAR_SEED_DOMAINS, NEKTAR_NETWORK,
# NEKTAR_ANNOUNCE, NEKTAR_FAUCET, NEKTAR_FAUCET_SATS,
# NEKTAR_FAUCET_DAILY_SATS, NEKTAR_MAX_SPEND_SATS_PER_DAY,
# NEKTAR_ZEROCONF_MAX_SATS, NEKTAR_ZEROCONF_MAX_ANCESTORS,
# NEKTAR_STRICT_HEADERS, NEKTAR_HUB_DIR (default ~/nektar-hub),
# NEKTAR_AGENT_BRANCH.
#
# Funding needs no WIF: the agent generates its own key on first boot and
# prints `operator fee key <address>` — send testnet sats there and set
# NEKTAR_FUND_TXIDS (or NEKTAR_WAIT_FUNDING=1 NEKTAR_INDEXER_DISCOVERY=1 to
# have the agent wait and detect on its own).
#
# NEKTAR_NETWORK=main additionally requires NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND
# in the environment — the install refuses to write a mainnet .env without it.
set -euo pipefail

DEST="${NEKTAR_HUB_DIR:-$HOME/nektar-hub}"
BRANCH="${NEKTAR_AGENT_BRANCH:-master}"
RAW="https://raw.githubusercontent.com/kryptokrates/nektar/$BRANCH/deploy/hub"
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]:-$0}")" >/dev/null 2>&1 && pwd -P || true)"

echo "== nektar hub install =="

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
for k in NEKTAR_KEY_PASSPHRASE NEKTAR_HTTP_TOKEN OVERLAY_DB_PASSWORD OVERLAY_ADMIN_TOKEN; do
  if grep -qE "^$k=[[:space:]]*$" .env; then
    set_kv "$k" "$(openssl rand -hex 32)"
  fi
done
if [ -n "${NEKTAR_PUBLIC_URL:-}" ]; then
  pub="${NEKTAR_PUBLIC_URL%/}"
  net="${NEKTAR_NETWORK:-testnet}"
  if [ -z "${NEKTAR_MARKET_HOSTS:-}" ]; then
    if [ "$net" = "testnet" ]; then
      NEKTAR_MARKET_HOSTS="${pub}/hosts/a,${pub}/hosts/b,https://nektar.run/hosts/a,https://nektar.run/hosts/b"
    else
      NEKTAR_MARKET_HOSTS="${pub}/hosts/a,${pub}/hosts/b"
    fi
  fi
  [ -z "${NEKTAR_PUSH_PUBLIC:-}" ] && NEKTAR_PUSH_PUBLIC="$pub"
  [ -z "${NEKTAR_WATCH_URLS:-}" ] && NEKTAR_WATCH_URLS="${pub}/watch/a,${pub}/watch/b"
  [ -z "${NEKTAR_WATCH_PUBLIC:-}" ] && NEKTAR_WATCH_PUBLIC="${pub}/watch"
fi
for k in NEKTAR_MARKET_HOSTS NEKTAR_PUSH_PUBLIC NEKTAR_WATCH_URLS NEKTAR_WATCH_PUBLIC \
  NEKTAR_FUND_WIF NEKTAR_FUND_TXIDS NEKTAR_WAIT_FUNDING NEKTAR_INDEXER_DISCOVERY \
  NEKTAR_SEED_DOMAINS NEKTAR_NETWORK NEKTAR_ANNOUNCE \
  NEKTAR_FAUCET NEKTAR_FAUCET_SATS NEKTAR_FAUCET_DAILY_SATS \
  NEKTAR_MAX_SPEND_SATS_PER_DAY NEKTAR_ZEROCONF_MAX_SATS NEKTAR_ZEROCONF_MAX_ANCESTORS \
  NEKTAR_STRICT_HEADERS; do
  eval "v=\${$k:-}"
  [ -n "$v" ] && set_kv "$k" "$v"
done
# smoke-check fallback: no public urls → localhost pair, announce off so a
# boot that can't be reached from the network also advertises nothing
if ! grep -qE '^NEKTAR_MARKET_HOSTS=.+,' .env; then
  set_kv NEKTAR_MARKET_HOSTS "http://localhost:18391,http://localhost:18392"
  set_kv NEKTAR_ANNOUNCE "0"
  echo "!! NEKTAR_MARKET_HOSTS unset — booting localhost-only with ANNOUNCE=0."
  echo "   Re-run with NEKTAR_MARKET_HOSTS=<public urls> to join the network."
fi
# mainnet ack gate — same refusal the runtime enforces, caught at install time
if grep -qE '^NEKTAR_NETWORK=main' .env; then
  if [ "${NEKTAR_MAINNET_CONFIRM:-}" != "I_UNDERSTAND" ]; then
    echo "refusing: NEKTAR_NETWORK=main spends REAL BSV."
    echo "re-run with NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND to acknowledge."
    exit 1
  fi
  set_kv NEKTAR_MAINNET_CONFIRM "I_UNDERSTAND"
fi

# 4 — up
$DOCKER compose --env-file .env up -d
echo
echo "-- hub starting; logs:  cd $DEST && $DOCKER compose logs -f agent"
echo "-- the agent prints 'operator fee key <address>' — send testnet sats"
echo "   there (or NEKTAR_FUND_TXIDS=<txid> + restart if you already paid it)"
echo "-- verify your card once announced:"
echo "   curl -X POST http://localhost:18391/lookup -H 'content-type: application/json' \\"
echo "     -d '{\"service\":\"ls_nektar\",\"query\":{\"announces\":{\"net\":\"testnet\"}}}'"
