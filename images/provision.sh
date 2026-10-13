#!/bin/bash
# Copyright 2026 Knoban LLC
# SPDX-License-Identifier: MIT
#
# Turns a fresh Ubuntu cloud image into a golden runner image. It runs once,
# as root, inside the image build VM (from cloud-init), then the VM powers
# off. It ends by printing EPH-BUILD-OK to the console, which
# `ephemeral-runners build-image` waits for; anything else is a failed build.
set -euxo pipefail

version=$1        # Ubuntu version, e.g. 24.04
runner_version=$2 # Actions runner, e.g. 2.338.0
runner_sha=$3     # its tarball's SHA-256, from the release notes
guest=/var/lib/eph-build/guest
export DEBIAN_FRONTEND=noninteractive

# Background maintenance would compete with jobs for CPU and hold apt's lock
# just when a job wants to install something.
systemctl disable --now \
	apt-daily.timer apt-daily-upgrade.timer unattended-upgrades.service \
	man-db.timer motd-news.timer fwupd-refresh.timer e2scrub_all.timer \
	update-notifier-download.timer update-notifier-motd.timer 2>/dev/null || true
apt-get purge -y --auto-remove snapd unattended-upgrades || true

apt-get update
apt-get -y dist-upgrade
apt-get install -y --no-install-recommends \
	ca-certificates curl wget git git-lfs jq unzip zip xz-utils zstd tar gnupg \
	lsb-release build-essential pkg-config python3 python3-pip python3-venv \
	pipx openssh-client rsync sudo acl file iproute2 iputils-ping dnsutils \
	docker.io
# Expected on hosted runners, but not packaged for every release.
for p in docker-buildx docker-compose-v2 gh shellcheck; do
	apt-get install -y --no-install-recommends "$p" || echo "Skipping $p: not available on $version"
done

# The job user, like GitHub-hosted runners: "runner", passwordless sudo,
# Docker without sudo.
uid=1001
getent passwd "$uid" >/dev/null && uid=
useradd --create-home --shell /bin/bash ${uid:+--uid "$uid"} runner
usermod -aG docker,adm,systemd-journal runner
echo 'runner ALL=(ALL) NOPASSWD:ALL' >/etc/sudoers.d/runner
chmod 0440 /etc/sudoers.d/runner

# The Actions runner release the host picked (the newest), checked against
# the SHA-256 GitHub publishes in its release notes.
curl -fsSL -o /tmp/runner.tar.gz \
	"https://github.com/actions/runner/releases/download/v$runner_version/actions-runner-linux-x64-$runner_version.tar.gz"
echo "$runner_sha  /tmp/runner.tar.gz" | sha256sum -c -
install -d -o runner -g runner /home/runner/actions-runner
tar -xzf /tmp/runner.tar.gz -C /home/runner/actions-runner
rm /tmp/runner.tar.gz
/home/runner/actions-runner/bin/installdependencies.sh

# Environment every job gets, as on hosted runners. setup-* actions use
# ImageOS and the tool cache to pick prebuilt toolchains. Git never prompts:
# a job that runs git without credentials fails at once instead of hanging.
install -d -o runner -g runner /opt/hostedtoolcache /home/runner/work
cat >/home/runner/actions-runner/.env <<EOF
LANG=C.UTF-8
ImageOS=ubuntu${version%%.*}
ImageVersion=$(date -u +%Y%m%d).1
AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache
RUNNER_TOOL_CACHE=/opt/hostedtoolcache
GCM_INTERACTIVE=never
GIT_TERMINAL_PROMPT=0
EOF
chown -R runner:runner /home/runner

# What the host's preload.toml asks for, one item per line, so jobs needn't
# download it. Each item only saves time, so one that fails or hangs is left
# out and named on the console, and jobs download it as before. Every word
# was checked by the host.
while read -r kind args; do
	[[ -n $kind ]] || continue
	# shellcheck disable=SC2086 # one word per argument
	if timeout 15m bash "$guest/preload.sh" "$kind" $args </dev/null; then
		echo "EPH-PRELOAD-OK $kind $args" >/dev/console
	else
		echo "EPH-PRELOAD-FAILED $kind $args" >/dev/console
	fi
done </var/lib/eph-build/preload
# A package install that failed partway must not reach jobs: this fails the
# build if apt can't put it right.
dpkg --configure -a
apt-get check

# The one-job service: started by a udev rule when the host's registration
# disk appears, it runs the runner and powers the VM off after.
install -m 0644 "$guest/eph-runner.service" /etc/systemd/system/
install -m 0644 "$guest/90-eph-runner.rules" /etc/udev/rules.d/
install -d /usr/local/libexec
install -m 0755 "$guest/eph-runner-prepare" "$guest/eph-runner-start" /usr/local/libexec/

# Networking that doesn't depend on this build VM's interface name or MAC.
rm -f /etc/netplan/50-cloud-init.yaml
cat >/etc/netplan/50-eph.yaml <<'EOF'
network:
  version: 2
  ethernets:
    all:
      match:
        name: "en*"
      dhcp4: true
EOF
chmod 0600 /etc/netplan/50-eph.yaml
echo runner >/etc/hostname

# Job VMs boot without cloud-init, and each gets a fresh machine ID.
echo 'network: {config: disabled}' >/etc/cloud/cloud.cfg.d/99-eph-no-network.cfg
touch /etc/cloud/cloud-init.disabled
apt-get clean
truncate -s 0 /etc/machine-id
rm -f /var/lib/dbus/machine-id
journalctl --rotate || true
journalctl --vacuum-time=1s || true
fstrim -av || true
sync

echo "EPH-RUNNER-VERSION=$runner_version" >/dev/console
echo "EPH-BUILD-OK" >/dev/console
