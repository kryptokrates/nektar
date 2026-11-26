#!/usr/bin/env sh
# nektar v0.1 zero-Docker native installer (macOS & Linux):
#   curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh -s -- --up
#   curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh -s -- --up --mainnet --confirm I_UNDERSTAND
#   curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh -s -- --dock
#   curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh -s -- --docker
set -eu

RAW_BASE="${NEKTAR_RAW_BASE:-https://raw.githubusercontent.com/kryptokrates/nektar/master}"
BIN_DIR="${NEKTAR_BIN_DIR:-$HOME/.local/bin}"
DO_UP=0
DO_DOCK=0
USE_DOCKER=0
USE_MAINNET=0
MAINNET_CONFIRM="${NEKTAR_MAINNET_CONFIRM:-}"
WALLET_MODE="${NEKTAR_WALLET:-}"
INSTALL_SERVICE=0

while [ "$#" -gt 0 ]; do
  case "$1" in
    --up|--start) DO_UP=1 ;;
    --dock) DO_DOCK=1 ;;
    --docker) USE_DOCKER=1 ;;
    --native) USE_DOCKER=0 ;;
    --mainnet|--main) USE_MAINNET=1 ;;
    --wallet)
      shift
      WALLET_MODE="${1:-}"
      ;;
    --wallet=*)
      WALLET_MODE="${1#--wallet=}"
      ;;
    --confirm)
      shift
      MAINNET_CONFIRM="${1:-}"
      ;;
    --confirm=*)
      MAINNET_CONFIRM="${1#--confirm=}"
      ;;
    --install-service) INSTALL_SERVICE=1 ;;
    -h|--help)
      printf 'Usage: install.sh [--up] [--mainnet --confirm I_UNDERSTAND] [--install-service] [--dock] [--docker]\n'
      printf '  default:           downloads the single zero-CGO static Go binary (~27 MB, zero Docker)\n'
      printf '  --up:              start background daemon + Restate + auto-dock Claude/Codex/Hermes/OpenClaw/Cursor + live receipt check\n'
      printf '  --mainnet:         boot on BSV Mainnet (isolated ~/.nektar/main; requires --confirm I_UNDERSTAND)\n'
      printf '  --install-service: also install login autostart service (launchd / systemd --user)\n'
      printf '  --dock:            auto-detect & wire AI apps without starting the daemon\n'
      printf '  --docker:          install the Docker/GHCR host launcher instead of the native binary\n'
      exit 0
      ;;
  esac
  shift
done

mkdir -p "$BIN_DIR"

if [ "$USE_DOCKER" = "1" ]; then
  printf '\n== nektar v0.1 install (Docker / GHCR opt-in) ==\n'
  if ! command -v docker >/dev/null 2>&1; then
    printf 'Docker is required for --docker mode. Re-run without --docker for the zero-dependency native Go binary.\n' >&2
    exit 1
  fi
  curl -fsSL "$RAW_BASE/install/nektar" -o "$BIN_DIR/nektar"
  chmod 755 "$BIN_DIR/nektar"
  printf '✓ installed Docker launcher → %s/nektar\n' "$BIN_DIR"
else
  printf '\n== nektar v0.1 install (native single static Go binary — zero Docker, zero Node.js) ==\n'
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"
  case "${os}-${arch}" in
    linux-x86_64|linux-amd64) asset="nektar-linux-amd64.tar.gz" ;;
    linux-aarch64|linux-arm64) asset="nektar-linux-arm64.tar.gz" ;;
    darwin-arm64|darwin-aarch64) asset="nektar-darwin-arm64.tar.gz" ;;
    darwin-x86_64|darwin-amd64) asset="nektar-darwin-amd64.tar.gz" ;;
    *)
      printf 'No prebuilt binary for %s-%s — build from source with `go build -o %s/nektar ./cmd/nektar` or pass --docker.\n' "$os" "$arch" "$BIN_DIR" >&2
      exit 1
      ;;
  esac
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  base_dl="https://github.com/kryptokrates/nektar/releases/latest/download"
  printf '-- downloading %s and SHA256SUMS from latest GitHub release\n' "$asset"
  curl -fsSL "$base_dl/$asset" -o "$tmp/$asset"
  if ! curl -fsSL "$base_dl/SHA256SUMS" -o "$tmp/SHA256SUMS"; then
    printf 'ERROR: failed to download SHA256SUMS — refusing unverified binary\n' >&2
    exit 1
  fi
  want_sum="$(awk -v f="$asset" '$2 == f || $2 == ("*" f) { print $1; exit }' "$tmp/SHA256SUMS")"
  if [ -z "$want_sum" ]; then
    printf 'ERROR: %s missing from SHA256SUMS — refusing unverified binary\n' "$asset" >&2
    exit 1
  fi
  if command -v sha256sum >/dev/null 2>&1; then
    got_sum="$(sha256sum "$tmp/$asset" | awk '{print $1}')"
  elif command -v shasum >/dev/null 2>&1; then
    got_sum="$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')"
  else
    printf 'ERROR: neither sha256sum nor shasum is installed — cannot verify binary integrity\n' >&2
    exit 1
  fi
  if [ "$got_sum" != "$want_sum" ]; then
    printf 'ERROR: SHA-256 mismatch for %s (got %s, want %s)\n' "$asset" "$got_sum" "$want_sum" >&2
    exit 1
  fi
  printf '✓ verified SHA-256 checksum for %s\n' "$asset"
  tar -xzf "$tmp/$asset" -C "$tmp"
  install -m 755 "$tmp/nektar" "$BIN_DIR/nektar"
  printf '✓ installed native nektar v0.1 → %s/nektar\n' "$BIN_DIR"
fi

# Persist BIN_DIR in user shell profile idempotently so `nektar` works immediately in future terminals.
persist_path() {
  rc_file="$1"
  [ -n "$rc_file" ] || return 0
  if [ -f "$rc_file" ] && grep -F "$BIN_DIR" "$rc_file" >/dev/null 2>&1; then
    return 0
  fi
  printf '\n# Added by nektar installer\nexport PATH="%s:$PATH"\n' "$BIN_DIR" >> "$rc_file" 2>/dev/null || return 0
  printf '✓ added %s to PATH in %s\n' "$BIN_DIR" "$rc_file"
}

case ":${PATH}:" in
  *":${BIN_DIR}:"*) ;;
  *)
    PATH="$BIN_DIR:$PATH"
    export PATH
    user_shell="$(basename "${SHELL:-sh}")"
    case "$user_shell" in
      zsh) persist_path "$HOME/.zshrc" ;;
      bash)
        if [ -f "$HOME/.bashrc" ]; then
          persist_path "$HOME/.bashrc"
        else
          persist_path "$HOME/.profile"
        fi
        ;;
      *) persist_path "$HOME/.profile" ;;
    esac
    ;;
esac

if [ "$DO_UP" = "1" ]; then
  set -- up
  if [ "$USE_MAINNET" = "1" ]; then
    set -- "$@" --network main --confirm "${MAINNET_CONFIRM:-I_UNDERSTAND}"
  fi
  if [ -n "$WALLET_MODE" ]; then
    set -- "$@" --wallet "$WALLET_MODE"
  fi
  if [ "$INSTALL_SERVICE" = "1" ]; then
    set -- "$@" --install-service
  fi
  "$BIN_DIR/nektar" "$@"
elif [ "$DO_DOCK" = "1" ]; then
  "$BIN_DIR/nektar" dock auto
else
  printf '\nNext (25 seconds to live docked agents — zero Docker):\n'
  printf '  nektar up              # start background daemon + Restate + auto-dock Claude/Codex/Hermes/OpenClaw/Cursor\n'
  printf '  nektar up --network main --confirm I_UNDERSTAND   # go live on BSV Mainnet\n'
  printf '  nektar earn            # view Hub knowledge bounties & earn BSV sats\n'
  printf '  nektar open            # open Dashboard with 1-click pairing\n'
  printf '  nektar doctor          # check wallet, Restate, policies, peers & cohort health\n\n'
fi
