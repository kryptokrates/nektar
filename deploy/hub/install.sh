#!/usr/bin/env bash
# Nektar v0.1 1-command Hub installer (Ubuntu/Debian/macOS VPS):
#   curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub/install.sh | NEKTAR_PUBLIC_URL=https://hub.example.com bash
#   curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub/install.sh | NEKTAR_AUTO_IP=1 bash
#   curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub/install.sh | NEKTAR_HUB_MODE=tunnel bash
set -euo pipefail

RAW_BASE="${NEKTAR_RAW_BASE:-https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub}"
HUB_DIR="${NEKTAR_HUB_DIR:-$HOME/nektar-hub}"
NET="${NEKTAR_NETWORK:-testnet}"
MODE="${NEKTAR_HUB_MODE:-domain}"
AUTO_IP="${NEKTAR_AUTO_IP:-0}"
if [ "${NEKTAR_TUNNEL:-0}" = "1" ]; then
  MODE="tunnel"
fi

if [ "$NET" = "main" ] && [ "${NEKTAR_MAINNET_CONFIRM:-}" != "I_UNDERSTAND" ]; then
  echo "Refusing mainnet Hub without NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND" >&2
  exit 2
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "== Installing Docker Engine =="
  curl -fsSL https://get.docker.com | sh
fi

mkdir -p "$HUB_DIR"
cd "$HUB_DIR"

curl -fsSL "$RAW_BASE/compose.yaml" -o compose.yaml

gen_hex() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 32
  else
    head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

DOMAIN=""
if [ -n "${NEKTAR_PUBLIC_URL:-}" ]; then
  DOMAIN="${NEKTAR_PUBLIC_URL#https://}"
  DOMAIN="${DOMAIN#http://}"
  DOMAIN="${DOMAIN%%/*}"
elif [ -n "${NEKTAR_DOMAIN:-}" ] && [ "${NEKTAR_DOMAIN}" != ":80" ]; then
  DOMAIN="${NEKTAR_DOMAIN#https://}"
  DOMAIN="${DOMAIN#http://}"
  DOMAIN="${DOMAIN%%/*}"
  NEKTAR_PUBLIC_URL="https://${DOMAIN}"
elif [ "$AUTO_IP" = "1" ] || [ "$MODE" = "auto-ip" ]; then
  IP="$(curl -fsSL https://api.ipify.org || curl -fsSL https://ifconfig.me/ip)"
  DOMAIN="${IP//./-}.sslip.io"
  NEKTAR_PUBLIC_URL="https://${DOMAIN}"
  echo "== Resolved zero-domain TLS host: ${NEKTAR_PUBLIC_URL} =="
fi

HTTP_TOK="${NEKTAR_HTTP_TOKEN:-$(gen_hex)}"
OP_TOK="${NEKTAR_OPERATOR_TOKEN:-$(gen_hex)}"
TRUST_PROXY="0"
if [ -n "$DOMAIN" ] || [ "$MODE" = "tunnel" ]; then
  TRUST_PROXY="1"
fi

SEED_HUB="$( [ "$NET" = "main" ] && echo "https://nektar.run/hub" || echo "https://testnet.nektar.run/hub" )"
DEFAULT_PRESET="$( [ "$NET" = "main" ] && echo "hybrid" || echo "free" )"

cat > .env <<EOF
NEKTAR_NETWORK=${NET}
NEKTAR_HTTP_BIND=127.0.0.1
NEKTAR_HTTP_PORT=18420
NEKTAR_PUBLIC_URL=${NEKTAR_PUBLIC_URL:-}
NEKTAR_DOMAIN=${DOMAIN:-:80}
NEKTAR_TRUST_PROXY=${TRUST_PROXY}
NEKTAR_HTTP_TOKEN=${HTTP_TOK}
NEKTAR_OPERATOR_TOKEN=${OP_TOK}
NEKTAR_HUB_ROLE=${NEKTAR_HUB_ROLE:-leaf}
NEKTAR_HUB_FEE_PRESET=${NEKTAR_HUB_FEE_PRESET:-${DEFAULT_PRESET}}
NEKTAR_PEER_HUBS=${NEKTAR_PEER_HUBS:-${SEED_HUB}}
NEKTAR_UPSTREAM_SPINES=${NEKTAR_UPSTREAM_SPINES:-${SEED_HUB}}
NEKTAR_SPINE_PEER_PUBS=${NEKTAR_SPINE_PEER_PUBS:-}
NEKTAR_HEADERS_SOURCE=${NEKTAR_HEADERS_SOURCE:-$([ "$NET" = "main" ] && echo "woc:main,chaintracks:https://mainnet-chaintracks.babbage.systems/v2" || echo "woc:test")}
NEKTAR_SETTLE_SPEC=${NEKTAR_SETTLE_SPEC:-$([ "$NET" = "main" ] && echo "arcade:https://arcade-v2-us-1.bsvblockchain.tech" || echo "arcade:https://arcade-testnet-us-1.bsvblockchain.tech")}
NEKTAR_MAINNET_CONFIRM=${NEKTAR_MAINNET_CONFIRM:-}
CLOUDFLARE_TUNNEL_TOKEN=${CLOUDFLARE_TUNNEL_TOKEN:-}
EOF
chmod 600 .env

PROFILES=()
if [ -n "$DOMAIN" ]; then
  PROFILES+=(--profile tls)
elif [ "$MODE" = "tunnel" ]; then
  if [ -n "${CLOUDFLARE_TUNNEL_TOKEN:-}" ]; then
    PROFILES+=(--profile tunnel-token)
  else
    PROFILES+=(--profile tunnel)
  fi
fi

docker compose -f compose.yaml --env-file .env "${PROFILES[@]}" pull
docker compose -f compose.yaml --env-file .env "${PROFILES[@]}" up -d

echo ""
echo "✓ Nektar v0.1 Hub running in ${HUB_DIR}"
if [ -n "$DOMAIN" ]; then
  echo "  Public URL : https://${DOMAIN}"
  echo "  Discovery  : https://${DOMAIN}/.well-known/nektar"
  echo "  Hub API    : https://${DOMAIN}/hub/v1/health"
elif [ "$MODE" = "tunnel" ]; then
  echo "  Tunnel URL : docker compose -f ${HUB_DIR}/compose.yaml logs cloudflared"
else
  echo "  Local URL  : http://127.0.0.1:18420"
fi
echo "  Logs       : docker compose -f ${HUB_DIR}/compose.yaml logs -f hub"
