#!/bin/sh
# Copyright 2026 Knoban LLC
# SPDX-License-Identifier: MIT
#
# Stops ephemeral-runners and removes its systemd units and the VMs'
# ephrunner-vm user. Leaves the config, keys, binary, golden images and the
# ephrunner user in place. --purge also deletes
# leftover VMs and their runner registrations. To stop serving just one
# deployment, use remove-deployment.sh instead.
set -eu

[ "$(id -u)" -eq 0 ] || { echo "Run as root: sudo $0 $*" >&2; exit 1; }

units="ephemeral-runners.service ephemeral-runners-images.timer ephemeral-runners-images.service ephemeral-runners-vm.socket ephemeral-runners-firewall-watch.timer ephemeral-runners-firewall-watch.service ephemeral-runners-firewall.service"
# shellcheck disable=SC2086 # word splitting is the point
systemctl disable --now -q $units 2>/dev/null || true
systemctl stop 'ephemeral-runners-vm@*.service' 2>/dev/null || true
if [ "${1:-}" = "--purge" ]; then
	runuser -u ephrunner -- /usr/local/bin/ephemeral-runners cleanup
fi
for u in $units ephemeral-runners-vm@.service system-ephemeralrunners.slice; do
	rm -f "/etc/systemd/system/$u"
done
rm -f /etc/sysctl.d/60-ephemeral-runners.conf /etc/systemd/system/nftables.service.d/50-ephemeral-runners.conf
# Back to the host's own ping_group_range, from the files left.
sysctl -q --system
rmdir /run/ephemeral-runners 2>/dev/null || true
rmdir /etc/systemd/system/nftables.service.d 2>/dev/null || true
systemctl daemon-reload
if id ephrunner-vm >/dev/null 2>&1; then userdel ephrunner-vm; fi
echo "Stopped and removed ephemeral-runners"
