#!/usr/bin/env sh
# nektar one-line installer (macOS & Linux):
#   curl -fsSL https://nektar.run/install.sh | sh
#   curl -fsSL https://nektar.run/install.sh | sh -s -- --dock
#   curl -fsSL https://nektar.run/install.sh | sh -s -- --native   # host binary for BSV Desktop
set -eu

RAW_BASE="${NEKTAR_RAW_BASE:-https://raw.githubusercontent.com/kryptokrates/nektar/master}"
BIN_DIR="${NEKTAR_BIN_DIR:-$HOME/.local/bin}"
DO_DOCK=0
NATIVE=0

for arg in "$@"; do
  case "$arg" in
    --dock) DO_DOCK=1 ;;
    --native) NATIVE=1 ;;
    -h|--help)
      printf 'Usage: install.sh [--dock] [--native]\n'
      exit 0
      ;;
  esac
done

printf '\n== nektar install (GHCR-first) ==\n'

if [ "$NATIVE" = "1" ]; then
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"
  case "${os}-${arch}" in
    linux-x86_64) asset="nektar-mcp-linux-x64.tar.gz" ;;
    darwin-arm64) asset="nektar-mcp-darwin-arm64.tar.gz" ;;
    *) printf 'No prebuilt native tarball for %s-%s — use Docker GHCR install instead.\n' "$os" "$arch" >&2; exit 1 ;;
  esac
  dest="${NEKTAR_HOME:-$HOME/.nektar/native}"
  mkdir -p "$dest" "$BIN_DIR"
  printf '-- downloading %s from latest GitHub release\n' "$asset"
  curl -fsSL "https://github.com/kryptokrates/nektar/releases/latest/download/$asset" | tar -xz -C "$dest"
  ln -sf "$dest/nektar" "$BIN_DIR/nektar"
  printf '✓ installed native nektar → %s/nektar\n' "$BIN_DIR"
  printf '  start: nektar serve   (supports NEKTAR_WALLET=client with BSV Desktop)\n'
  exit 0
fi

# 1. Check Docker
if ! command -v docker >/dev/null 2>&1; then
  if [ "$(uname -s)" = "Linux" ]; then
    printf '-- Docker not found; installing Docker Engine via https://get.docker.com\n'
    curl -fsSL https://get.docker.com | sudo sh
  else
    printf 'Docker is required for GHCR install.\n' >&2
    printf '  macOS: install OrbStack (https://orbstack.dev) or Docker Desktop (https://docker.com)\n' >&2
    printf '  or re-run with --native for the host Node.js build.\n' >&2
    exit 1
  fi
fi

# 2. Install thin host launcher
mkdir -p "$BIN_DIR"
SCRIPT_DIR="$(cd -- "$(dirname -- "${0:-}")" 2>/dev/null && pwd -P || true)"
if [ -n "$SCRIPT_DIR" ] && [ -f "$SCRIPT_DIR/nektar" ]; then
  cp "$SCRIPT_DIR/nektar" "$BIN_DIR/nektar"
else
  curl -fsSL "$RAW_BASE/install/nektar" -o "$BIN_DIR/nektar"
fi
chmod 755 "$BIN_DIR/nektar"
printf '✓ installed launcher → %s/nektar\n' "$BIN_DIR"

case ":${PATH}:" in
  *":${BIN_DIR}:"*) ;;
  *)
    printf '\nNote: add %s to your PATH:\n  export PATH="%s:$PATH"\n\n' "$BIN_DIR" "$BIN_DIR"
    PATH="$BIN_DIR:$PATH"
    export PATH
    ;;
esac

# 3. Boot GHCR daemon
"$BIN_DIR/nektar" up

# 4. Optional auto-dock
if [ "$DO_DOCK" = "1" ]; then
  "$BIN_DIR/nektar" dock auto
else
  printf '\nNext:\n'
  printf '  nektar open        # open Dashboard (1-click testnet faucet + live recall)\n'
  printf '  nektar dock        # wire Claude Code, Cursor, Hermes, OpenClaw, Codex, VS Code…\n'
  printf '  nektar dock proxy  # get OPENAI_BASE_URL for any existing running agent\n'
  printf '  nektar hub up      # graduate to a public Hub Node when ready\n\n'
fi
