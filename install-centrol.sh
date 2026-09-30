#!/bin/sh
# install-centrol.sh — installs the centrol CLI to /usr/local/bin (or
# $CENTROL_INSTALL_DIR if set). No telemetry, no account, no network
# access beyond fetching this one release asset.
set -eu

REPO="baitelmal/centrol"
INSTALL_DIR="${CENTROL_INSTALL_DIR:-/usr/local/bin}"

os() {
  case "$(uname -s)" in
    Darwin) echo "darwin" ;;
    Linux) echo "linux" ;;
    *) echo "unsupported OS: $(uname -s)" >&2; exit 1 ;;
  esac
}

arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    arm64|aarch64) echo "arm64" ;;
    *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
  esac
}

main() {
  platform_os="$(os)"
  platform_arch="$(arch)"
  asset="centrol-${platform_os}-${platform_arch}"
  url="https://github.com/${REPO}/releases/latest/download/${asset}"

  echo "centrol: downloading ${asset}..." >&2
  tmp="$(mktemp)"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" -o "$tmp"
  elif command -v wget >/dev/null 2>&1; then
    wget -q "$url" -O "$tmp"
  else
    echo "centrol: need curl or wget to install" >&2
    exit 1
  fi

  chmod +x "$tmp"
  if [ -w "$INSTALL_DIR" ]; then
    mv "$tmp" "$INSTALL_DIR/centrol"
  else
    echo "centrol: installing to $INSTALL_DIR requires sudo" >&2
    sudo mv "$tmp" "$INSTALL_DIR/centrol"
  fi

  echo "centrol: installed to $INSTALL_DIR/centrol" >&2
  "$INSTALL_DIR/centrol" 2>&1 | head -1 >&2 || true
}

main "$@"
