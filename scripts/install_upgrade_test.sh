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
# target untouched and exit non-zero), the forward-only guard (a latest that
# moved backward is hard-refused and NOT overridable — a stray --force is an
# unknown option; an explicitly named older --version installs with a notice),
# --rollback, --check, first-install, and RequireRoot (no auto-sudo).
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
# Fail-closed must leave NO half-staged binary in the target dir either: the
# move-aside + rename is never reached, so no ${TGT}.new.* may survive.
if ls "${TGT}".new.* >/dev/null 2>&1; then no "S2 no staging (.new) residue left"; else ok "S2 no staging (.new) residue left"; fi
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
# A rollback that finds no backup must touch nothing — it must not fabricate a
# ${TGT}.bak (or any staging file) as a side effect of failing.
if [ -e "${TGT}.bak" ]; then no "S3 failed rollback creates no backup"; else ok "S3 failed rollback creates no backup"; fi
if ls "${TGT}".new.* >/dev/null 2>&1; then no "S3 failed rollback leaves no staging residue"; else ok "S3 failed rollback leaves no staging residue"; fi

# ---------------------------------------------------------------------------
# S4: forward-only guard — a resolved latest OLDER than installed is a rollback
# anomaly. Hard-refuse, and NOT overridable: --force no longer exists, so a
# stray --force is rejected as an unknown option rather than forcing the
# downgrade. The deliberate older-install path is --version (see S11).
# ---------------------------------------------------------------------------
build_release 0.1.2   # 'latest' is OLDER than the installed 0.1.5
TGT="$WORK/s4/portitor"
mkdir -p "$WORK/s4"
make_fake_binary "$TGT" 0.1.5
run_script --upgrade --target "$TGT"
if [ "$RC" -ne 0 ]; then ok "S4 latest-older hard-refused"; else no "S4 latest-older hard-refused (got 0)"; fi
check "S4 target untouched by refused latest-older" 0.1.5 "$(tgt_version "$TGT")"
case "$OUT" in *"rollback anomaly"*) ok "S4 names the rollback anomaly" ;; *) no "S4 names the rollback anomaly" ;; esac
case "$OUT" in *"not overridable"*) ok "S4 states it is not overridable" ;; *) no "S4 states it is not overridable" ;; esac
# --force is gone: it must be rejected as an unknown option, NOT accepted to
# force the downgrade (proving the anomaly refusal cannot be overridden).
run_script --upgrade --target "$TGT" --force
if [ "$RC" -ne 0 ]; then ok "S4 stray --force is rejected"; else no "S4 stray --force is rejected (got 0)"; fi
case "$OUT" in *"unknown option"*) ok "S4 --force reported as unknown option" ;; *) no "S4 --force reported as unknown option" ;; esac
check "S4 target still untouched after --force" 0.1.5 "$(tgt_version "$TGT")"

# ---------------------------------------------------------------------------
# S5: equal version — explicit 'nothing changed' notice, exit 0, unchanged, on
# BOTH the latest-equal path (VERSION unset) and the explicit-version-equal path
# (--version names the installed version). No .bak / no .new residue either way.
# ---------------------------------------------------------------------------
S5_NOTICE="portitor upgrade: already at v0.1.2; nothing changed"

build_release 0.1.2
TGT="$WORK/s5/portitor"
mkdir -p "$WORK/s5"
make_fake_binary "$TGT" 0.1.2
run_script --upgrade --target "$TGT"
check "S5 equal version (latest path) exits 0" 0 "$RC"
check "S5 target unchanged (0.1.2)" 0.1.2 "$(tgt_version "$TGT")"
if [ -e "${TGT}.bak" ]; then no "S5 no backup on a no-op"; else ok "S5 no backup on a no-op"; fi
if ls "${TGT}".new.* >/dev/null 2>&1; then no "S5 no staging (.new) residue on a no-op"; else ok "S5 no staging (.new) residue on a no-op"; fi
case "$OUT" in *"$S5_NOTICE"*) ok "S5 prints the explicit nothing-changed notice" ;; *) no "S5 prints the explicit nothing-changed notice" ;; esac

# --version naming the installed version reaches the identical notice.
TGT="$WORK/s5b/portitor"
mkdir -p "$WORK/s5b"
make_fake_binary "$TGT" 0.1.2
run_script --upgrade --target "$TGT" --version v0.1.2
check "S5 equal --version exits 0" 0 "$RC"
check "S5 --version target unchanged (0.1.2)" 0.1.2 "$(tgt_version "$TGT")"
if [ -e "${TGT}.bak" ]; then no "S5 --version no backup on a no-op"; else ok "S5 --version no backup on a no-op"; fi
if ls "${TGT}".new.* >/dev/null 2>&1; then no "S5 --version no staging (.new) residue"; else ok "S5 --version no staging (.new) residue"; fi
case "$OUT" in *"$S5_NOTICE"*) ok "S5 --version prints the identical nothing-changed notice" ;; *) no "S5 --version prints the identical nothing-changed notice" ;; esac

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
  # As root, access(2) W_OK succeeds even on a 0500 dir, so the writability probe
  # cannot fail and the refusal path is unreachable — skip with that reason.
  echo "ok   - S9 RequireRoot skipped (euid 0: W_OK succeeds on a read-only dir)"
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
  # portitor's writability guard refuses before touching the target; this test
  # proves that observable outcome — the running binary is untouched and no
  # .bak / .new.* is written into the (now-restored) target dir.
  check "S9 target untouched before any staging" 0.1.0 "$(tgt_version "$TGT")"
  if [ -e "${TGT}.bak" ]; then no "S9 no backup written before refusal"; else ok "S9 no backup written before refusal"; fi
  if ls "${TGT}".new.* >/dev/null 2>&1; then no "S9 no staging (.new) residue before refusal"; else ok "S9 no staging (.new) residue before refusal"; fi
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

# ---------------------------------------------------------------------------
# S11: explicit --version OLDER than installed — the forward-only anomaly does
# NOT apply on the named-release path. It installs the older release in that
# direction, with a clear "as explicitly requested" notice.
# ---------------------------------------------------------------------------
build_release 0.1.2   # 'latest' pointer; the pin below names an older release
build_release 0.1.1
TGT="$WORK/s11/portitor"
mkdir -p "$WORK/s11"
make_fake_binary "$TGT" 0.1.5   # installed is NEWER than the pinned 0.1.1
run_script --upgrade --target "$TGT" --version v0.1.1
check "S11 explicit older --version exits 0" 0 "$RC"
check "S11 target moved to the pinned 0.1.1" 0.1.1 "$(tgt_version "$TGT")"
case "$OUT" in *"as explicitly requested"*) ok "S11 notes the older install was requested" ;; *) no "S11 notes the older install was requested" ;; esac

# ---------------------------------------------------------------------------
# S12: --check when latest is OLDER than installed — report only. Its purpose is
# to report, so it flags the anomaly as a WARNING and STILL exits 0 (a real
# upgrade would refuse; --check never gates), changing nothing.
# ---------------------------------------------------------------------------
build_release 0.1.2   # 'latest' is OLDER than the installed 0.1.5
TGT="$WORK/s12/portitor"
mkdir -p "$WORK/s12"
make_fake_binary "$TGT" 0.1.5
run_script --upgrade --target "$TGT" --check
check "S12 --check on older latest still exits 0" 0 "$RC"
check "S12 --check changes nothing (still 0.1.5)" 0.1.5 "$(tgt_version "$TGT")"
if [ -e "${TGT}.bak" ]; then no "S12 --check writes no backup"; else ok "S12 --check writes no backup"; fi
case "$OUT" in *"anomalous"*) ok "S12 --check flags the anomaly as a warning" ;; *) no "S12 --check flags the anomaly as a warning" ;; esac

echo "# ${PASS} passed, ${FAIL} failed"
[ "$FAIL" -eq 0 ]
