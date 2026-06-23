#!/bin/sh
# portitor container entrypoint: a minimal git-over-SSH server.
#
# Repos are provisioned out of band with `portitor init-repo` (e.g.
# `docker exec portitor portitor init-repo --bare /srv/git/<name>.git ...`),
# so they survive on the /srv/git volume across restarts.
set -eu

# Host keys (persisted on the volume if /srv/git/etc-ssh is bind-mounted there;
# otherwise regenerated per container).
ssh-keygen -A

# Install the agent's push public key, if supplied, so it can reach the proxy.
if [ -n "${AGENT_AUTHORIZED_KEY:-}" ]; then
	install -d -m 700 -o git -g git /home/git/.ssh
	printf '%s\n' "$AGENT_AUTHORIZED_KEY" >/home/git/.ssh/authorized_keys
	chown git:git /home/git/.ssh/authorized_keys
	chmod 600 /home/git/.ssh/authorized_keys
fi

# sshd in the foreground (PID 1 under tini); -e logs to stderr.
exec /usr/sbin/sshd -D -e
