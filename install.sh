#!/bin/sh
# portitor installer.
#
# Resolves the latest portitor release (or VERSION, if set), downloads the
# matching binary archive and its SSHSIG signature, verifies the signature
# against the embedded release-signing public key, and installs the binary.
#
# This script's own content is version-agnostic: it does not change between
# releases, so it carries the same signature on every release it is attached
# to. It is meant to be downloaded and verified BEFORE it runs — see the
# README's install section for the verified copy-paste block. Running it
# via a pipe (curl ... | sh) skips that verification step and is not the
# supported path.
#
# Usage:
#   sh install.sh                    install the latest release
#   VERSION=v0.1.0 sh install.sh     install a specific release
#   INSTALL_DIR=~/bin sh install.sh  install somewhere other than /usr/local/bin
#
# Fails closed: if the download or the signature verification does not
# succeed, this script exits non-zero without installing anything.

set -eu

OWNER="dmitriyb"
REPO="portitor"

# The principal string used both here and in the README's allowed_signers
# line — it must match exactly, or verification fails even with the right
# key (ssh-keygen -Y verify checks -I against the allowed_signers entry).
SIGNER_ID="dvbozhko@gmail.com"

SIGNING_PUBKEY="ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing"

INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

fail() {
  echo "install.sh: error: $*" >&2
  exit 1
}

need() {
  command -v "$1" >/dev/null 2>&1 || fail "'$1' is required but not found on PATH"
}

need curl
need ssh-keygen
need tar
need mktemp
need uname

# --- resolve the release tag ---
if [ -n "${VERSION:-}" ]; then
  tag="$VERSION"
else
  api_url="https://api.github.com/repos/${OWNER}/${REPO}/releases/latest"
  tag=$(curl -fsSL "$api_url" | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')
  [ -n "$tag" ] || fail "could not resolve the latest release tag from $api_url"
fi
version="${tag#v}"

# --- detect OS/arch (portitor ships linux and darwin, amd64 and arm64) ---
os=$(uname -s)
case "$os" in
  Linux) goos=linux ;;
  Darwin) goos=darwin ;;
  *) fail "unsupported OS: $os (portitor ships linux and darwin binaries only)" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) goarch=amd64 ;;
  arm64 | aarch64) goarch=arm64 ;;
  *) fail "unsupported architecture: $arch (portitor ships amd64 and arm64 binaries only)" ;;
esac

archive="portitor_${version}_${goos}_${goarch}.tar.gz"
base_url="https://github.com/${OWNER}/${REPO}/releases/download/${tag}"

workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT INT TERM

echo "portitor installer: fetching ${archive} (${tag})" >&2
curl -fsSL "${base_url}/${archive}" -o "${workdir}/${archive}" \
  || fail "download failed: ${base_url}/${archive}"
curl -fsSL "${base_url}/${archive}.sig" -o "${workdir}/${archive}.sig" \
  || fail "download failed: ${base_url}/${archive}.sig"

allowed_signers="${workdir}/allowed_signers"
printf '%s %s\n' "$SIGNER_ID" "$SIGNING_PUBKEY" >"$allowed_signers"

if ! ssh-keygen -Y verify \
  -f "$allowed_signers" \
  -I "$SIGNER_ID" \
  -n file \
  -s "${workdir}/${archive}.sig" \
  <"${workdir}/${archive}" >&2; then
  fail "signature verification FAILED for ${archive} — refusing to install an unverified binary"
fi
echo "portitor installer: signature verified" >&2

tar -xzf "${workdir}/${archive}" -C "$workdir" portitor \
  || fail "failed to extract the portitor binary from ${archive}"
chmod 755 "${workdir}/portitor"

if [ -w "$INSTALL_DIR" ]; then
  install -m 0755 "${workdir}/portitor" "${INSTALL_DIR}/portitor"
else
  echo "portitor installer: ${INSTALL_DIR} is not writable, retrying with sudo" >&2
  need sudo
  sudo install -m 0755 "${workdir}/portitor" "${INSTALL_DIR}/portitor"
fi

echo "portitor installed to ${INSTALL_DIR}/portitor" >&2
"${INSTALL_DIR}/portitor" version || true
