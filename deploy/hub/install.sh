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
# Optional CLI flags (or env passthroughs):
#   --domain <fqdn>      Enable built-in Caddy Auto-TLS on <fqdn>
#   --auto-ip            Zero-domain HTTPS via <public-ip>.sslip.io + Caddy Auto-TLS
#   --tunnel             Zero-port HTTPS via Cloudflare Quick Tunnel (behind NAT)
#   --network <net>      testnet (default) | main (requires --confirm I_UNDERSTAND)
#   --no-faucet          Skip automatic testnet faucet drip
while [ $# -gt 0 ]; do
  case "$1" in
    --domain) NEKTAR_PUBLIC_URL="https://${2#https://}"; shift 2 ;;
    --domain=*) v="${1#*=}"; NEKTAR_PUBLIC_URL="https://${v#https://}"; shift ;;
    --auto-ip) NEKTAR_AUTO_IP=1; shift ;;
    --tunnel) NEKTAR_HUB_MODE=tunnel; shift ;;
    --network) NEKTAR_NETWORK="$2"; shift 2 ;;
    --network=*) NEKTAR_NETWORK="${1#*=}"; shift ;;
    --confirm) NEKTAR_MAINNET_CONFIRM="$2"; shift 2 ;;
    --confirm=*) NEKTAR_MAINNET_CONFIRM="${1#*=}"; shift ;;
    --no-faucet) NEKTAR_SKIP_AUTO_FAUCET=1; shift ;;
    *) shift ;;
  esac
done

for k in NEKTAR_KEY_PASSPHRASE NEKTAR_HTTP_TOKEN NEKTAR_OPERATOR_TOKEN OVERLAY_DB_PASSWORD OVERLAY_ADMIN_TOKEN; do
  if grep -qE "^$k=[[:space:]]*$" .env; then
    set_kv "$k" "$(openssl rand -hex 32)"
  fi
done

# Zero-domain HTTPS via sslip.io when NEKTAR_AUTO_IP=1 (or --auto-ip)
if [ "${NEKTAR_AUTO_IP:-0}" = "1" ] && [ -z "${NEKTAR_PUBLIC_URL:-}" ]; then
  pub_ip="$(curl -fsS --max-time 4 https://api.ipify.org 2>/dev/null || curl -fsS --max-time 4 https://ifconfig.me/ip 2>/dev/null || true)"
  if echo "$pub_ip" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$'; then
    sslip="$(echo "$pub_ip" | tr '.' '-').sslip.io"
    NEKTAR_PUBLIC_URL="https://${sslip}"
    echo "-- auto-IP TLS host: ${NEKTAR_PUBLIC_URL}"
  fi
fi

PROFILES=""
if [ -n "${NEKTAR_PUBLIC_URL:-}" ]; then
  pub="${NEKTAR_PUBLIC_URL%/}"
  dom="${pub#https://}"
  dom="${dom#http://}"
  dom="${dom%%/*}"
  set_kv NEKTAR_DOMAIN "$dom"
  set_kv NEKTAR_PUBLIC_URL "$pub"
  PROFILES="--profile tls"
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
elif [ "${NEKTAR_HUB_MODE:-}" = "tunnel" ]; then
  set_kv NEKTAR_DOMAIN ":80"
  PROFILES="--profile tls --profile tunnel"
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
  echo "!! NEKTAR_PUBLIC_URL unset — booting localhost-only (pass --auto-ip, --domain <fqdn>, or --tunnel to publish)."
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
# shellcheck disable=SC2086
$DOCKER compose $PROFILES --env-file .env up -d

# 5 — wait for agent health + automatic testnet faucet drip
http_tok="$(grep '^NEKTAR_HTTP_TOKEN=' .env | cut -d= -f2-)"
for _ in $(seq 1 40); do
  if curl -fsS --max-time 2 http://127.0.0.1:8080/v1/health >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
if ! grep -qE '^NEKTAR_NETWORK=main' .env && [ "${NEKTAR_SKIP_AUTO_FAUCET:-0}" != "1" ] && [ -n "$http_tok" ]; then
  echo "-- requesting free testnet sats for this hub's fee address..."
  curl -fsS --max-time 15 -X POST http://127.0.0.1:8080/v1/onboard/faucet \
    -H "Authorization: Bearer $http_tok" >/dev/null 2>&1 \
    && echo "-- ✓ testnet faucet drip requested & imported" \
    || echo "-- · auto-faucet skipped (already funded or faucet busy — fund anytime in Dashboard)"
fi
echo
echo "-- hub UP; logs:  cd $DEST && $DOCKER compose logs -f agent"
if [ -n "${NEKTAR_PUBLIC_URL:-}" ]; then
  echo "-- public HTTPS : ${NEKTAR_PUBLIC_URL%/}/.well-known/nektar (Caddy Auto-TLS active)"
elif [ "${NEKTAR_HUB_MODE:-}" = "tunnel" ]; then
  echo "-- tunnel URL   : cd $DEST && $DOCKER compose logs cloudflared | grep trycloudflare"
fi
echo "-- verify card  : curl -X POST http://localhost:18391/lookup -H 'content-type: application/json' \\"
echo "                    -d '{\"service\":\"ls_nektar\",\"query\":{\"announces\":{\"net\":\"testnet\"}}}'"
