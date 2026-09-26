#!/usr/bin/env bash
# Build the px agent LXC template on a Proxmox VE node: the runner template
# (bash, git, curl, build-essential, jq, python3) plus the Claude Code CLI,
# baked in as the native release binary (a self-contained executable — no
# Node runtime in the image).
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

echo "==> installing Claude Code (native binary, manifest checksum)"
# The native installer's `claude install` step re-downloads through an
# internal client that can deliver a truncated staging file on lab networks
# (install dies with "staged binary no longer matches the verified
# checksum"), so this fetches the raw release binary and verifies it
# against the published manifest here instead — same SHA256, one download.
# The runner's PATH is pct exec's default (no /usr/local/bin), hence the
# /usr/bin symlink; --version proves the binary actually runs before the
# template is frozen — a template whose agent CLI cannot start fails every
# task it clones.
pct exec "$CTID" -- bash -eux <<'EOS'
case $(uname -m) in
  x86_64|amd64) plat=linux-x64 ;;
  aarch64|arm64) plat=linux-arm64 ;;
  *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;;
esac
base=https://downloads.claude.ai/claude-code-releases
ver=$(curl -fsSL "$base/latest")
expect=$(curl -fsSL "$base/$ver/manifest.json" | jq -r ".platforms[\"$plat\"].checksum")
dest="/root/.local/share/claude/versions/$ver"
mkdir -p /root/.local/share/claude/versions /root/.local/bin
curl -fsSL -o "$dest" "$base/$ver/$plat/claude"
[ "$(sha256sum "$dest" | cut -d' ' -f1)" = "$expect" ] || { echo "checksum mismatch" >&2; exit 1; }
chmod +x "$dest"
ln -sf "$dest" /root/.local/bin/claude
ln -sf /root/.local/bin/claude /usr/bin/claude
env PATH=/sbin:/bin:/usr/sbin:/usr/bin claude --version
EOS

echo "==> stopping and converting to template"
pct stop "$CTID"
pct template "$CTID"

echo "==> done: template VMID=$CTID name=$NAME"
echo "    reference it in Task manifests as: image: $NAME"
