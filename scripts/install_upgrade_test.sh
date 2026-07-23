#!/bin/sh
# Fake-origin harness for install.sh — exercises the REAL script end to end
# against a throwaway release origin (served over file:// via the script's
# PORTITOR_API_BASE / PORTITOR_DL_BASE origin hooks) and a throwaway signing key
# baked into a temp copy of the script (the shipped trust anchor is never
# overridden — the harness bakes a test key into its own copy, mirroring how a
# real release bakes the real key).
#
# `sh -n` proves only that the script parses; this drives the logic that
# matters: resolve -> download -> ssh-keygen -Y verify -> self-replace of a
# running binary, plus the fail-closed path (a tampered artifact must leave the
# target untouched and exit non-zero), the downgrade guard, --rollback, --check,
# first-install, and RequireRoot (no auto-sudo).
#
# The self-replace scenarios assert the observable outcome of move-aside +
# rename (the displaced binary is kept as <target>.bak, the new one is swapped
# in). ETXTBSY-avoidance is the *reason* the script uses rename(2) rather than a
# write-in-place; it cannot be reproduced with an interpreted fake binary (a
# shell script is not mapped as program text), so it is guaranteed by design,
# not by this harness.
#
# Exit 0 iff every scenario passes.

set -eu

OWNER="dmitriyb"
REPO="portitor"

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
INSTALL_SH="${SCRIPT_DIR}/../install.sh"
[ -f "$INSTALL_SH" ] || { echo "harness: cannot find install.sh at $INSTALL_SH" >&2; exit 1; }

for tool in curl ssh-keygen tar mktemp uname cut awk sed; do
  command -v "$tool" >/dev/null 2>&1 || { echo "harness: '$tool' is required" >&2; exit 1; }
done

case "$(uname -s)" in
  Linux) GOOS=linux ;;
  Darwin) GOOS=darwin ;;
  *) echo "harness: unsupported OS $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) GOARCH=amd64 ;;
  arm64 | aarch64) GOARCH=arm64 ;;
  *) echo "harness: unsupported arch $(uname -m)" >&2; exit 1 ;;
esac

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT INT TERM
ORIGIN="$WORK/origin"
mkdir -p "$ORIGIN"

# --- throwaway signer + a test copy of install.sh with its pubkey baked in ---
SIGNER_KEY="$WORK/signer"
ssh-keygen -t ed25519 -f "$SIGNER_KEY" -N "" -C "test signing" -q
PUB=$(cat "${SIGNER_KEY}.pub")

TESTSH="$WORK/install_under_test.sh"
awk -v pub="$PUB" '
  /^SIGNING_PUBKEY=/ { print "SIGNING_PUBKEY=\"" pub "\""; next }
  { print }
' "$INSTALL_SH" > "$TESTSH"
grep -q "^SIGNING_PUBKEY=\"$(printf '%s' "$PUB" | cut -d' ' -f1) " "$TESTSH" \
  || { echo "harness: failed to bake the test signing key into the script copy" >&2; exit 1; }

# --- tiny assertion framework ---
PASS=0
FAIL=0
ok() { PASS=$((PASS + 1)); echo "ok   - $1"; }
no() {
  FAIL=$((FAIL + 1))
  echo "FAIL - $1"
  # Surface the failing scenario's captured script output — the harness runs the
  # real install.sh, and without this a fail-closed exit hides the actual error
  # (e.g. an install/curl/tar diagnostic) behind a bare exit code.
  if [ -n "${OUT:-}" ]; then
    printf '%s\n' "$OUT" | sed 's/^/       script| /'
  fi
}
check() { # check <desc> <expected> <actual>
  if [ "$2" = "$3" ]; then ok "$1"; else no "$1 (want [$2], got [$3])"; fi
}

# make_fake_binary DEST VER — a stand-in 'portitor' that prints the version line.
make_fake_binary() {
  cat > "$1" <<EOF
#!/bin/sh
case "\$1" in
  version) echo "portitor $2 (commit test, built test)" ;;
esac
EOF
  chmod 755 "$1"
}

tgt_version() { "$1" version 2>/dev/null | head -n1 | sed -n 's/^portitor \([^ ]*\).*/\1/p'; }

# build_release VER [tamper] — stage a signed archive for VER under the origin,
# and point the 'latest' API pointer at it. With 'tamper', the archive is
# corrupted AFTER signing so verification must fail.
build_release() {
  _ver="$1"
  _mode="${2:-}"
  _tag="v${_ver}"
  _dldir="$ORIGIN/$OWNER/$REPO/releases/download/$_tag"
  mkdir -p "$_dldir"
  _stage="$WORK/stage-$_ver"
  rm -rf "$_stage"
  mkdir -p "$_stage"
  make_fake_binary "$_stage/portitor" "$_ver"
  _arc="portitor_${_ver}_${GOOS}_${GOARCH}.tar.gz"
  # Rebuild from clean. ssh-keygen -Y sign PROMPTS "Overwrite (y/n)?" when
  # <arc>.sig already exists and, given no stdin (go test), reads EOF and
  # declines — silently leaving a STALE signature from an earlier build of the
  # same version. The rebuilt archive's gzip mtime differs, so that stale sig
  # then fails verification (flaky, depending on whether a rebuild crossed a
  # wall-clock second). Removing both first makes the signature always match the
  # archive just written; the `|| exit` stops masking a genuine signing failure.
  rm -f "$_dldir/$_arc" "$_dldir/$_arc.sig"
  tar -czf "$_dldir/$_arc" -C "$_stage" portitor
  ssh-keygen -Y sign -f "$SIGNER_KEY" -n file "$_dldir/$_arc" >/dev/null 2>&1 \
    || { echo "harness: signing $_arc failed" >&2; exit 1; }
  if [ "$_mode" = tamper ]; then
    printf 'tampered' >> "$_dldir/$_arc"
  fi
  mkdir -p "$ORIGIN/repos/$OWNER/$REPO/releases"
  printf '{"tag_name":"%s"}\n' "$_tag" > "$ORIGIN/repos/$OWNER/$REPO/releases/latest"
}

# run_script — invoke the script under test with the origin hooks set; captures
# OUT and RC without tripping set -e.
run_script() {
  OUT=$(PORTITOR_API_BASE="file://$ORIGIN" PORTITOR_DL_BASE="file://$ORIGIN" \
    sh "$TESTSH" "$@" 2>&1) && RC=0 || RC=$?
}

echo "# install.sh fake-origin harness (${GOOS}/${GOARCH})"

# ---------------------------------------------------------------------------
# S1: happy upgrade — resolve latest, download, verify, self-replace.
# ---------------------------------------------------------------------------
build_release 0.1.2
TGT="$WORK/s1/portitor"
mkdir -p "$WORK/s1"
make_fake_binary "$TGT" 0.1.0
run_script --upgrade --target "$TGT"
check "S1 upgrade exits 0" 0 "$RC"
check "S1 target is now 0.1.2" 0.1.2 "$(tgt_version "$TGT")"
if [ -e "${TGT}.bak" ]; then ok "S1 backup left for rollback"; else no "S1 backup left for rollback"; fi
check "S1 backup holds the prior 0.1.0" 0.1.0 "$(tgt_version "${TGT}.bak")"
if [ -e "${TGT}.new.$$" ]; then no "S1 no staging file left behind"; else ok "S1 no staging file left behind"; fi

# ---------------------------------------------------------------------------
# S2: fail-closed — a tampered artifact must NOT replace the running binary.
# ---------------------------------------------------------------------------
build_release 0.1.3 tamper
TGT="$WORK/s2/portitor"
mkdir -p "$WORK/s2"
make_fake_binary "$TGT" 0.1.0
run_script --upgrade --target "$TGT"
if [ "$RC" -ne 0 ]; then ok "S2 tampered artifact exits non-zero"; else no "S2 tampered artifact exits non-zero (got 0)"; fi
check "S2 running binary untouched (still 0.1.0)" 0.1.0 "$(tgt_version "$TGT")"
if [ -e "${TGT}.bak" ]; then no "S2 no backup created (never reached replace)"; else ok "S2 no backup created (never reached replace)"; fi
case "$OUT" in *"verification FAILED"*) ok "S2 reports a verification failure" ;; *) no "S2 reports a verification failure" ;; esac

# ---------------------------------------------------------------------------
# S3: --rollback restores the pre-upgrade binary.
# ---------------------------------------------------------------------------
build_release 0.1.2
TGT="$WORK/s3/portitor"
mkdir -p "$WORK/s3"
make_fake_binary "$TGT" 0.1.0
run_script --upgrade --target "$TGT"
check "S3 pre-roll upgrade exits 0" 0 "$RC"
check "S3 upgraded to 0.1.2" 0.1.2 "$(tgt_version "$TGT")"
run_script --upgrade --target "$TGT" --rollback
check "S3 rollback exits 0" 0 "$RC"
check "S3 restored to 0.1.0" 0.1.0 "$(tgt_version "$TGT")"

# rollback with no backup present must fail cleanly.
TGT="$WORK/s3b/portitor"
mkdir -p "$WORK/s3b"
make_fake_binary "$TGT" 0.1.0
run_script --upgrade --target "$TGT" --rollback
if [ "$RC" -ne 0 ]; then ok "S3 rollback without a backup fails" ; else no "S3 rollback without a backup fails (got 0)"; fi
check "S3 target left untouched by failed rollback" 0.1.0 "$(tgt_version "$TGT")"

# ---------------------------------------------------------------------------
# S4: downgrade guard — refuse an older release unless --force.
# ---------------------------------------------------------------------------
build_release 0.1.2   # 'latest' is OLDER than the installed 0.1.5
TGT="$WORK/s4/portitor"
mkdir -p "$WORK/s4"
make_fake_binary "$TGT" 0.1.5
run_script --upgrade --target "$TGT"
if [ "$RC" -ne 0 ]; then ok "S4 downgrade refused without --force"; else no "S4 downgrade refused without --force (got 0)"; fi
check "S4 target untouched by refused downgrade" 0.1.5 "$(tgt_version "$TGT")"
run_script --upgrade --target "$TGT" --force
check "S4 downgrade proceeds with --force" 0 "$RC"
check "S4 target moved to 0.1.2 under --force" 0.1.2 "$(tgt_version "$TGT")"

# ---------------------------------------------------------------------------
# S5: equal version — 'already up to date', exit 0, unchanged.
# ---------------------------------------------------------------------------
build_release 0.1.2
TGT="$WORK/s5/portitor"
mkdir -p "$WORK/s5"
make_fake_binary "$TGT" 0.1.2
run_script --upgrade --target "$TGT"
check "S5 equal version exits 0" 0 "$RC"
check "S5 target unchanged (0.1.2)" 0.1.2 "$(tgt_version "$TGT")"
if [ -e "${TGT}.bak" ]; then no "S5 no backup on a no-op"; else ok "S5 no backup on a no-op"; fi
case "$OUT" in *"already up to date"*) ok "S5 says already up to date" ;; *) no "S5 says already up to date" ;; esac

# ---------------------------------------------------------------------------
# S6: --check / --dry-run reports and changes nothing.
# ---------------------------------------------------------------------------
build_release 0.1.2
TGT="$WORK/s6/portitor"
mkdir -p "$WORK/s6"
make_fake_binary "$TGT" 0.1.0
run_script --upgrade --target "$TGT" --check
check "S6 --check exits 0" 0 "$RC"
check "S6 --check changes nothing (still 0.1.0)" 0.1.0 "$(tgt_version "$TGT")"
if [ -e "${TGT}.bak" ]; then no "S6 --check writes no backup"; else ok "S6 --check writes no backup"; fi
case "$OUT" in *"update available"*) ok "S6 --check reports an available update" ;; *) no "S6 --check reports an available update" ;; esac

# ---------------------------------------------------------------------------
# S7: --version pins a specific (older, but explicitly requested) release.
# ---------------------------------------------------------------------------
build_release 0.1.1
build_release 0.1.2   # 'latest' pointer now says 0.1.2, but we pin 0.1.1
TGT="$WORK/s7/portitor"
mkdir -p "$WORK/s7"
make_fake_binary "$TGT" 0.1.0
run_script --upgrade --target "$TGT" --version v0.1.1
check "S7 pinned upgrade exits 0" 0 "$RC"
check "S7 target is the pinned 0.1.1" 0.1.1 "$(tgt_version "$TGT")"

# ---------------------------------------------------------------------------
# S8: first-install path (no flags) still installs into INSTALL_DIR.
# ---------------------------------------------------------------------------
build_release 0.1.2
INSTALL_DIR="$WORK/s8/bin"
mkdir -p "$INSTALL_DIR"
OUT=$(PORTITOR_API_BASE="file://$ORIGIN" PORTITOR_DL_BASE="file://$ORIGIN" \
  INSTALL_DIR="$INSTALL_DIR" sh "$TESTSH" 2>&1) && RC=0 || RC=$?
check "S8 first-install exits 0" 0 "$RC"
check "S8 installed binary is 0.1.2" 0.1.2 "$(tgt_version "$INSTALL_DIR/portitor")"

# ---------------------------------------------------------------------------
# S9: RequireRoot — an unwritable target dir must be reported, never auto-sudo.
# Skipped under root (uid 0), where the writability probe cannot fail.
# ---------------------------------------------------------------------------
if [ "$(id -u)" -eq 0 ]; then
  echo "ok   - S9 RequireRoot skipped (running as root)"
  PASS=$((PASS + 1))
else
  build_release 0.1.2
  RODIR="$WORK/s9/ro"
  mkdir -p "$RODIR"
  TGT="$RODIR/portitor"
  make_fake_binary "$TGT" 0.1.0
  chmod 0500 "$RODIR"
  run_script --upgrade --target "$TGT"
  chmod 0700 "$RODIR" # restore so cleanup can remove it
  if [ "$RC" -ne 0 ]; then ok "S9 unwritable target dir refused"; else no "S9 unwritable target dir refused (got 0)"; fi
  case "$OUT" in
    *"elevated privileges"*) ok "S9 asks for elevated privileges" ;;
    *) no "S9 asks for elevated privileges" ;;
  esac
  case "$OUT" in
    *sudo*retrying*) no "S9 must NOT auto-sudo in upgrade mode" ;;
    *) ok "S9 does not auto-sudo in upgrade mode" ;;
  esac
fi

# ---------------------------------------------------------------------------
# S10: --current overrides the probed version (the Go command always passes its
# compiled-in version as --current). The target *reports* 0.1.0, but --current
# claims 0.1.5, so a 0.1.2 latest must be treated as a downgrade and refused.
# ---------------------------------------------------------------------------
build_release 0.1.2
TGT="$WORK/s10/portitor"
mkdir -p "$WORK/s10"
make_fake_binary "$TGT" 0.1.0
run_script --upgrade --target "$TGT" --current 0.1.5
if [ "$RC" -ne 0 ]; then ok "S10 --current takes precedence over the probe"; else no "S10 --current takes precedence over the probe (got 0)"; fi
check "S10 target untouched" 0.1.0 "$(tgt_version "$TGT")"

echo "# ${PASS} passed, ${FAIL} failed"
[ "$FAIL" -eq 0 ]
