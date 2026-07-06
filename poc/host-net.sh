#!/bin/sh
# Give the macOS host an address in the pod IPv6 /64 so host->pod traffic
# works (pods live on the vmnet shared bridge, bridge100).
#
# Needs root. bridge100 only exists while at least one pod VM is running,
# and the alias disappears with it — re-run after the bridge is recreated.
set -e
PREFIX="${1:-fd42:6b75:6265::1/64}"
exec sudo ifconfig bridge100 inet6 alias "$PREFIX"
