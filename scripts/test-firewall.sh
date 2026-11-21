#!/bin/sh
# Copyright 2026 Knoban LLC
# SPDX-License-Identifier: MIT
#
# Checks what the runner firewall lets the ephrunner user reach. It builds a
# throwaway host, a LAN neighbour and a router out of network namespaces,
# loads the rules into the host's namespace only, so this machine's own
# ruleset is never touched, and connects to listeners on each, as ephrunner
# and as root.
#
#   sudo scripts/test-firewall.sh [rules.nft]    default: packaging/firewall.nft
#
# Needs Linux, nft, ip, setpriv, OpenBSD netcat (nc) and an ephrunner user,
# which the rules name. The addresses are documentation ranges, standing in
# for public ones.
set -eu

[ "$(id -u)" -eq 0 ] || { echo "Run as root: sudo $0 $*" >&2; exit 1; }
rules=${1:-$(cd "$(dirname "$0")/.." && pwd)/packaging/firewall.nft}
uid=$(id -u ephrunner 2>/dev/null) || { echo "No ephrunner user: create one first (useradd --system ephrunner)" >&2; exit 1; }
gid=$(id -g ephrunner)

# Named per run, so that two runs on one machine don't tear each other down.
host=fwtest-host-$$ neighbour=fwtest-neighbour-$$ router=fwtest-router-$$

cleanup() {
	for ns in $host $neighbour $router; do
		ip netns pids "$ns" 2>/dev/null | xargs -r kill 2>/dev/null || true
		ip netns delete "$ns" 2>/dev/null || true
	done
}
trap cleanup EXIT
# dash runs the EXIT trap on exit only, not when a signal kills it.
trap 'exit 130' INT TERM

for ns in $host $neighbour $router; do
	ip netns add "$ns"
	ip -n "$ns" link set lo up
done

# host lan0 <-> neighbour eth0, and host wan0 <-> router eth0, the host's
# way out. Addresses skip duplicate address detection so they work at once.
ip -n $host link add lan0 type veth peer name eth0 netns $neighbour
ip -n $host link add wan0 type veth peer name eth0 netns $router
ip -n $host link set lan0 up
ip -n $host link set wan0 up
ip -n $neighbour link set eth0 up
ip -n $router link set eth0 up

ip -n $host addr add 2001:db8:a::1/64 dev lan0 nodad
ip -n $neighbour addr add 2001:db8:a::2/64 dev eth0 nodad
ip -n $host addr add 2001:db8:b::1/64 dev wan0 nodad
ip -n $host addr add 203.0.113.1/24 dev wan0
ip -n $router addr add fe80::ff/64 dev eth0 nodad
ip -n $router addr add 2001:db8:b::2/64 dev eth0 nodad
ip -n $router addr add 203.0.113.2/24 dev eth0
ip -n $host addr add 2001:db8:c::1/128 dev lo
ip -n $host addr add 198.51.100.1/32 dev lo
# The internet, behind the router.
ip -n $router addr add 2001:db8:f::1/128 dev lo
ip -n $router addr add 192.0.2.1/32 dev lo
ip -n $host route add default via fe80::ff dev wan0
ip -n $host route add default via 203.0.113.2

listen() { # namespace address port
	ip netns exec "$1" nc -lk "$2" "$3" >/dev/null 2>&1 &
}

failures=0
check() { # want(connects|refused) user namespace address port what
	want=$1 user=$2 ns=$3 addr=$4 port=$5 what=$6
	if [ "$user" = root ]; then as=; else as="setpriv --reuid=$uid --regid=$gid --clear-groups"; fi
	# shellcheck disable=SC2086 # $as is a command prefix, split on purpose
	if ip netns exec "$ns" $as nc -z -w 2 "$addr" "$port" 2>/dev/null; then got=connects; else got=refused; fi
	if [ "$got" = "$want" ]; then
		echo "ok    $user to $addr port $port: $got ($what)"
	else
		echo "FAIL  $user to $addr port $port: $got, want $want ($what)"
		failures=$((failures + 1))
	fi
}

# Each target is checked as root first, which the firewall doesn't restrict,
# so a refusal as ephrunner is the firewall's and not a missing listener.
blocked() { # address port what
	check connects root $host "$1" "$2" "$3"
	check refused ephrunner $host "$1" "$2" "$3"
}

for a in 127.0.0.1 ::1 2001:db8:a::1 2001:db8:b::1 2001:db8:c::1 198.51.100.1 203.0.113.1; do
	listen $host "$a" 8000
done
listen $host 127.0.0.53 53
listen $neighbour 2001:db8:a::2 8000
listen $router 2001:db8:b::2 8000
for a in 2001:db8:f::1 192.0.2.1; do listen $router "$a" 8000; done
sleep 1

ip netns exec $host nft -f "$rules"

check connects ephrunner $host 127.0.0.53 53 "the host's resolver"
check connects ephrunner $host 2001:db8:f::1 8000 "the internet over IPv6"
check connects ephrunner $host 192.0.2.1 8000 "the internet over IPv4"
blocked 127.0.0.1 8000 "the host's loopback"
blocked ::1 8000 "the host's IPv6 loopback"
blocked 198.51.100.1 8000 "a public IPv4 address of the host's own"
blocked 203.0.113.1 8000 "the host's public IPv4 address on its way out"
blocked 2001:db8:c::1 8000 "a public IPv6 address of the host's own"
blocked 2001:db8:a::1 8000 "the host's public IPv6 address on its LAN"
blocked 2001:db8:b::1 8000 "the host's public IPv6 address on its way out"
blocked 2001:db8:a::2 8000 "a LAN neighbour's public IPv6 address"
blocked 2001:db8:b::2 8000 "the router's public IPv6 address on the host's link"

# Addresses that arrive after the rules load, as SLAAC's do when a network
# starts advertising a prefix.
ip -n $host addr add 2001:db8:e::1/64 dev lan0 nodad
ip -n $neighbour addr add 2001:db8:e::2/64 dev eth0 nodad
ip -n $host addr add 198.51.100.9/32 dev lo
listen $host 2001:db8:e::1 8000
listen $neighbour 2001:db8:e::2 8000
listen $host 198.51.100.9 8000
sleep 1

blocked 2001:db8:e::1 8000 "the host's IPv6 address added after loading"
blocked 2001:db8:e::2 8000 "a neighbour on a prefix added after loading"
blocked 198.51.100.9 8000 "the host's IPv4 address added after loading"

if [ "$failures" -ne 0 ]; then
	echo "$failures checks failed" >&2
	exit 1
fi
echo "All checks passed"
