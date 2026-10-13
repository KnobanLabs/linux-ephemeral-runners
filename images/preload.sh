#!/bin/bash
# Copyright 2026 Knoban LLC
# SPDX-License-Identifier: MIT
#
# Preloads one item of the host's preload.toml into the golden image. It
# runs as root inside the image build VM, once per item, from provision.sh,
# which leaves an item out of the image if this fails.
#
#   preload.sh playwright <version> <browser>...
#   preload.sh docker <image>@sha256:<digest>
set -euxo pipefail
export DEBIAN_FRONTEND=noninteractive

kind=$1
shift
case $kind in
playwright)
	version=$1
	shift
	# playwright-core has no dependencies, so its npm tarball is the whole
	# CLI, and it runs on the Node the Actions runner ships.
	node=$(find /home/runner/actions-runner/externals -regextype posix-extended \
		-regex '.*/externals/node[0-9]+/bin/node' -type f | sort -V | tail -n 1)
	[[ -x $node ]]
	dir=$(mktemp -d)
	trap 'rm -rf "$dir"' EXIT
	chmod 0755 "$dir"
	integrity=$(curl -fsSL "https://registry.npmjs.org/playwright-core/$version" | jq -er .dist.integrity)
	curl -fsSL -o "$dir/playwright-core.tgz" \
		"https://registry.npmjs.org/playwright-core/-/playwright-core-$version.tgz"
	[[ $integrity == "sha512-$(openssl dgst -sha512 -binary "$dir/playwright-core.tgz" | base64 -w 0)" ]]
	tar -xzf "$dir/playwright-core.tgz" -C "$dir" --no-same-owner
	"$node" "$dir/package/cli.js" install-deps "$@"
	# Into the runner user's ~/.cache/ms-playwright, where jobs' Playwright
	# looks by default. It reuses a browser only if the revision matches and
	# the download completed, so another version just downloads its own.
	sudo -H -u runner "$node" "$dir/package/cli.js" install "$@"
	;;
docker)
	systemctl start docker
	docker pull --quiet "$1"
	;;
*)
	echo "Unknown preload kind: $kind" >&2
	exit 1
	;;
esac
