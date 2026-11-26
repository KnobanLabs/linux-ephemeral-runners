#!/bin/bash
# Copyright 2026 Knoban LLC
# SPDX-License-Identifier: MIT
#
# Preloads one item of the host's preload.toml into the golden image. It
# runs as root inside the image build VM, once per item, from provision.sh,
# which leaves an item out of the image if this fails.
#
#   preload.sh node <major or exact version>
#   preload.sh npm <count> sha256:<hash of /var/lib/eph-build/npm-packages>
#   preload.sh playwright <version> <browser>...
#   preload.sh docker <image>@sha256:<digest>
set -euxo pipefail
export DEBIAN_FRONTEND=noninteractive

kind=$1
shift
case $kind in
node)
	spec=$1
	version=$spec
	if [[ $spec != *.* ]]; then
		# The newest stable release of the major version in the manifest
		# actions/setup-node picks from, so a job asking for the newest finds
		# it here. The manifest only picks the version; the bytes are checked
		# against nodejs.org's own checksums.
		version=$(curl -fsSL https://raw.githubusercontent.com/actions/node-versions/main/versions-manifest.json |
			jq -er --arg major "$spec" 'first(.[] | select(.stable and (.version | split(".")[0]) == $major
				and any(.files[]; .platform == "linux" and .arch == "x64")) | .version)')
	fi
	[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]
	# Where setup-node caches a version itself, and the marker without which
	# it ignores the directory.
	dest=/opt/hostedtoolcache/node/$version/x64
	dir=$(mktemp -d)
	trap 'rm -rf "$dir"; [[ -e $dest.complete ]] || rm -rf "$dest"' EXIT
	if [[ ! -e $dest.complete ]]; then
		file=node-v$version-linux-x64.tar.xz
		curl -fsSL -o "$dir/$file" "https://nodejs.org/dist/v$version/$file"
		curl -fsSL -o "$dir/SHASUMS256.txt" "https://nodejs.org/dist/v$version/SHASUMS256.txt"
		(cd "$dir" && awk -v f="$file" '$2 == f' SHASUMS256.txt | sha256sum --strict -c -)
		rm -rf "$dest"
		install -d "$dest"
		tar -xJf "$dir/$file" -C "$dest" --strip-components=1 --no-same-owner
		chown -R runner:runner /opt/hostedtoolcache/node
		install -o runner -g runner -m 0644 /dev/null "$dest.complete"
	fi
	echo "EPH-PRELOAD-INFO node $spec is $version" >/dev/console
	;;
npm)
	count=$1
	list=/var/lib/eph-build/npm-packages
	# The list the host wrote for this item, and no other.
	[[ "sha256:$(sha256sum <"$list" | cut -d ' ' -f 1)" == "$2" ]]
	(($(wc -l <"$list") == count))
	# The npm of the newest Node.js the node items put in the tool cache.
	marker=$(find /opt/hostedtoolcache/node -mindepth 2 -maxdepth 2 -name x64.complete | sort -V | tail -n 1)
	bin=${marker%.complete}/bin
	[[ -x $bin/npm ]]
	# npm ci reads a lockfile's package from the cache by its integrity, so
	# how the bytes got there doesn't matter, and different bytes land under
	# another digest that's never read. npm cache add runs no package
	# scripts. It fetches all its arguments at once, hence the batches.
	cache=/home/runner/.npm
	trap '[[ -n ${ok:-} ]] || rm -rf "$cache"' EXIT
	cd /home/runner
	cut -d ' ' -f 1 "$list" | xargs -n 50 sudo -H -u runner \
		env PATH="$bin:$PATH" npm_config_update_notifier=false npm cache add --cache "$cache"
	# Every package or none: one missing would make the image's record
	# claim what it doesn't hold.
	{ set +x; } 2>/dev/null
	missing=0
	while read -r url integrity; do
		hex=$(base64 -d <<<"${integrity#sha512-}" | od -An -v -tx1 | tr -d ' \n')
		if [[ ! -f $cache/_cacache/content-v2/sha512/${hex:0:2}/${hex:2:2}/${hex:4} ]]; then
			echo "Not in the npm cache: $url" >&2
			missing=$((missing + 1))
		fi
	done <"$list"
	set -x
	((missing == 0))
	rm -rf "$cache/_logs"
	echo "EPH-PRELOAD-INFO npm: $count packages, $(du -sm "$cache" | cut -f 1) MB" >/dev/console
	ok=1
	;;
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
	# Without the GC switch, installing one version would delete the browsers
	# of every version installed before it, whose CLI copies are gone.
	sudo -H -u runner env PLAYWRIGHT_SKIP_BROWSER_GC=1 "$node" "$dir/package/cli.js" install "$@"
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
