#!/usr/bin/env sh
# nektar v0.4 zero-Docker native installer (macOS & Linux):
#   curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh -s -- --dock
#   curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh -s -- --docker
set -eu

RAW_BASE="${NEKTAR_RAW_BASE:-https://raw.githubusercontent.com/kryptokrates/nektar/master}"
BIN_DIR="${NEKTAR_BIN_DIR:-$HOME/.local/bin}"
DO_DOCK=0
USE_DOCKER=0

for arg in "$@"; do
  case "$arg" in
    --dock) DO_DOCK=1 ;;
    --docker) USE_DOCKER=1 ;;
    --native) USE_DOCKER=0 ;;
    -h|--help)
      printf 'Usage: install.sh [--dock] [--docker]\n'
      printf '  default: downloads the single zero-CGO static Go binary (~27 MB, zero Docker)\n'
      printf '  --dock:   auto-detect & wire Claude Code, Cursor, Codex, VS Code, Windsurf\n'
      printf '  --docker: install the Docker/GHCR host launcher instead of the native binary\n'
      exit 0
      ;;
  esac
done

mkdir -p "$BIN_DIR"

if [ "$USE_DOCKER" = "1" ]; then
  printf '\n== nektar v0.4 install (Docker / GHCR opt-in) ==\n'
  if ! command -v docker >/dev/null 2>&1; then
    printf 'Docker is required for --docker mode. Re-run without --docker for the zero-dependency native Go binary.\n' >&2
    exit 1
  fi
  curl -fsSL "$RAW_BASE/install/nektar" -o "$BIN_DIR/nektar"
  chmod 755 "$BIN_DIR/nektar"
  printf '✓ installed Docker launcher → %s/nektar\n' "$BIN_DIR"
else
  printf '\n== nektar v0.4 install (native single static Go binary — zero Docker, zero Node.js) ==\n'
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"
  case "${os}-${arch}" in
    linux-x86_64|linux-amd64) asset="nektar-linux-amd64.tar.gz" ;;
    linux-aarch64|linux-arm64) asset="nektar-linux-arm64.tar.gz" ;;
    darwin-arm64|darwin-aarch64) asset="nektar-darwin-arm64.tar.gz" ;;
    *)
      printf 'No prebuilt binary for %s-%s — build from source with `go build -o %s/nektar ./cmd/nektar` or pass --docker.\n' "$os" "$arch" "$BIN_DIR" >&2
      exit 1
      ;;
  esac
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  printf '-- downloading %s from latest GitHub release\n' "$asset"
  curl -fsSL "https://github.com/kryptokrates/nektar/releases/latest/download/$asset" | tar -xz -C "$tmp"
  install -m 755 "$tmp/nektar" "$BIN_DIR/nektar"
  printf '✓ installed native nektar v0.4 → %s/nektar\n' "$BIN_DIR"
fi

case ":${PATH}:" in
  *":${BIN_DIR}:"*) ;;
  *)
    printf '\nNote: add %s to your PATH:\n  export PATH="%s:$PATH"\n\n' "$BIN_DIR" "$BIN_DIR"
    PATH="$BIN_DIR:$PATH"
    export PATH
    ;;
esac

if [ "$DO_DOCK" = "1" ]; then
  "$BIN_DIR/nektar" dock auto
else
  printf '\nNext (zero Docker, starts in <50ms on http://127.0.0.1:18420):\n'
  printf '  nektar serve           # start unified agent daemon + Svelte Dashboard + MCP + Market Broker\n'
  printf '  nektar serve --hub     # start agent + embedded durable SQLite Overlay Hub + Bounties\n'
  printf '  nektar dock            # auto-wire Claude Code, Cursor, VS Code, Hermes, OpenClaw, Codex…\n'
  printf '  nektar open            # open Dashboard with 1-click pairing\n'
  printf '  nektar doctor          # check wallet, policies, peers & SSE health\n\n'
fi
