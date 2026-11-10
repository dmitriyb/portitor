#!/bin/sh
# portitor gate entrypoint, run as root under tini inside the image portitor
# deploy builds. See spec/gate/arch_pre_receive.md.
set -eu

PORTITOR_BIN=/usr/local/bin/portitor
SECRETS=/run/secrets
TOKEN=$SECRETS/gh_token
HOSTKEY=/var/lib/portitor/hostkey/ssh_host_ed25519_key

refuse() {
	echo "portitor entrypoint: refusing to start — $*" >&2
	exit 1
}

# setpriv sets no environment of its own; git reads HOME for the global config
# that holds the credential helper.
as_git() {
	setpriv --reuid=git --regid=git --init-groups env HOME=/home/git USER=git LOGNAME=git "$@"
}

# portitor never logs in to GitHub; every consumer reads this file per call.
[ -s "$TOKEN" ] || refuse "$TOKEN is missing or empty (deliver it with portitor deploy)"
chown git:git "$SECRETS" "$TOKEN"
chmod 0700 "$SECRETS"
chmod 0400 "$TOKEN"

as_git git config --global credential.https://github.com.helper "$PORTITOR_BIN git-credential"

if [ ! -f "$HOSTKEY" ]; then
	ssh-keygen -q -t ed25519 -N '' -f "$HOSTKEY"
fi

# Each role key gets the forced command, so it can only reach the portitor shell
# dispatcher. no-touch-required: role keys may be no-touch resident FIDO2 keys,
# whose signatures sshd otherwise rejects.
install -d -m 700 -o git -g git /home/git/.ssh
: >/home/git/.ssh/authorized_keys
printf '%s\n' "${AGENT_AUTHORIZED_KEY:-}" | while IFS= read -r key; do
	[ -n "$key" ] || continue
	fp=$(printf '%s\n' "$key" | ssh-keygen -lf - | awk '{print $2}')
	printf 'command="%s shell %s",restrict,no-touch-required %s\n' "$PORTITOR_BIN" "$fp" "$key" \
		>>/home/git/.ssh/authorized_keys
done
chown git:git /home/git/.ssh/authorized_keys
chmod 600 /home/git/.ssh/authorized_keys

for cfg in /etc/portitor/repos.d/*.json; do
	[ -f "$cfg" ] || continue
	as_git "$PORTITOR_BIN" validate-config --config "$cfg" || refuse "$cfg is invalid (see above)"
done

for cfg in /etc/portitor/repos.d/*.json; do
	[ -f "$cfg" ] || continue
	repo=$(basename "$cfg" .json)
	as_git "$PORTITOR_BIN" add-repo --ensure --repo "$repo" || refuse "provisioning $repo failed (see above)"
done

exec /usr/sbin/sshd -D -e
