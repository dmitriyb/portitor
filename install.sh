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
# It has two modes:
#
#   * first-install (default, no flags) — install into INSTALL_DIR, exactly as
#     documented in the README's install block.
#   * upgrade (--upgrade --target <path>) — replace an already-installed binary
#     in place, safely (move-aside + rename, never a write over the running
#     file), with a downgrade guard and a rollback path. `portitor upgrade`
#     embeds this same script and drives it in this mode; the embedded copy is,
#     for a given release, byte-identical to the standalone install.sh.
#
# Usage:
#   sh install.sh                            install the latest release
#   VERSION=v0.1.0 sh install.sh             install a specific release
#   INSTALL_DIR=~/bin sh install.sh          install somewhere other than /usr/local/bin
#   sh install.sh --upgrade --target <path>  replace the binary at <path> in place
#   sh install.sh --upgrade --target <path> --check     report current vs latest only
#   sh install.sh --upgrade --target <path> --rollback  restore <path>.bak
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

# Origin bases. These default to the production GitHub endpoints; the
# PORTITOR_API_BASE / PORTITOR_DL_BASE overrides exist ONLY so the fake-origin
# test harness (scripts/install_upgrade_test.sh) can drive the real resolve /
# download / verify / self-replace logic against a throwaway origin. They do
# not touch the trust anchor: whatever origin is used, the downloaded archive
# must still verify against SIGNING_PUBKEY below, so redirecting the origin can
# never yield an installed binary that was not signed by the release key. In a
# released, signed copy of this script these variables are unset, so production
# behaviour is unchanged.
API_BASE="${PORTITOR_API_BASE:-https://api.github.com}"
DL_BASE="${PORTITOR_DL_BASE:-https://github.com}"

fail() {
  echo "install.sh: error: $*" >&2
  exit 1
}

need() {
  command -v "$1" >/dev/null 2>&1 || fail "'$1' is required but not found on PATH"
}

# require_writable reports an unwritable target directory and stops. Upgrade mode
# deliberately does NOT self-invoke sudo (unlike first-install): silently
# re-running a self-replace as root is a surprise, so we detect the condition
# and tell the operator to re-run with elevated privileges instead.
require_writable() {
  fail "$1 is not writable by the current user — re-run with elevated privileges (e.g. run 'portitor upgrade' as the owner of $1, or via sudo)"
}

# binary_version PATH -> prints the version token from `<PATH> version`
# (the "0.1.0" out of "portitor 0.1.0 (commit …, built …)"), or nothing if the
# binary cannot be run or does not print the expected line.
binary_version() {
  _line=$("$1" version 2>/dev/null | head -n1) || _line=""
  _v="${_line#portitor }"
  _v="${_v%% *}"
  printf '%s' "$_v"
}

# ver_cmp A B -> prints -1 if A<B, 0 if A==B, 1 if A>B, comparing the numeric
# major.minor.patch fields only (a leading v and any -pre/+build suffix are
# stripped; non-numeric fields are treated as 0). Sufficient for portitor's
# vMAJOR.MINOR.PATCH release tags; deliberately not a full semver comparator.
ver_cmp() {
  _a="${1#v}"; _b="${2#v}"
  _a="${_a%%[-+]*}"; _b="${_b%%[-+]*}"
  _i=1
  while [ "$_i" -le 3 ]; do
    _x=$(printf '%s' "$_a" | cut -d. -f"$_i")
    _y=$(printf '%s' "$_b" | cut -d. -f"$_i")
    case "$_x" in ''|*[!0-9]*) _x=0 ;; esac
    case "$_y" in ''|*[!0-9]*) _y=0 ;; esac
    if [ "$_x" -lt "$_y" ]; then echo -1; return 0; fi
    if [ "$_x" -gt "$_y" ]; then echo 1; return 0; fi
    _i=$((_i + 1))
  done
  echo 0
}

# self_replace TARGET NEWBIN — swap NEWBIN into TARGET's path using move-aside +
# rename. rename(2) over a running binary is safe (the running process keeps its
# inode; the next invocation gets the new file), whereas install/cp over it
# truncates in place and hits ETXTBSY on Linux — so the new binary is staged in
# TARGET's own directory (same filesystem → the final swap is an atomic rename)
# and never written over TARGET directly. The displaced binary is kept as
# TARGET.bak for --rollback; on any mid-replace failure it is restored, so the
# on-disk binary is never left missing or half-updated.
self_replace() {
  _t="$1"
  _src="$2"
  _dir=$(dirname "$_t")
  [ -w "$_dir" ] || require_writable "$_dir"
  _bak="${_t}.bak"
  _new="${_t}.new.$$"
  rm -f "$_new"
  cp "$_src" "$_new" || fail "could not stage the new binary at ${_new}"
  chmod 0755 "$_new" || { rm -f "$_new"; fail "could not chmod ${_new}"; }
  if [ -e "$_t" ]; then
    mv -f "$_t" "$_bak" || { rm -f "$_new"; fail "could not move ${_t} aside"; }
  fi
  if ! mv -f "$_new" "$_t"; then
    [ -e "$_bak" ] && mv -f "$_bak" "$_t" 2>/dev/null || true
    rm -f "$_new"
    fail "could not install the new binary at ${_t}; restored the previous one"
  fi
}

# do_rollback TARGET — restore TARGET.bak over TARGET (atomic rename). Used by
# --rollback and standalone-usable to undo a prior upgrade.
do_rollback() {
  _t="$1"
  _bak="${_t}.bak"
  [ -e "$_bak" ] || fail "no backup at ${_bak} to roll back to"
  _dir=$(dirname "$_t")
  [ -w "$_dir" ] || require_writable "$_dir"
  mv -f "$_bak" "$_t" || fail "rollback failed: could not restore ${_bak} -> ${_t}"
  _rv=$(binary_version "$_t")
  echo "portitor: rolled back ${_t} to ${_rv:-the previous binary}" >&2
}

# --- parse flags (first-install mode takes none; upgrade mode is opt-in) ---
MODE=install
CHECK=0
FORCE=0
ROLLBACK=0
TARGET=""
CURRENT=""

while [ $# -gt 0 ]; do
  case "$1" in
    --upgrade) MODE=upgrade ;;
    --target) shift; TARGET="${1:-}" ;;
    --target=*) TARGET="${1#--target=}" ;;
    --check | --dry-run) CHECK=1 ;;
    --force) FORCE=1 ;;
    --rollback) ROLLBACK=1 ;;
    --current) shift; CURRENT="${1:-}" ;;
    --current=*) CURRENT="${1#--current=}" ;;
    --version) shift; VERSION="${1:-}" ;;
    --version=*) VERSION="${1#--version=}" ;;
    --) shift; break ;;
    -*) fail "unknown option: $1" ;;
    *) fail "unexpected argument: $1" ;;
  esac
  shift
done

need curl
need ssh-keygen
need tar
need mktemp
need uname

# --- rollback short-circuits: no download or verification needed ---
if [ "$ROLLBACK" -eq 1 ]; then
  [ "$MODE" = upgrade ] || fail "--rollback is only valid with --upgrade"
  [ -n "$TARGET" ] || fail "--rollback requires --target <path>"
  do_rollback "$TARGET"
  exit 0
fi

# --- resolve the release tag ---
if [ -n "${VERSION:-}" ]; then
  tag="$VERSION"
else
  api_url="${API_BASE}/repos/${OWNER}/${REPO}/releases/latest"
  # grep (no -m1) + head: read the body to EOF so curl can't abort with (56).
  tag=$(curl -fsSL "$api_url" | grep '"tag_name"' | head -n1 | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')
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

# --- upgrade mode: decide whether to proceed BEFORE downloading anything ---
if [ "$MODE" = upgrade ]; then
  need cut
  [ -n "$TARGET" ] || fail "--upgrade requires --target <path> (the binary to replace)"
  cur="$CURRENT"
  [ -n "$cur" ] || cur=$(binary_version "$TARGET")
  cmp=""
  [ -n "$cur" ] && cmp=$(ver_cmp "$version" "$cur")

  if [ "$CHECK" -eq 1 ]; then
    # --check / --dry-run: report only, change nothing on disk.
    if [ -z "$cur" ]; then
      echo "portitor: latest ${version} (current version could not be determined)" >&2
    elif [ "$cmp" -eq 0 ]; then
      echo "portitor: up to date (${cur})" >&2
    elif [ "$cmp" -gt 0 ]; then
      echo "portitor: update available: ${cur} -> ${version}" >&2
    else
      echo "portitor: current ${cur} is newer than latest ${version}" >&2
    fi
    exit 0
  fi

  if [ -n "$cur" ]; then
    if [ "$cmp" -eq 0 ]; then
      echo "portitor: already up to date (${cur})" >&2
      exit 0
    fi
    if [ "$cmp" -lt 0 ] && [ "$FORCE" -ne 1 ]; then
      fail "refusing to downgrade ${cur} -> ${version}: a signature proves authenticity, not freshness — pass --force to override"
    fi
  else
    echo "portitor: warning: could not determine the current version of ${TARGET}; proceeding without the downgrade guard" >&2
  fi

  # Fail fast on an unwritable target directory, before spending a download.
  _tdir=$(dirname "$TARGET")
  [ -w "$_tdir" ] || require_writable "$_tdir"
elif [ "$CHECK" -eq 1 ]; then
  # --check without --upgrade: report the resolved latest, install nothing.
  echo "portitor: latest ${version}" >&2
  exit 0
fi

archive="portitor_${version}_${goos}_${goarch}.tar.gz"
base_url="${DL_BASE}/${OWNER}/${REPO}/releases/download/${tag}"

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

if [ "$MODE" = upgrade ]; then
  self_replace "$TARGET" "${workdir}/portitor"
  echo "portitor: upgraded ${cur:-unknown} -> ${version} at ${TARGET}" >&2
  "$TARGET" version >&2 || true
else
  if [ -w "$INSTALL_DIR" ]; then
    install -m 0755 "${workdir}/portitor" "${INSTALL_DIR}/portitor"
  else
    echo "portitor installer: ${INSTALL_DIR} is not writable, retrying with sudo" >&2
    need sudo
    sudo install -m 0755 "${workdir}/portitor" "${INSTALL_DIR}/portitor"
  fi
  echo "portitor installed to ${INSTALL_DIR}/portitor" >&2
  "${INSTALL_DIR}/portitor" version || true
fi
