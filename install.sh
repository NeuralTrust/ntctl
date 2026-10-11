#!/bin/sh
# Installs ntctl from GitHub Releases.
#
#   curl -sSfL https://raw.githubusercontent.com/NeuralTrust/ntctl/main/install.sh | sh
#
# Environment:
#   NTCTL_VERSION            release tag to install, e.g. v0.1.0 (default: latest)
#   NTCTL_INSTALL_DIR        where to put the binary (default: /usr/local/bin if
#                            writable, else ~/.local/bin)
#   NTCTL_REQUIRE_SIGNATURE  set to 1 to fail when cosign is not installed
#
# Every download is checked against checksums.txt. When cosign is installed, the
# signature on checksums.txt is verified too: it must come from this
# repository's release workflow.
set -eu

repo="NeuralTrust/ntctl"
version="${NTCTL_VERSION:-latest}"
base="${NTCTL_BASE_URL:-https://github.com/${repo}/releases}"
identity="^https://github.com/${repo}/\.github/workflows/release\.yml@refs/tags/v"
issuer="https://token.actions.githubusercontent.com"

fail() { printf 'ntctl install: %s\n' "$1" >&2; exit 1; }

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) fail "unsupported OS $(uname -s); on Windows download ntctl_windows_amd64.zip from ${base}" ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) fail "unsupported architecture $(uname -m)" ;;
esac

if [ "$version" = latest ]; then
  url="${base}/latest/download"
else
  url="${base}/download/${version}"
fi
archive="ntctl_${os}_${arch}.tar.gz"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fetch() { curl -sSfL --retry 3 -o "$tmp/$1" "$url/$1" || fail "could not download $url/$1"; }
fetch "$archive"
fetch checksums.txt

expected="$(awk -v f="$archive" '$2 == f {print $1}' "$tmp/checksums.txt")"
[ -n "$expected" ] || fail "$archive is not listed in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$tmp/$archive" | awk '{print $1}')"
else
  actual="$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')"
fi
[ "$expected" = "$actual" ] || fail "checksum mismatch for $archive; refusing to install"

if command -v cosign >/dev/null 2>&1; then
  fetch checksums.txt.sigstore.json
  cosign verify-blob --bundle "$tmp/checksums.txt.sigstore.json" \
    --certificate-identity-regexp "$identity" --certificate-oidc-issuer "$issuer" \
    "$tmp/checksums.txt" >/dev/null 2>&1 || fail "signature verification failed; refusing to install"
  echo "Signature verified."
elif [ "${NTCTL_REQUIRE_SIGNATURE:-0}" = 1 ]; then
  fail "cosign is not installed and NTCTL_REQUIRE_SIGNATURE=1"
else
  echo "Checksum verified. Install cosign to verify the signature as well."
fi

tar -xzf "$tmp/$archive" -C "$tmp" ntctl

dir="${NTCTL_INSTALL_DIR:-}"
if [ -z "$dir" ]; then
  if [ -w /usr/local/bin ]; then dir=/usr/local/bin; else dir="$HOME/.local/bin"; fi
fi
mkdir -p "$dir"
install -m 0755 "$tmp/ntctl" "$dir/ntctl"
echo "Installed $("$dir/ntctl" version 2>/dev/null | head -1 || echo ntctl) to $dir/ntctl"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "Add $dir to your PATH to run ntctl." ;;
esac
