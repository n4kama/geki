#!/usr/bin/env bash
# Installs Geki from artifacts staged in a directory (default /tmp). Run as root
# (via sudo) on the target host. Contains no secrets — the token, if any, arrives
# as a pre-written geki.env in the staging dir.
set -euo pipefail
stage="${1:-/tmp}"

install -m 0755 "$stage/geki" /usr/local/bin/geki
install -m 0644 "$stage/geki.service" /etc/systemd/system/geki.service
if [[ -f "$stage/geki.env" ]]; then
	install -m 0600 "$stage/geki.env" /etc/geki.env
	rm -f "$stage/geki.env"
fi

systemctl daemon-reload
systemctl enable geki
systemctl restart geki
rm -f "$stage/geki" "$stage/geki.service"
echo "geki installed and restarted on $(hostname)"
