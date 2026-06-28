#!/bin/sh
# portitor container entrypoint: a git-over-SSH gateway + GitHub action mediator.
#
# Inputs (env):
#   GH_TOKEN              portitor's GitHub credential (scoped PAT). Used for both
#                        `gh pr ...` and `git push upstream` (via gh's git
#                        credential helper). The agent never sees it.
#   AGENT_AUTHORIZED_KEY one or more agent/role PUBLIC keys (one per line). Each is
#                        installed with a forced command so the key can ONLY do
#                        gated git + the role-checked `portitor pr` API.
#   PORTITOR_CONFIG      in-container path to the per-repo portitor config the
#                        `portitor shell` dispatcher loads (role map etc.).
#
# Repos are provisioned out of band with `portitor init-repo` and live on the
# /srv/git volume.
set -eu

PORTITOR_CONFIG="${PORTITOR_CONFIG:-/srv/git/portitor.json}"
PORTITOR_BIN=/usr/local/bin/portitor

ssh-keygen -A # host keys

# Give the proxy its GitHub credential: persist the token to the git user's gh
# config (so post-receive + `portitor pr` use it with no env) and point git's
# credential helper at gh (so `git push upstream` over HTTPS authenticates too).
if [ -n "${GH_TOKEN:-}" ]; then
	# `gh auth login --with-token` refuses to STORE while GH_TOKEN is set in the
	# env (it would just use the env value); unset it in the subshell so the token
	# is persisted to the git user's gh config for later sessions.
	printf '%s' "$GH_TOKEN" | su git -c 'unset GH_TOKEN; gh auth login --with-token && gh auth setup-git'
	unset GH_TOKEN # don't leave it in the sshd environment
fi

# Install each agent key with a forced command: the connection can only run the
# `portitor shell` dispatcher (gated git pack OR the role-checked pr API), keyed
# to the key's fingerprint. `restrict` disables pty/forwarding/etc.;
# `no-touch-required` is required because the role keys are no-touch resident
# FIDO2 (sk) keys — their signatures carry user-presence=0, which sshd otherwise
# rejects. (Touch-required keys still authenticate fine with this option set.)
if [ -n "${AGENT_AUTHORIZED_KEY:-}" ]; then
	install -d -m 700 -o git -g git /home/git/.ssh
	: >/home/git/.ssh/authorized_keys
	printf '%s\n' "$AGENT_AUTHORIZED_KEY" | while IFS= read -r key; do
		[ -n "$key" ] || continue
		fp=$(printf '%s\n' "$key" | ssh-keygen -lf - | awk '{print $2}')
		printf 'command="PORTITOR_CONFIG=%s %s shell %s",restrict,no-touch-required %s\n' \
			"$PORTITOR_CONFIG" "$PORTITOR_BIN" "$fp" "$key" \
			>>/home/git/.ssh/authorized_keys
	done
	chown git:git /home/git/.ssh/authorized_keys
	chmod 600 /home/git/.ssh/authorized_keys
fi

exec /usr/sbin/sshd -D -e # foreground under tini
