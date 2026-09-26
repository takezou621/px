#!/usr/bin/env bash
# E2E for template discovery (M9) against a real px-server + PVE node
# (see docs/e2e.md).
#
# Requires:
#   - px-server running with a working PVE token and node SSH access
#   - the agent template on the node (px-agent-debian12; build it with
#     template/agent/build.sh)
#
# Env:
#   PX_SERVER  px-server URL            (default http://127.0.0.1:7420)
#   PX         px CLI binary path       (default ./px)
#   TIMEOUT    seconds to wait per check (default 300)
#   PVE_SSH    ssh target of the PVE node (default root@192.168.2.100;
#              empty disables the node-side template builds)
#
# Checks:
#   1. the shipped templates list with PX-OK true and describe carries
#      their facts
#   2. a minimal user-authored template built per template/README.md
#      (unprivileged, net0 ip=dhcp) lists PX-OK true
#   3. a template violating the contract (privileged, static ip) lists
#      PX-OK false and names both failed requirements
#   4. a task naming an unknown image reaches ProvisionFailed with the
#      template named in the reason
# The two probe templates are created template-only (never booted) and
# destroyed in cleanup; no existing VM or hand-written firewall config is
# touched.
set -euo pipefail

export PX_SERVER="${PX_SERVER:-http://127.0.0.1:7420}"
PX="${PX:-./px}"
TIMEOUT="${TIMEOUT:-300}"
PVE_SSH="${PVE_SSH-root@192.168.2.100}"
BRIDGE=vmbr0
STORAGE=local-lvm
TPL_STORAGE=local
NAME_OK=px-tmpl-ok
NAME_BAD=px-tmpl-bad

pass=0
fail=0
say() { printf '\n== %s\n' "$*"; }
ok()  { pass=$((pass+1)); echo "PASS: $*"; }
bad() { fail=$((fail+1)); echo "FAIL: $*"; }

phase() { "$PX" get tasks 2>/dev/null | awk -v n="$1" '$1==n {print $2}'; }
wait_phase() { # name want
  local name=$1 want=$2 waited=0
  while (( waited < TIMEOUT )); do
    if [[ $(phase "$name") == "$want" ]]; then
      ok "$name -> $want (${waited}s)"
      return 0
    fi
    sleep 2
    waited=$((waited+2))
  done
  bad "$name did not reach $want within ${TIMEOUT}s (last phase: '$(phase "$name")')"
  return 1
}
wait_listed() { # template-name — the cluster view may lag a moment
  local name=$1 waited=0
  while (( waited < TIMEOUT )); do
    if grep -q "^$name " <("$PX" get templates 2>/dev/null); then
      ok "$name listed (${waited}s)"
      return 0
    fi
    sleep 1
    waited=$((waited+1))
  done
  bad "$name never appeared in px get templates"
  return 1
}

CTID_OK=""
CTID_BAD=""
destroy_probe() {
  for id in $CTID_OK $CTID_BAD; do
    [[ -n $id ]] || continue
    if ! ssh -o BatchMode=yes -o ConnectTimeout=5 "$PVE_SSH" "pct destroy $id" >/dev/null 2>&1; then
      echo "FAIL: pct destroy $id on $PVE_SSH — a probe template leaked" >&2
      fail=$((fail+1))
    fi
  done
}
trap destroy_probe EXIT

node() { # cmd... — run on the PVE node, failing the check on error
  ssh -o BatchMode=yes -o ConnectTimeout=5 "$PVE_SSH" "$@"
}

say "0. px-server is reachable and the node is reachable"
if ! "$PX" get tasks >/dev/null 2>&1; then
  echo "FATAL: cannot reach px-server at $PX_SERVER (start it first, see docs/e2e.md)" >&2
  exit 1
fi
ok "GET /v1/tasks responded"
if [[ -n $PVE_SSH ]]; then
  if node pct list >/dev/null 2>&1; then
    ok "ssh to the node works"
  else
    echo "FATAL: ssh to $PVE_SSH failed" >&2
    exit 1
  fi
fi

say "1. the shipped templates list with PX-OK true"
if wait_listed px-agent-debian12; then
  desc=$("$PX" describe template px-agent-debian12)
  if grep -q '"unprivileged": true' <<<"$desc" && grep -q '"dhcp": true' <<<"$desc" \
      && grep -q '"pxOk": true' <<<"$desc"; then
    ok "describe carries the agent template's facts and verdict"
  else
    bad "describe wrong for px-agent-debian12: $desc"
  fi
  if wait_listed px-runner-debian12; then
    row=$("$PX" get templates | awk '$1=="px-runner-debian12" {print $4}')
    if [[ $row == true ]]; then
      ok "runner template lists PX-OK true"
    else
      bad "runner template PX-OK column is '$row'"
    fi
  fi
fi

if [[ -n $PVE_SSH ]]; then
  say "2. a user-authored minimal template built per the contract lists PX-OK"
  node "pveam update" >/dev/null 2>&1 || true
  vol=$(node "pveam available --section system | awk '/debian-12-standard/ {print \$2; exit}'")
  image=${vol##*/}
  [[ -n "$image" ]] || { bad "no debian-12 image found on the node"; exit 1; }
  if ! node "pvesm list $TPL_STORAGE --content vztmpl 2>/dev/null" | grep -qF "$image"; then
    node "pveam download $TPL_STORAGE $image" >/dev/null
  fi
  CTID_OK=$(node "pvesh get /cluster/nextid")
  node "pct create $CTID_OK $TPL_STORAGE:vztmpl/$image \
    --hostname $NAME_OK \
    --net0 name=eth0,bridge=$BRIDGE,ip=dhcp \
    --unprivileged 1 \
    --rootfs $STORAGE:2" >/dev/null
  node "pct template $CTID_OK" >/dev/null
  if wait_listed "$NAME_OK"; then
    if "$PX" get templates | awk -v n="$NAME_OK" '$1==n {exit !($4=="true")}'; then
      ok "contract-conforming template lists PX-OK true"
    else
      bad "$NAME_OK lists PX-OK false: $("$PX" describe template "$NAME_OK")"
    fi
  fi

  say "3. a template violating the contract names what fails"
  CTID_BAD=$(node "pvesh get /cluster/nextid")
  node "pct create $CTID_BAD $TPL_STORAGE:vztmpl/$image \
    --hostname $NAME_BAD \
    --net0 name=eth0,bridge=$BRIDGE,ip=192.168.2.249/24,gw=192.168.2.1 \
    --unprivileged 0 \
    --rootfs $STORAGE:2" >/dev/null
  node "pct template $CTID_BAD" >/dev/null
  if wait_listed "$NAME_BAD"; then
    desc=$("$PX" describe template "$NAME_BAD")
    row=$("$PX" get templates | awk -v n="$NAME_BAD" '$1==n')
    if grep -q '"pxOk": false' <<<"$desc" \
        && grep -q '"unprivileged": false' <<<"$desc" \
        && grep -q '"dhcp": false' <<<"$desc" \
        && grep -q 'unprivileged' <<<"$row" \
        && grep -q 'net0 ip=dhcp' <<<"$row"; then
      ok "violating template lists PX-OK false naming both requirements"
    else
      bad "verdict wrong for $NAME_BAD: $desc"
    fi
  fi
else
  echo "SKIP: node-side checks (PVE_SSH is empty)"
fi

# Node-side only because the probe templates above need ssh; this check
# itself is pure px API, so it runs whenever the server is reachable.
say "4. a task naming an unknown image fails at provision with the template named"
"$PX" run -image no-such-tmpl-e2e -name e2e-tmpl-gone -no-wait "must not run" >/dev/null
if wait_phase e2e-tmpl-gone ProvisionFailed; then
  if grep -q "no-such-tmpl-e2e" <("$PX" describe task e2e-tmpl-gone); then
    ok "ProvisionFailed reason names the missing template"
  else
    bad "reason does not name the template: $("$PX" describe task e2e-tmpl-gone)"
  fi
fi

say "5. cleanup"
destroy_probe
CTID_OK=""
CTID_BAD=""
"$PX" delete task e2e-tmpl-gone >/dev/null 2>&1 || true

printf '\n== results: %d passed, %d failed\n' "$pass" "$fail"
(( fail == 0 )) || exit 1
echo "E2E templates test PASSED"
