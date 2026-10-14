#!/bin/sh
# Copyright 2026 Knoban LLC
# SPDX-License-Identifier: MIT
#
# Installs or updates ephemeral-runners on this host, then (re)starts it.
#
#   sudo ./scripts/install.sh <deployment.toml> ...   add or update deployments, then restart
#   sudo ./scripts/install.sh                         update the controller, then restart
#
# A deployment file is named <name>.toml or <name>/config.toml. Keep them
# outside this repo; see "Where to keep deployment configs" in README.md.
# Deployments already installed and not named stay installed. Golden images
# that are missing are built first (several minutes each), and everything is
# checked together before anything is replaced. Safe to rerun.
set -eu

[ "$(id -u)" -eq 0 ] || { echo "Run as root: sudo $0 $*" >&2; exit 1; }

repo=$(cd "$(dirname "$0")/.." && pwd)
user=ephrunner
etc=/etc/ephemeral-runners
state=/var/lib/ephemeral-runners
share=/usr/local/share/ephemeral-runners
bin=/usr/local/bin/ephemeral-runners
units="ephemeral-runners-firewall.service ephemeral-runners-firewall-watch.service ephemeral-runners-firewall-watch.timer ephemeral-runners.service ephemeral-runners-images.service ephemeral-runners-images.timer"

deployment_name() {
	name=$(basename "$1" .toml)
	if [ "$name" = config ]; then name=$(basename "$(dirname "$1")"); fi
	echo "$name"
}

# Fail before changing anything if a deployment file is missing.
for f in "$@"; do
	[ -f "$f" ] || { echo "No such file: $f" >&2; exit 1; }
done

echo "Installing packages..."
need=
for p in qemu-system-x86 qemu-utils cloud-image-utils ubuntu-cloudimage-keyring nftables gpgv; do
	dpkg-query -W -f='${Status}' "$p" 2>/dev/null | grep -q "install ok installed" || need="$need $p"
done
if [ -n "$need" ]; then
	apt-get update -q
	# shellcheck disable=SC2086 # word splitting is the point
	DEBIAN_FRONTEND=noninteractive apt-get install -y -q --no-install-recommends $need
fi

go=$(command -v go || echo /usr/local/go/bin/go)
[ -x "$go" ] || { echo "Go is needed to build the controller; see Requirements in README.md" >&2; exit 1; }
echo "Building..."
(cd "$repo" && "$go" build -trimpath -o "$bin.new" .)
mv "$bin.new" "$bin"

if ! id "$user" >/dev/null 2>&1; then
	useradd --system --user-group --home-dir "$state" --no-create-home --shell /usr/sbin/nologin "$user"
fi
usermod -aG kvm "$user"
install -d -o root -g "$user" -m 0750 "$etc" "$etc/deployments" "$etc/keys"
install -d -o "$user" -g "$user" -m 0750 "$state" "$state/images" "$state/vms"
for k in "$etc"/keys/*.pem; do
	if [ -f "$k" ]; then chown root:"$user" "$k" && chmod 0640 "$k"; fi
done

# Let VMs ping: QEMU's user networking needs unprivileged ICMP sockets, which
# this allows for the runner user's group only.
gid=$(id -g "$user")
echo "net.ipv4.ping_group_range = $gid $gid" >/etc/sysctl.d/60-ephemeral-runners.conf
sysctl -q -p /etc/sysctl.d/60-ephemeral-runners.conf

install -d "$share"
install -m 0644 "$repo/packaging/firewall.nft" "$share/firewall.nft"
install -m 0755 "$repo/packaging/firewall-watch" "$share/firewall-watch"
# Anything that flushes the whole nftables ruleset also removes the runner
# firewall: nftables.service puts it back at once, and the watchdog timer
# checks every minute for anything else.
install -d /etc/systemd/system/nftables.service.d
install -m 0644 "$repo/packaging/nftables-ephemeral-runners.conf" /etc/systemd/system/nftables.service.d/50-ephemeral-runners.conf
for u in $units; do
	install -m 0644 "$repo/packaging/$u" "/etc/systemd/system/$u"
done
systemctl daemon-reload
systemctl enable -q ephemeral-runners-firewall.service ephemeral-runners-firewall-watch.timer ephemeral-runners-images.timer
# Reload, not restart: restarting the firewall would stop the controller,
# which is bound to it, and cut off any running jobs before the check below.
systemctl reload-or-restart ephemeral-runners-firewall.service
systemctl start ephemeral-runners-firewall-watch.timer ephemeral-runners-images.timer

# Stage the would-be config and check it before touching the real one.
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
mkdir -p "$stage/deployments"
if [ -f "$etc/host.toml" ]; then cp "$etc/host.toml" "$stage/"; fi
# Images built below preload what it lists, and the check below checks it.
if [ -f "$etc/preload.toml" ]; then
	chown root:"$user" "$etc/preload.toml" && chmod 0640 "$etc/preload.toml"
	cp "$etc/preload.toml" "$stage/"
fi
for f in "$etc"/deployments/*.toml; do
	if [ -f "$f" ]; then cp "$f" "$stage/deployments/"; fi
done
for f in "$@"; do
	cp "$f" "$stage/deployments/$(deployment_name "$f").toml"
done
chown -R "$user" "$stage"

if ! ls "$stage"/deployments/*.toml >/dev/null 2>&1; then
	echo "No deployments installed. Pass one: sudo $0 <deployment.toml>" >&2
	echo "(start a new one from config.example.toml)" >&2
	exit 1
fi

for image in $(runuser -u "$user" -- "$bin" -config-dir "$stage" check 2>&1 | sed -n 's/.*image \([^ ]*\) is missing.*/\1/p'); do
	echo "Building image $image (several minutes)..."
	runuser -u "$user" -- "$bin" -config-dir "$stage" build-image "$image"
done

echo "Checking config, credentials, images and budget..."
runuser -u "$user" -- "$bin" -config-dir "$stage" check

for f in "$@"; do
	dest="$etc/deployments/$(deployment_name "$f").toml"
	install -o root -g "$user" -m 0640 "$f" "$dest"
	echo "Installed $dest"
done

systemctl enable -q ephemeral-runners.service
systemctl restart ephemeral-runners.service
echo "Started. Logs: journalctl -fu ephemeral-runners"
