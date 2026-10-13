#!/bin/sh
# Copyright 2026 Knoban LLC
# SPDX-License-Identifier: MIT
#
# Stops ephemeral-runners and removes its systemd units. Leaves the config,
# keys, binary, golden images and the ephrunner user in place. --purge also
# deletes leftover VMs and their runner registrations. To stop serving just
# one deployment, use remove-deployment.sh instead.
set -eu

[ "$(id -u)" -eq 0 ] || { echo "Run as root: sudo $0 $*" >&2; exit 1; }

units="ephemeral-runners.service ephemeral-runners-images.timer ephemeral-runners-images.service ephemeral-runners-firewall.service"
# shellcheck disable=SC2086 # word splitting is the point
systemctl disable --now -q $units 2>/dev/null || true
if [ "${1:-}" = "--purge" ]; then
	runuser -u ephrunner -- /usr/local/bin/ephemeral-runners cleanup
fi
for u in $units; do
	rm -f "/etc/systemd/system/$u"
done
rm -f /etc/sysctl.d/60-ephemeral-runners.conf
systemctl daemon-reload
echo "Stopped and removed ephemeral-runners"
