#!/bin/sh
# install-centrol.sh — installs the centrol CLI and the centrol-verify
# reference verifier to /usr/local/bin (or $CENTROL_INSTALL_DIR if set).
# No telemetry, no account, no network access beyond fetching this
# release's binaries and its SHA256SUMS file. Both binaries are checked
# against SHA256SUMS before either is installed. sudo is used only for
# the final move, and only when the install directory is not writable.
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

# install_one TMP_FILE NAME — moves a verified binary into INSTALL_DIR.
install_one() {
  if [ -w "$INSTALL_DIR" ]; then
    mv "$1" "$INSTALL_DIR/$2"
  else
    echo "centrol: installing to $INSTALL_DIR requires sudo" >&2
    sudo mv "$1" "$INSTALL_DIR/$2"
  fi
}

main() {
  platform_os="$(os)"
  platform_arch="$(arch)"
  names="centrol centrol-verify"
  base_url="https://github.com/${REPO}/releases/latest/download"

  tmp_dir="$(mktemp -d)"
  trap 'rm -rf "$tmp_dir"' EXIT
  tmp_sums="${tmp_dir}/SHA256SUMS"

  fetch "${base_url}/SHA256SUMS" "$tmp_sums"

  # Download and verify everything first; install only if all of it checks out.
  for name in $names; do
    asset="${name}-${platform_os}-${platform_arch}"
    echo "centrol: downloading ${asset}..." >&2
    fetch "${base_url}/${asset}" "${tmp_dir}/${asset}"

    expected="$(awk -v f="$asset" '$2 == f { print $1 }' "$tmp_sums")"
    if [ -z "$expected" ]; then
      echo "centrol: ${asset} not listed in SHA256SUMS — refusing to install an unverifiable binary" >&2
      exit 1
    fi

    actual="$(sha256_of "${tmp_dir}/${asset}")"
    if [ "$actual" != "$expected" ]; then
      echo "centrol: checksum mismatch for ${asset}" >&2
      echo "  expected: $expected" >&2
      echo "  actual:   $actual" >&2
      echo "centrol: refusing to install a binary that doesn't match its published checksum" >&2
      exit 1
    fi
    echo "centrol: checksum OK (${asset})" >&2
    chmod +x "${tmp_dir}/${asset}"
  done

  for name in $names; do
    install_one "${tmp_dir}/${name}-${platform_os}-${platform_arch}" "$name"
    echo "centrol: installed to $INSTALL_DIR/$name" >&2
  done

  case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *) echo "centrol: $INSTALL_DIR is not on your PATH; add it to run centrol and centrol-verify by name" >&2 ;;
  esac
  "$INSTALL_DIR/centrol" 2>&1 | head -1 >&2 || true
}

main "$@"
