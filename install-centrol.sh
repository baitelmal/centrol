#!/bin/sh
# install-centrol.sh — installs the centrol CLI to /usr/local/bin (or
# $CENTROL_INSTALL_DIR if set). No telemetry, no account, no network
# access beyond fetching this release's binary and its SHA256SUMS file.
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

# fetch URL DEST — downloads with curl if available, falling back to wget.
fetch() {
  url="$1"
  dest="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" -o "$dest"
  elif command -v wget >/dev/null 2>&1; then
    wget -q "$url" -O "$dest"
  else
    echo "centrol: need curl or wget to install" >&2
    exit 1
  fi
}

# sha256_of FILE — prints the file's SHA-256 hex digest. Linux ships
# sha256sum (GNU coreutils); macOS does not — it has shasum instead.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    echo "centrol: need sha256sum or shasum to verify the download" >&2
    exit 1
  fi
}

main() {
  platform_os="$(os)"
  platform_arch="$(arch)"
  asset="centrol-${platform_os}-${platform_arch}"
  base_url="https://github.com/${REPO}/releases/latest/download"

  tmp_dir="$(mktemp -d)"
  trap 'rm -rf "$tmp_dir"' EXIT
  tmp_bin="${tmp_dir}/${asset}"
  tmp_sums="${tmp_dir}/SHA256SUMS"

  echo "centrol: downloading ${asset}..." >&2
  fetch "${base_url}/${asset}" "$tmp_bin"

  echo "centrol: verifying checksum..." >&2
  fetch "${base_url}/SHA256SUMS" "$tmp_sums"

  expected="$(awk -v f="$asset" '$2 == f { print $1 }' "$tmp_sums")"
  if [ -z "$expected" ]; then
    echo "centrol: ${asset} not listed in SHA256SUMS — refusing to install an unverifiable binary" >&2
    exit 1
  fi

  actual="$(sha256_of "$tmp_bin")"
  if [ "$actual" != "$expected" ]; then
    echo "centrol: checksum mismatch for ${asset}" >&2
    echo "  expected: $expected" >&2
    echo "  actual:   $actual" >&2
    echo "centrol: refusing to install a binary that doesn't match its published checksum" >&2
    exit 1
  fi
  echo "centrol: checksum OK" >&2

  chmod +x "$tmp_bin"
  if [ -w "$INSTALL_DIR" ]; then
    mv "$tmp_bin" "$INSTALL_DIR/centrol"
  else
    echo "centrol: installing to $INSTALL_DIR requires sudo" >&2
    sudo mv "$tmp_bin" "$INSTALL_DIR/centrol"
  fi

  echo "centrol: installed to $INSTALL_DIR/centrol" >&2
  "$INSTALL_DIR/centrol" 2>&1 | head -1 >&2 || true
}

main "$@"
