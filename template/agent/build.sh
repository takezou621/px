#!/usr/bin/env bash
# Build the px agent LXC template on a Proxmox VE node: the runner template
# (bash, git, curl, build-essential, jq, python3) plus the Claude Code CLI,
# baked in via the native installer (a self-contained binary — no Node
# runtime in the image).
#
# Usage (on the PVE node, or via ssh):
#   ./build.sh <CTID> [bridge] [storage] [tpl-storage]
#
# Result: container <CTID> converted to a template, named px-agent-debian12.
# Reference it in Task manifests as: image: px-agent-debian12 — which `px run`
# assumes by default.
set -euo pipefail

CTID="${1:?usage: build.sh <CTID> [bridge] [storage] [tpl-storage]}"
BRIDGE="${2:-vmbr0}"
STORAGE="${3:-local-lvm}"
TPL_STORAGE="${4:-local}"
NAME="px-agent-debian12"

if [[ $EUID -ne 0 ]]; then
  echo "must run as root on the PVE node" >&2
  exit 1
fi

pveam update >/dev/null
IMAGE="$(pveam available --section system | awk '/debian-12-standard/ {print $2; exit}')"
[[ -n "$IMAGE" ]] || { echo "no debian-12 image found" >&2; exit 1; }
# Template files need a vztmpl-capable storage (dir/NFS); lvmthin pools like
# local-lvm reject them, so images land on TPL_STORAGE (default: local).
if ! pvesm list "$TPL_STORAGE" --content vztmpl 2>/dev/null | grep -qF "$IMAGE"; then
  echo "==> downloading Debian 12 image"
  pveam download "$TPL_STORAGE" "$IMAGE"
else
  echo "==> $IMAGE already on $TPL_STORAGE, skipping download"
fi

echo "==> creating container $CTID ($NAME)"
pct create "$CTID" "${TPL_STORAGE}:vztmpl/${IMAGE}" \
  --hostname "$NAME" \
  --net0 "name=eth0,bridge=${BRIDGE},ip=dhcp" \
  --cores 4 --memory 4096 --swap 0 \
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

echo "==> installing Claude Code (native installer)"
# The installer puts the binary under /root/.local/bin; the runner executes
# through a bare `sh` whose PATH stops at /usr/local/bin, so the CLI is
# linked there. --version proves the binary actually runs before the
# template is frozen — a template whose agent CLI cannot start fails every
# task it clones.
pct exec "$CTID" -- bash -euxc \
  'curl -fsSL https://claude.ai/install.sh | bash -s stable'
pct exec "$CTID" -- bash -euxc \
  'ln -sf /root/.local/bin/claude /usr/local/bin/claude && claude --version'

echo "==> stopping and converting to template"
pct stop "$CTID"
pct template "$CTID"

echo "==> done: template VMID=$CTID name=$NAME"
echo "    reference it in Task manifests as: image: $NAME"
