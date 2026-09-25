#!/usr/bin/env bash
# E2E: multi-node scheduling — cluster-mode px-server picks a node.
#
# Requires a px-server started WITHOUT -pve-node (cluster mode) against
# a PVE cluster with the runner template on at least one online node.
# Start it like (see docs/e2e.md "Cluster mode"):
#   go run ./cmd/px-server -tls-insecure \
#     -ssh-host-override "third=192.168.2.100,second=192.168.2.183"
#
# Env:
#   PX_SERVER     px-server URL          (default http://127.0.0.1:7420)
#   PX            px CLI binary path     (default ./px)
#   TIMEOUT       seconds to wait per task (default 300)
#   PVE_SSH       ssh target of one template-bearing node, used for the
#                 node-level leftover check (default root@192.168.2.100;
#                 empty disables the checks)
#   NODE_HOST_MAP node=host pairs the script resolves NODE column values
#                 through, mirroring the server's -ssh-host-override
#                 (default third=192.168.2.100,second=192.168.2.183)
#
# Checks: a task schedules onto a node that is online AND holds the
# image (NODE column non-empty and one of the template-bearing nodes),
# the container really lives on that node, exec/logs/suspend/resume all
# cross the node boundary, delete removes the container from that same
# node, and the NODE column stays stable across polls. Then cleanup.
set -euo pipefail

export PX_SERVER="${PX_SERVER:-http://127.0.0.1:7420}"
PX="${PX:-./px}"
TIMEOUT="${TIMEOUT:-300}"
PVE_SSH="${PVE_SSH-root@192.168.2.100}"
NODE_HOST_MAP="${NODE_HOST_MAP-third=192.168.2.100,second=192.168.2.183}"
TEMPLATE_NODES="${TEMPLATE_NODES-third second}"

pass=0
fail=0
say() { printf '\n== %s\n' "$*"; }
ok()  { pass=$((pass+1)); echo "PASS: $*"; }
bad() { fail=$((fail+1)); echo "FAIL: $*"; }

phase() { "$PX" get tasks 2>/dev/null | awk -v n="$1" '$1==n {print $2}'; }
nodeof() { "$PX" get tasks 2>/dev/null | awk -v n="$1" '$1==n {print $3}'; }
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
wait_gone() { # name
  local name=$1 waited=0 listing
  while (( waited < 120 )); do
    if ! listing=$("$PX" get tasks 2>/dev/null); then
      bad "get tasks failed while waiting for $name"
      return 1
    fi
    if ! grep -q "^$name " <<<"$listing"; then
      ok "$name removed"
      return 0
    fi
    sleep 2
    waited=$((waited+2))
  done
  bad "$name still present after delete"
  return 1
}
apply() { "$PX" apply -f - >/dev/null; }

# node_ssh_target NODE — NODE column value to an ssh host, through the
# same mapping the server's -ssh-host-override carries.
node_ssh_target() {
  local node=$1 pair host
  if [[ $node == "$PVE_SSH"* ]]; then
    echo "$node"; return
  fi
  IFS=',' read -ra pairs <<<"$NODE_HOST_MAP"
  for pair in "${pairs[@]}"; do
    host=${pair#*=}
    if [[ ${pair%%=*} == "$node" ]]; then
      # reuse the same login as PVE_SSH (its user part)
      echo "${PVE_SSH%%@*}@$host"
      return
    fi
  done
  echo "$node"
}

cleanup() {
  "$PX" delete task e2e-multi >/dev/null 2>&1 || true
}
trap cleanup EXIT

say "0. px-server is reachable and in cluster mode"
if ! "$PX" get tasks >/dev/null 2>&1; then
  echo "FATAL: cannot reach px-server at $PX_SERVER (start it first, see docs/e2e.md)" >&2
  exit 1
fi
# The NODE column header must exist — cluster-aware listing.
if "$PX" get tasks 2>/dev/null | head -1 | grep -qi node; then
  ok "listing carries the NODE column"
else
  bad "NODE column missing from px get tasks"
fi

say "1. a task schedules onto an online node that holds the image"
apply <<'EOF'
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: e2e-multi
spec:
  image: px-runner-debian12
  runner:
    command: ["sleep", "900"]
EOF
wait_phase e2e-multi Running
picked=$(nodeof e2e-multi)
if [[ -n $picked ]]; then
  ok "scheduled onto node '$picked'"
else
  bad "NODE column empty for a cluster-mode task"
fi
found=no
for tn in $TEMPLATE_NODES; do
  [[ $picked == "$tn" ]] && found=yes
done
[[ $found == yes ]] && ok "node '$picked' is a template-bearing node" \
  || bad "node '$picked' is not among: $TEMPLATE_NODES"

say "2. NODE column is stable across polls"
stable=yes
for _ in 1 2 3; do
  [[ $(nodeof e2e-multi) == "$picked" ]] || stable=no
  sleep 1
done
[[ $stable == yes ]] && ok "node stayed '$picked'" || bad "NODE column flickered"

say "3. the container really lives on that node"
if [[ -n $PVE_SSH ]]; then
  target=$(node_ssh_target "$picked")
  vmid=$("$PX" describe task e2e-multi | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"]["container"])')
  if ssh -o BatchMode=yes "$target" "pct status $vmid" >/dev/null 2>&1; then
    ok "ct $vmid found on '$picked' (via $target)"
  else
    bad "ct $vmid not found on '$picked' (via $target)"
  fi
fi

say "4. exec and logs cross the node boundary"
got=$("$PX" exec e2e-multi -- sh -c 'hostname')
[[ -n $got ]] && ok "exec returned from '$picked'" || bad "exec failed"
"$PX" logs e2e-multi >/dev/null 2>&1 && ok "logs fetched across nodes" || bad "logs failed"

say "5. suspend/resume freeze the task on its own node"
"$PX" suspend task e2e-multi >/dev/null
wait_phase e2e-multi Suspended
if [[ -n $PVE_SSH ]]; then
  target=$(node_ssh_target "$picked")
  f=$(ssh -o BatchMode=yes "$target" '
    pid=$(pct status '"$vmid"' --verbose 2>/dev/null | sed -nE "s/^[pP][iI][dD]:[[:space:]]*//p")
    cg=$(sed -n "s/^0:://p" /proc/$pid/cgroup)
    awk "\$1==\"frozen\"{print \$2}" "/sys/fs/cgroup$cg/cgroup.events"' 2>/dev/null || echo err)
  [[ $f == 1 ]] && ok "cgroup frozen on '$picked'" || bad "frozen state '$f' on '$picked'"
fi
"$PX" resume task e2e-multi >/dev/null
wait_phase e2e-multi Running

say "6. delete removes the container from the same node"
"$PX" delete task e2e-multi >/dev/null
wait_gone e2e-multi
if [[ -n $PVE_SSH ]]; then
  target=$(node_ssh_target "$picked")
  left=yes
  for _ in 1 2 3 4 5; do
    if ! ssh -o BatchMode=yes "$target" 'pct status '"$vmid"' >/dev/null 2>&1'; then left=no; break; fi
    sleep 2
  done
  [[ $left == no ]] && ok "ct $vmid gone from '$picked'" || bad "ct $vmid left on '$picked'"
fi

printf '\n== results: %d passed, %d failed\n' "$pass" "$fail"
(( fail == 0 )) && echo "E2E multi-node test PASSED" || { echo "E2E multi-node test FAILED"; exit 1; }
