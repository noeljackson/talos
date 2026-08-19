#!/usr/bin/env bash

# This file is both sourced by e2e-qemu.sh and run directly by Make before an
# expensive QEMU build. Keep the local topology deterministic, but never
# create a bridge inside an existing host link-scope route.

QEMU_CIDR="${QEMU_CIDR:-172.20.1.0/24}"

case "${QEMU_CIDR}" in
  *'.0/24')
    QEMU_NETWORK_PREFIX="${QEMU_CIDR%.0/24}"
    ;;
  *)
    echo "QEMU_CIDR must be an IPv4 /24 ending in .0/24: ${QEMU_CIDR}" >&2

    exit 1
    ;;
esac

QEMU_GATEWAY="${QEMU_NETWORK_PREFIX}.1"
QEMU_CONTROLPLANE_IP="${QEMU_NETWORK_PREFIX}.2"

case "$(uname -s)" in
  Linux)
    if ip -4 route show match "${QEMU_GATEWAY}" scope link | grep -q .; then
      echo "QEMU_CIDR ${QEMU_CIDR} overlaps an existing host link-scope route; choose a free QEMU_CIDR" >&2

      exit 1
    fi
    ;;
esac

export QEMU_CIDR QEMU_GATEWAY QEMU_CONTROLPLANE_IP
