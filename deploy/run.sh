#!/usr/bin/env bash
# run.sh — launch the portitor container.
#
# Reads portitor's GitHub PAT from the OS keychain (override with GH_TOKEN), and
# starts portitor with its config + the agent/role public keys. The PAT and keys
# never appear on the command line or in the image.
#
#   deploy/run.sh \
#     --config-dir ~/.dca-keys/portitor-config \   # holds portitor.json + allowed_signers
#     --keys     ~/.dca-keys/implementer.pub,~/.dca-keys/reviewer.pub,~/.dca-keys/merger.pub \
#     --name portitor --image portitor --volume portitor-repos
#
# portitor needs both GitHub egress (default bridge) and the agent's internal
# network, so run it on the bridge (default) then attach dca-net:
#   docker network connect dca-net portitor
#
# After it is up, provision the mirror once:
#   docker exec portitor portitor init-repo --bare /srv/git/<repo>.git \
#     --upstream https://github.com/<owner>/<repo>.git --config /etc/portitor/portitor.json
set -euo pipefail

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
[[ -n "$CONFIGDIR" && -f "$CONFIGDIR/portitor.json" ]] || { echo "run.sh: --config-dir <dir with portitor.json> required" >&2; exit 2; }

# GitHub PAT: env override, else the OS keychain (libsecret / macOS security).
TOKEN="${GH_TOKEN:-}"
if [[ -z "$TOKEN" ]]; then
	if [[ "$(uname)" == "Darwin" ]]; then
		TOKEN="$(security find-generic-password -s "$KEYCHAIN_SERVICE" -a "$KEYCHAIN_ACCOUNT" -w 2>/dev/null || true)"
	else
		TOKEN="$(secret-tool lookup service "$KEYCHAIN_SERVICE" account "$KEYCHAIN_ACCOUNT" 2>/dev/null || true)"
	fi
fi
[[ -n "$TOKEN" ]] || { echo "run.sh: no GH_TOKEN and none in keychain (service=$KEYCHAIN_SERVICE account=$KEYCHAIN_ACCOUNT)" >&2; exit 1; }

# Concatenate the agent/role public keys (one per line) for AGENT_AUTHORIZED_KEY.
AUTH=""
if [[ -n "$KEYS" ]]; then
	IFS=',' read -ra paths <<<"$KEYS"
	for p in "${paths[@]}"; do
		[[ -f "$p" ]] || { echo "run.sh: key not found: $p" >&2; exit 1; }
		AUTH+="$(awk '{print $1, $2}' "$p")"$'\n'
	done
fi

docker rm -f "$NAME" >/dev/null 2>&1 || true
args=(-d --name "$NAME" --restart unless-stopped
	-v "$VOLUME":/srv/git
	-v "$CONFIGDIR":/etc/portitor:ro
	-e PORTITOR_CONFIG=/etc/portitor/portitor.json
	-e GH_TOKEN="$TOKEN"
	-e AGENT_AUTHORIZED_KEY="$AUTH")
[[ -n "$NETWORK" ]] && args+=(--network "$NETWORK")

docker run "${args[@]}" "$IMAGE"
echo "portitor up as '$NAME'${NETWORK:+ on $NETWORK}. Provision a repo with: docker exec $NAME portitor init-repo ..."
