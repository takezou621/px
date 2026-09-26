#!/usr/bin/env bash
# E2E: quota caps — tasks park instead of failing when the cap is full.
#
# Requires (see docs/e2e.md for setup):
#   - px-server started WITH the cap, e.g.:
#       px-server -max-running-tasks 1 ... (other flags as usual)
#   - the runner template built on the node (px-runner-debian12)
#
# Env:
#   PX_SERVER  px-server URL            (default http://127.0.0.1:7420)
#   PX         px CLI binary path       (default ./px)
#   TIMEOUT    seconds to wait per task (default 300)
#
# Checks: at a full cap a second task parks Pending with the
# "waiting for capacity" reason holding no container, suspending it is a
# 409, repeated ticks fire only one CapacityWait event, px_quota_waiting
# tracks the parked count, deleting the admitted task lets the parked one
# advance, and the gauge returns to 0. Then cleanup.
set -euo pipefail

export PX_SERVER="${PX_SERVER:-http://127.0.0.1:7420}"
PX="${PX:-./px}"
TIMEOUT="${TIMEOUT:-300}"

pass=0
fail=0
say() { printf '\n== %s\n' "$*"; }
ok()  { pass=$((pass+1)); echo "PASS: $*"; }
bad() { fail=$((fail+1)); echo "FAIL: $*"; }

phase() { "$PX" get tasks 2>/dev/null | awk -v n="$1" '$1==n {print $2}'; }
# Read the API, not `px describe`: describe appends an Events section after
# the JSON, which would break json.load. `container` is omitempty — a parked
# task has no container key, read it as 0.
describe_json() { curl -sf "$PX_SERVER/v1/tasks/$1" ${PX_TOKEN:+-H "Authorization: Bearer $PX_TOKEN"}; }
reasonof() { describe_json "$1" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"].get("reason",""))'; }
containerof() { describe_json "$1" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"].get("container",0))'; }
metrics_get() { curl -sf "$PX_SERVER/v1/metrics" ${PX_TOKEN:+-H "Authorization: Bearer $PX_TOKEN"}; }
quota_waiting() { metrics_get | awk '$1=="px_quota_waiting"{print $2}'; }
capacity_events() { curl -sf "$PX_SERVER/v1/tasks/$1/events" ${PX_TOKEN:+-H "Authorization: Bearer $PX_TOKEN"} \
  | python3 -c 'import json,sys; print(sum(1 for e in json.load(sys.stdin) if e["reason"]=="CapacityWait"))'; }
apply() { "$PX" apply -f - >/dev/null; }

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
wait_parked() { # name — Pending + "waiting for capacity"
  local name=$1 waited=0
  while (( waited < TIMEOUT )); do
    if [[ $(phase "$name") == "Pending" && $(reasonof "$name") == "waiting for capacity" ]]; then
      ok "$name parked at the cap (${waited}s)"
      return 0
    fi
    sleep 2
    waited=$((waited+2))
  done
  bad "$name not parked (phase '$(phase "$name")', reason '$(reasonof "$name")')"
  return 1
}
wait_gone() { # name — gone means absent from the list
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

cleanup() {
  "$PX" delete task e2e-quota-a >/dev/null 2>&1 || true
  "$PX" delete task e2e-quota-b >/dev/null 2>&1 || true
}
trap cleanup EXIT

say "0. px-server is reachable and reports the cap"
if "$PX" get tasks >/dev/null 2>&1; then
  ok "GET /v1/tasks responded"
else
  echo "FATAL: cannot reach px-server at $PX_SERVER (start it with -max-running-tasks 1 first, see docs/e2e.md)" >&2
  exit 1
fi

say "1. the first task admits normally"
apply <<'EOF'
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: e2e-quota-a
spec:
  image: px-runner-debian12
  runner:
    command: ["sleep", "900"]
EOF
wait_phase e2e-quota-a Running
[[ $(quota_waiting) == 0 ]] && ok "px_quota_waiting is 0 while under the cap" \
  || bad "px_quota_waiting = $(quota_waiting), want 0 under the cap"

say "2. the second task parks at the cap, holding no container"
apply <<'EOF'
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: e2e-quota-b
spec:
  image: px-runner-debian12
  runner:
    command: ["sleep", "900"]
EOF
wait_parked e2e-quota-b
c=$(containerof e2e-quota-b)
[[ $c == 0 ]] && ok "parked task holds no container" || bad "parked task holds container $c, want 0"

say "3. suspending a parked task is refused (it holds nothing to freeze)"
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$PX_SERVER/v1/tasks/e2e-quota-b/suspend" \
  ${PX_TOKEN:+-H "Authorization: Bearer $PX_TOKEN"} \
  -H 'Content-Type: application/json' -d '{}')
[[ $code == 409 ]] && ok "suspend on parked task -> 409" || bad "suspend on parked task -> $code, want 409"

say "4. repeated ticks fire exactly one CapacityWait event"
sleep 12  # ~6 reconcile ticks at the default 2s interval
[[ $(phase e2e-quota-b) == "Pending" && $(reasonof e2e-quota-b) == "waiting for capacity" ]] \
  && ok "still parked after many ticks" || bad "parked task drifted: $(phase e2e-quota-b) / '$(reasonof e2e-quota-b)'"
n=$(capacity_events e2e-quota-b)
[[ $n == 1 ]] && ok "exactly one CapacityWait event" || bad "CapacityWait events = $n, want 1"
[[ $(quota_waiting) == 1 ]] && ok "px_quota_waiting is 1 while parked" \
  || bad "px_quota_waiting = $(quota_waiting), want 1 while parked"

say "5. deleting the admitted task frees the slot; the parked one advances"
"$PX" delete task e2e-quota-a >/dev/null
wait_gone e2e-quota-a
wait_phase e2e-quota-b Running
[[ $(quota_waiting) == 0 ]] && ok "px_quota_waiting back to 0" \
  || bad "px_quota_waiting = $(quota_waiting), want 0 after the slot freed"

say "6. cleanup"
"$PX" delete task e2e-quota-b >/dev/null
wait_gone e2e-quota-b

printf '\n== results: %d passed, %d failed\n' "$pass" "$fail"
(( fail == 0 )) && echo "E2E quota test PASSED" || { echo "E2E quota test FAILED"; exit 1; }
