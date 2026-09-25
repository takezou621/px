#!/usr/bin/env bash
# E2E: px suspend / px resume — cgroup-freeze based pause.
#
# Requires (see docs/e2e.md for setup):
#   - px-server running (single-node or cluster mode both exercise the
#     same node-level freeze path)
#   - the runner template built on the node (px-runner-debian12)
#
# Env:
#   PX_SERVER  px-server URL            (default http://127.0.0.1:7420)
#   PX         px CLI binary path       (default ./px)
#   TIMEOUT    seconds to wait per task (default 300)
#   PVE_SSH    ssh target for node-level freeze checks
#              (default root@<node>; empty disables the checks)
#
# Checks: Suspended lands with the cgroup actually frozen, exec refuses
# (409) while frozen, resume returns to Running with exec working again,
# idempotent suspend/resume are 200s, a Suspended task that is thawed
# out of band gets re-frozen by the controller (declarative state),
# deleting a frozen task thaws first and leaves no container, and
# terminal tasks refuse suspend with 409. Then cleanup.
set -euo pipefail

export PX_SERVER="${PX_SERVER:-http://127.0.0.1:7420}"
PX="${PX:-./px}"
TIMEOUT="${TIMEOUT:-300}"
PVE_SSH="${PVE_SSH-root@192.168.2.100}"

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
wait_gone() { # name — gone means absent from the list (a failed task still shows)
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

# node_frozen vmid host — reads frozen from the container's cgroup v2
# cgroup.events through the same node-level path the controller uses
# (API pid -> /proc/<pid>/cgroup -> cgroup.freeze), never pct exec,
# mirroring the provisioner's reason for avoiding pct exec.
node_frozen() { # vmid -> echoes 0/1, 2 means the check itself failed
  ssh -o BatchMode=yes "$PVE_SSH" '
    pid=$(pct status '"$1"' --verbose 2>/dev/null | sed -nE "s/^[pP][iI][dD]:[[:space:]]*//p")
    [[ -n $pid ]] || exit 2
    cg=$(sed -n "s/^0:://p" /proc/$pid/cgroup)
    [[ -n $cg ]] || exit 2
    awk "\$1==\"frozen\"{print \$2}" "/sys/fs/cgroup$cg/cgroup.events" 2>/dev/null || exit 2
  '
}

cleanup() {
  "$PX" delete task e2e-susp >/dev/null 2>&1 || true
  "$PX" delete task e2e-susp-del >/dev/null 2>&1 || true
  "$PX" delete task e2e-susp-gone >/dev/null 2>&1 || true
}
trap cleanup EXIT

say "0. px-server is reachable"
if "$PX" get tasks >/dev/null 2>&1; then
  ok "GET /v1/tasks responded"
else
  echo "FATAL: cannot reach px-server at $PX_SERVER (start it first, see docs/e2e.md)" >&2
  exit 1
fi

say "1. suspend freezes the container: Running -> Suspending -> Suspended"
apply <<'EOF'
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: e2e-susp
spec:
  image: px-runner-debian12
  runner:
    command: ["sleep", "900"]
EOF
wait_phase e2e-susp Running
vmid=$("$PX" describe task e2e-susp | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"]["container"])')
if [[ -n $PVE_SSH ]]; then
  f=$(node_frozen "$vmid")
  [[ $f == 0 ]] && ok "cgroup not frozen before suspend" || bad "cgroup frozen state '$f' before suspend, want 0"
fi
"$PX" suspend task e2e-susp >/dev/null
wait_phase e2e-susp Suspended
if [[ -n $PVE_SSH ]]; then
  f=$(node_frozen "$vmid")
  [[ $f == 1 ]] && ok "cgroup actually frozen on the node" || bad "cgroup frozen state '$f' after suspend, want 1"
fi

say "2. exec refuses a frozen task (it would hang inside the freezer)"
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$PX_SERVER/v1/tasks/e2e-susp/exec" \
  ${PX_TOKEN:+-H "Authorization: Bearer $PX_TOKEN"} \
  -H 'Content-Type: application/json' -d '{"command":["true"]}')
[[ $code == 409 ]] && ok "exec on Suspended -> 409" || bad "exec on Suspended -> $code, want 409"

say "3. idempotent suspend/resume are 200s"
code=$("$PX" suspend task e2e-susp >/dev/null 2>&1; echo $?)
[[ $code == 0 ]] && ok "second suspend -> 200" || bad "second suspend exited $code"
code=$("$PX" get tasks >/dev/null 2>&1 && "$PX" describe task e2e-susp >/dev/null 2>&1; echo $?)
[[ $code == 0 ]] && ok "get/describe work while Suspended" || bad "get/describe while Suspended exited $code"
"$PX" resume task e2e-susp >/dev/null 2>&1
sleep 2
"$PX" resume task e2e-susp >/dev/null 2>&1
wait_phase e2e-susp Running
code=$("$PX" resume task e2e-susp >/dev/null 2>&1; echo $?)
[[ $code == 0 ]] && ok "resume while running -> 200 (idempotent)" || bad "resume while running exited $code"

say "4. after resume, exec works again and the process kept its state"
got=$("$PX" exec e2e-susp -- sh -c 'echo alive')
[[ $got == alive ]] && ok "exec works after resume" || bad "exec after resume failed: '$got'"

say "5. an out-of-band thaw is re-frozen (declarative Suspended)"
"$PX" suspend task e2e-susp >/dev/null
wait_phase e2e-susp Suspended
if [[ -n $PVE_SSH ]]; then
  vmid=$("$PX" describe task e2e-susp | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"]["container"])')
  # Thaw through the node the same way the controller would — but without
  # telling px, as a manual pct-level freeze lift would.
  ssh -o BatchMode=yes "$PVE_SSH" '
    pid=$(pct status '"$vmid"' --verbose 2>/dev/null | sed -nE "s/^[pP][iI][dD]:[[:space:]]*//p")
    cg=$(sed -n "s/^0:://p" /proc/$pid/cgroup)
    echo 0 > "/sys/fs/cgroup$cg/cgroup.freeze"
  '
  f=$(node_frozen "$vmid")
  [[ $f == 0 ]] && ok "out-of-band thaw landed" || bad "thaw did not land (frozen '$f')"
  # The controller must re-freeze within a few reconcile ticks; give it
  # up to 15s and require frozen to be back.
  refrozen=""
  for _ in 1 2 3 4 5; do
    sleep 3
    f=$(node_frozen "$vmid")
    if [[ $f == 1 ]]; then refrozen=yes; break; fi
  done
  [[ -n $refrozen ]] && ok "controller re-froze the task" || bad "task still thawed (frozen '$f')"
fi
"$PX" resume task e2e-susp >/dev/null
wait_phase e2e-susp Running

say "6. deleting a frozen task thaws first and leaves nothing behind"
apply <<'EOF'
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: e2e-susp-del
spec:
  image: px-runner-debian12
  runner:
    command: ["sleep", "900"]
EOF
wait_phase e2e-susp-del Running
delvmid=$("$PX" describe task e2e-susp-del | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"]["container"])')
"$PX" suspend task e2e-susp-del >/dev/null
wait_phase e2e-susp-del Suspended
"$PX" delete task e2e-susp-del >/dev/null
wait_gone e2e-susp-del
if [[ -n $PVE_SSH ]]; then
  left=yes
  for _ in 1 2 3 4 5; do
    if ! ssh -o BatchMode=yes "$PVE_SSH" 'pct status '"$delvmid"' >/dev/null 2>&1'; then left=no; break; fi
    sleep 2
  done
  [[ $left == no ]] && ok "frozen task destroyed, no container left" || bad "ct $delvmid still on the node"
fi

say "7. terminal tasks refuse suspend with 409"
apply <<'EOF'
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: e2e-susp-gone
spec:
  image: no-such-template-xyz
  runner:
    command: ["true"]
EOF
wait_phase e2e-susp-gone ProvisionFailed
code=$("$PX" suspend task e2e-susp-gone >/dev/null 2>&1; echo $?)
[[ $code != 0 ]] && ok "suspend on ProvisionFailed refused (non-zero exit)" || bad "suspend on terminal task succeeded"

say "8. cleanup"
"$PX" delete task e2e-susp-gone >/dev/null 2>&1 || true
"$PX" delete task e2e-susp >/dev/null
wait_gone e2e-susp-gone
wait_gone e2e-susp

printf '\n== results: %d passed, %d failed\n' "$pass" "$fail"
(( fail == 0 )) && echo "E2E suspend/resume test PASSED" || { echo "E2E suspend/resume test FAILED"; exit 1; }
