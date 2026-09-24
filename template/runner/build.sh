#!/usr/bin/env bash
# Build the px runner LXC template on a Proxmox VE node.
#
# Usage (on the PVE node, or via ssh):
#   ./build.sh <CTID> [bridge] [storage]
#
# Result: container <CTID> converted to a template, named px-runner-debian12.
# The template contains: bash, git, curl, and the px boot shim layout
# (/run/px/{goal,cmd.sh,task.log,exit} created at runtime by the controller).
set -euo pipefail

CTID="${1:?usage: build.sh <CTID> [bridge] [storage]}"
BRIDGE="${2:-vmbr0}"
STORAGE="${3:-local-lvm}"
NAME="px-runner-debian12"

if [[ $EUID -ne 0 ]]; then
  echo "must run as root on the PVE node" >&2
  exit 1
fi

echo "==> downloading Debian 12 image (once)"
pveam update >/dev/null
IMAGE="$(pveam available --section system | awk '/debian-12-standard/ {print $2; exit}')"
[[ -n "$IMAGE" ]] || { echo "no debian-12 image found" >&2; exit 1; }
pveam download "$STORAGE" "$IMAGE" >/dev/null 2>&1 || true

echo "==> creating container $CTID ($NAME)"
pct create "$CTID" "${STORAGE}:vztmpl/${IMAGE}" \
  --hostname "$NAME" \
  --net0 "name=eth0,bridge=${BRIDGE},ip=dhcp" \
  --cores 2 --memory 2048 --swap 0 \
  --unprivileged 1 \
  --features nesting=1 \
  --rootfs "${STORAGE}:8" \
  --start 1

echo "==> installing base tools"
pct exec "$CTID" -- bash -eux <<'EOS'
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
  bash git curl ca-certificates build-essential jq python3 \
  >/dev/null
EOS

echo "==> stopping and converting to template"
pct stop "$CTID"
pct template "$CTID"

echo "==> done: template VMID=$CTID name=$NAME"
echo "    reference it in Task manifests as: image: $NAME"
