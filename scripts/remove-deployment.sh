#!/bin/sh
# Copyright 2026 Knoban LLC
# SPDX-License-Identifier: MIT
#
# Retires one deployment from this host: the controller stops serving it,
# then its runner scale sets are deleted from GitHub. Other deployments keep
# running. Usage: sudo ./scripts/remove-deployment.sh <name>
set -eu

[ "$(id -u)" -eq 0 ] || { echo "Run as root: sudo $0 $*" >&2; exit 1; }
[ $# -eq 1 ] || { echo "usage: $0 <deployment-name>" >&2; exit 2; }
name=$1
etc=/etc/ephemeral-runners
file="$etc/deployments/$name.toml"
[ -f "$file" ] || { echo "No deployment named $name in $etc/deployments" >&2; exit 1; }

# Take it out first, so nothing is listening on the scale sets when they go.
install -d -o root -g ephrunner -m 0750 "$etc/removed"
mv "$file" "$etc/removed/$name.toml"
if ls "$etc"/deployments/*.toml >/dev/null 2>&1; then
	systemctl restart ephemeral-runners.service
else
	systemctl stop ephemeral-runners.service
fi

runuser -u ephrunner -- /usr/local/bin/ephemeral-runners remove-scale-sets "$etc/removed/$name.toml"

echo "Removed $name. Its config is kept in $etc/removed/."
echo "Delete its key ($(sed -n 's/^private_key_path *= *"\(.*\)"/\1/p' "$etc/removed/$name.toml")) here and on the GitHub App's settings page."
