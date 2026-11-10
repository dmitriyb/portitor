#!/bin/sh
# Turn on Docker's userns-remap on a CI runner, keeping whatever the runner's
# daemon.json already configures, and prove the daemon picked it up.
set -eu
cfg=/etc/docker/daemon.json
if [ -s "$cfg" ]; then
	sudo jq '. + {"userns-remap": "default"}' "$cfg" | sudo tee "$cfg.new" >/dev/null
else
	echo '{"userns-remap": "default"}' | sudo tee "$cfg.new" >/dev/null
fi
sudo mv "$cfg.new" "$cfg"
sudo systemctl restart docker
docker info --format '{{json .SecurityOptions}}' | grep -q 'name=userns' || {
	echo "ci-userns-remap: the daemon still runs without userns-remap" >&2
	exit 1
}
