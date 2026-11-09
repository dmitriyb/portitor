#!/usr/bin/env bash
# run.sh — launch the portitor container, sharing no host path with it.
#
# Reads portitor's GitHub PAT from the OS keychain (override with GH_TOKEN), bakes
# the config dir into a derived image (/etc/portitor, used by its image ID), checks
# every repo config with `portitor validate-config` as the gate's own user inside that
# image, creates the container from it, copies the PAT to /run/secrets/gh_token
# inside, and starts it.
# The PAT never appears on a command line, in the container's env, or in an image
# layer; the agent/role public keys go in as AGENT_AUTHORIZED_KEY.
#
#   deploy/run.sh \
#     --config-dir "$HOME/.dca-keys/portitor-config" \   # holds repos.d/<repo>.json + allowed_signers + anything they reference
#     --keys "$HOME/.dca-keys/implementer.pub,$HOME/.dca-keys/reviewer.pub,$HOME/.dca-keys/merger.pub" \
#     --name portitor --image portitor --volume portitor-repos
#
# Bind roles in the config dir BEFORE running this (see docs/deploy.md): the gate
# reads its config from the image, so a change reaches it by re-running run.sh.
#
# portitor needs both GitHub egress (default bridge) and the agent's internal
# network, so run it on the bridge (default) then attach dca-net:
#   docker network connect dca-net portitor
#
# After it is up, provision the mirror once (config read from the registry,
# /etc/portitor/repos.d/<repo>.json — the single canonical config identity):
#   docker exec -u git portitor portitor add-repo --repo <repo> \
#     --upstream https://github.com/<owner>/<repo>.git
set -euo pipefail

die() { echo "run.sh: $*" >&2; exit 1; }

NAME=portitor IMAGE=portitor VOLUME=portitor-repos NETWORK="" CONFIGDIR="" KEYS=""
KEYCHAIN_SERVICE="${PORTITOR_KEYCHAIN_SERVICE:-portitor}"
KEYCHAIN_ACCOUNT="${PORTITOR_KEYCHAIN_ACCOUNT:-dca-sandbox}"
while [[ $# -gt 0 ]]; do
	case "$1" in
		--name) NAME="$2"; shift 2 ;;
		--image) IMAGE="$2"; shift 2 ;;
		--volume) VOLUME="$2"; shift 2 ;;
		--network) NETWORK="$2"; shift 2 ;;
		--config-dir) CONFIGDIR="$2"; shift 2 ;;
		--keys) KEYS="$2"; shift 2 ;; # comma-separated *.pub paths
		*) echo "run.sh: unknown flag: $1" >&2; exit 2 ;;
	esac
done
[[ -n "$CONFIGDIR" && -d "$CONFIGDIR/repos.d" ]] || { echo "run.sh: --config-dir <dir with repos.d/> required" >&2; exit 2; }

# GitHub PAT: env override, else the OS keychain (libsecret / macOS security).
TOKEN="${GH_TOKEN:-}"
if [[ -z "$TOKEN" ]]; then
	if [[ "$(uname)" == "Darwin" ]]; then
		TOKEN="$(security find-generic-password -s "$KEYCHAIN_SERVICE" -a "$KEYCHAIN_ACCOUNT" -w 2>/dev/null || true)"
	else
		TOKEN="$(secret-tool lookup service "$KEYCHAIN_SERVICE" account "$KEYCHAIN_ACCOUNT" 2>/dev/null || true)"
	fi
fi
[[ -n "$TOKEN" ]] || die "no GH_TOKEN and none in keychain (service=$KEYCHAIN_SERVICE account=$KEYCHAIN_ACCOUNT)"
# Every docker CLI below would inherit it, and a bare `-e GH_TOKEN` would forward it.
unset GH_TOKEN

# Concatenate the agent/role public keys (one per line) for AGENT_AUTHORIZED_KEY.
AUTH=""
if [[ -n "$KEYS" ]]; then
	IFS=',' read -ra paths <<<"$KEYS"
	for p in "${paths[@]}"; do
		[[ -f "$p" ]] || die "key not found: $p"
		AUTH+="$(awk '{print $1, $2}' "$p")"$'\n'
	done
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# ---- bake the config into a derived image -------------------------------------
# The whole config dir (symlinks followed; .lock and dot-prefixed entries left out),
# root-owned and world-readable: the gate's users need not match any host uid, so a
# userns-remapped daemon reads it as is. Used by ID, never by a shared tag, so two
# concurrent runs can never start each other's config.
CTX="$WORK/ctx"
mkdir -p "$CTX/etc"
cp -RL "$CONFIGDIR" "$CTX/etc/portitor"
find "$CTX/etc/portitor" -mindepth 1 \( -name '.*' -o -name '*.lock' \) -prune -exec rm -rf {} +
chmod -R u=rwX,go=rX "$CTX"
shopt -s nullglob
cfgs=("$CTX"/etc/portitor/repos.d/*.json)
shopt -u nullglob
[[ ${#cfgs[@]} -gt 0 ]] || die "no repos.d/*.json under $CONFIGDIR"
CONFIG_IMAGE="$(printf 'FROM %s\nCOPY --chown=0:0 etc/portitor/ /etc/portitor/\n' "$IMAGE" \
	| docker build -q -f - "$CTX")" || die "docker build of the config layer on $IMAGE failed"
CONFIG_IMAGE="$(printf '%s\n' "$CONFIG_IMAGE" | tail -n 1)"
[[ "$CONFIG_IMAGE" =~ ^sha256:[0-9a-f]{64}$ ]] || die "docker build printed no image ID for the config layer on $IMAGE (got '$CONFIG_IMAGE')"
# The entrypoint refuses to boot on an invalid config. Running the same check here, as
# the user the gate's forced command runs as, proves git can read every repo config and
# that its allowed_signers path exists — before the running container is touched, not
# as a restart loop after "portitor up".
for f in "${cfgs[@]}"; do
	docker run --rm --network none --user git --entrypoint /usr/local/bin/portitor "$CONFIG_IMAGE" \
		validate-config --config "${f#"$CTX"}" >&2 \
		|| die "${f#"$CTX"} does not validate as user git inside the baked image (see above)"
done

# ---- create, deliver the PAT, start -----------------------------------------
docker rm -f "$NAME" >/dev/null 2>&1 || true
args=(--name "$NAME" --restart unless-stopped
	-v "$VOLUME":/srv/git
	-e AGENT_AUTHORIZED_KEY="$AUTH")
if [[ -n "$NETWORK" ]]; then args+=(--network "$NETWORK"); fi
docker create "${args[@]}" "$CONFIG_IMAGE" >/dev/null || die "docker create $NAME failed"

# The PAT exists on the host only for the copy, in a 0700 dir as a 0600 file.
SECRET_DIR="$WORK/secret"
if ! (umask 077 && mkdir "$SECRET_DIR" && printf '%s' "$TOKEN" >"$SECRET_DIR/gh_token"); then
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	die "could not write the PAT for the copy under $WORK"
fi
if ! docker cp "$SECRET_DIR/gh_token" "$NAME:/run/secrets/gh_token" >/dev/null; then
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	die "copying the PAT into $NAME failed (an image built before /run/secrets existed? rebuild it)"
fi
rm -f "$SECRET_DIR/gh_token"

if ! docker start "$NAME" >/dev/null; then
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	die "docker start $NAME failed"
fi
echo "portitor up as '$NAME'${NETWORK:+ on $NETWORK} (config image $CONFIG_IMAGE). Provision a repo with: docker exec -u git $NAME portitor add-repo ..."
