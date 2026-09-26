#!/usr/bin/env bash
# E2E: Schedule — cron-driven task generation.
#
# Requires (see docs/e2e.md for setup):
#   - px-server running (single-node or cluster mode both exercise the
#     same controller fire path)
#   - the runner template built on the node (px-runner-debian12)
#
# Env:
#   PX_SERVER  px-server URL            (default http://127.0.0.1:7420)
#   PX         px CLI binary path       (default ./px)
#
# The fire cadence is minute-boundary, so this run takes ~10 minutes of
# real wall-clock time. Checks: apply fires a real task to Succeeded
# (with a Scheduled event and scheduleOwner), suspend holds across a
# minute boundary, resume fires exactly once for the suspended window
# (missed-fire compression), historyLimit prunes finished generated
# tasks, delete is definition-only (generated tasks stay).
#
# The restart-does-not-double-fire check needs px-server restarted by
# hand between two runs — docs/e2e.md carries the procedure.
set -euo pipefail

export PX_SERVER="${PX_SERVER:-http://127.0.0.1:7420}"
PX="${PX:-./px}"

pass=0
fail=0
say() { printf '\n== %s\n' "$*"; }
ok()  { pass=$((pass+1)); echo "PASS: $*"; }
bad() { fail=$((fail+1)); echo "FAIL: $*"; }

phase() { "$PX" get tasks 2>/dev/null | awk -v n="$1" '$1==n {print $2}'; }
# Fired tasks are named <schedule>-<unixsec>, so anchor on digits: the
# prefix e2e-sched- must not swallow e2e-sched-win-<n> as well.
count_tasks() {
  "$PX" get tasks 2>/dev/null | awk -v p="$1" '$1 ~ ("^" p "[0-9]+$") {c++} END {print c+0}'
}
sched_task() { # prefix idx — idx-th (1-based, time-ordered) fired task
  "$PX" get tasks 2>/dev/null | awk -v p="$1" '$1 ~ ("^" p "[0-9]+$") {print $1}' | sort | sed -n "$2 p"
}
wait_for_count() { # prefix want budget
  local prefix=$1 want=$2 budget=$3 waited=0 n
  while (( waited < budget )); do
    n=$(count_tasks "$prefix")
    if (( n == want )); then
      ok "$prefix* count == $want (${waited}s)"
      return 0
    fi
    sleep 2
    waited=$((waited+2))
  done
  bad "$prefix* count = $(count_tasks "$prefix"), want $want within ${budget}s"
  return 1
}
wait_phase() { # name want budget
  local name=$1 want=$2 budget=${3:-120} waited=0
  while (( waited < budget )); do
    if [[ $(phase "$name") == "$want" ]]; then
      ok "$name -> $want (${waited}s)"
      return 0
    fi
    sleep 2
    waited=$((waited+2))
  done
  bad "$name did not reach $want within ${budget}s (last: '$(phase "$name")')"
  return 1
}
apply() { "$PX" apply -f - >/dev/null; }

cleanup() {
  "$PX" delete schedule e2e-sched >/dev/null 2>&1 || true
  # e2e-sched-win stays (suspended, clock held) for the restart-recovery
  # check in docs/e2e.md section 4; the closing hint prints its teardown.
  for t in $("$PX" get tasks 2>/dev/null | awk '$1 ~ /^e2e-sched/ {print $1}'); do
    "$PX" delete task "$t" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

say "0. px-server is reachable"
if "$PX" get tasks >/dev/null 2>&1; then
  ok "GET /v1/tasks responded"
else
  echo "FATAL: cannot reach px-server at $PX_SERVER (start it first, see docs/e2e.md)" >&2
  exit 1
fi

say "1. apply fires a real task and it reaches Succeeded"
apply <<'EOF'
apiVersion: px.io/v1alpha1
kind: Schedule
metadata:
  name: e2e-sched
spec:
  schedule: "* * * * *"
  taskTemplate:
    image: px-runner-debian12
    runner:
      command: ["true"]
EOF
# First fire is the next minute boundary after apply (the clock starts
# at never-fired).
wait_for_count e2e-sched- 1 90 || true
first=$(sched_task e2e-sched- 1)
[[ -n $first ]] && ok "first fire stamped $first" || bad "no fired task to inspect"
wait_phase "$first" Succeeded
# describe appends a human Events section after the JSON; raw_decode takes
# the leading JSON object only.
owner=$("$PX" describe task "$first" | python3 -c 'import json,sys; print(json.JSONDecoder().raw_decode(sys.stdin.read())[0]["status"].get("scheduleOwner",""))')
[[ $owner == e2e-sched ]] && ok "task carries scheduleOwner=e2e-sched" || bad "scheduleOwner='$owner', want e2e-sched"
ev=$("$PX" events "$first" 2>/dev/null | grep -c "fired by schedule e2e-sched" || true)
(( ev >= 1 )) && ok "Scheduled event recorded" || bad "no 'fired by schedule' event"
# SUSPEND is the 7th whitespace column: NAME, then the five words of the
# cron expression, then SUSPEND.
row=$("$PX" get schedules 2>/dev/null | awk '$1=="e2e-sched" {print $7}')
[[ $row == "false" ]] && ok "get schedules shows SUSPEND=false" || bad "get schedules SUSPEND='$row', want false"
dsout=$("$PX" describe schedule e2e-sched 2>&1)
if echo "$dsout" | grep -q "$first"; then
  ok "describe schedule lists the fired task"
else
  bad "describe schedule does not list $first (out: '$(echo "$dsout" | head -3 | tr '\n' ' ')')"
fi

say "2. suspend holds across a minute boundary"
"$PX" suspend schedule e2e-sched >/dev/null
# Fire happens within ~2s of the boundary; if we waited only until the
# boundary we could race a not-yet-observed fire. 70s clears one full
# boundary plus margin, and the count must stay at 1.
sleep 70
n=$(count_tasks e2e-sched-)
(( n == 1 )) && ok "no fire while suspended" || bad "fired while suspended: count=$n"

say "3. resume fires exactly once for the suspended window"
"$PX" resume schedule e2e-sched >/dev/null
# The suspended window spans >=1 missed boundary; compression must turn
# it into ONE task at the next boundary, not a replay.
wait_for_count e2e-sched- 2 90 || true
sleep 8
n=$(count_tasks e2e-sched-)
(( n == 2 )) && ok "resume compressed the missed window to one fire" || bad "count=$n after resume+settle, want 2"
second=$(sched_task e2e-sched- 2)
[[ $second != "$first" ]] && ok "newest fire is a new task ($second)" || bad "no new task after resume"
# Freeze the base schedule: section 4 takes minutes and it would keep
# firing every minute, inflating section 5's survivor count.
"$PX" suspend schedule e2e-sched >/dev/null

say "4. historyLimit prunes finished generated tasks"
apply <<'EOF'
apiVersion: px.io/v1alpha1
kind: Schedule
metadata:
  name: e2e-sched-win
spec:
  schedule: "* * * * *"
  historyLimit: 1
  taskTemplate:
    image: px-runner-debian12
    runner:
      command: ["true"]
EOF
wait_for_count e2e-sched-win- 1 90 || true
old=$(sched_task e2e-sched-win- 1)
wait_phase "$old" Succeeded
# After the second fire the controller must prune the first. Poll until
# the window holds exactly one task and it is not the old one.
kept=""
for _ in $(seq 1 90); do
  if (( $(count_tasks e2e-sched-win-) == 1 )); then
    kept=$(sched_task e2e-sched-win- 1)
    [[ $kept != "$old" ]] && break
  fi
  sleep 2
done
[[ -n $kept && $kept != "$old" ]] \
  && ok "historyLimit=1 pruned $old, kept $kept" \
  || bad "prune failed: old=$old present=$(count_tasks e2e-sched-win-) kept='$kept'"

say "5. delete is definition-only: generated tasks stay"
"$PX" delete schedule e2e-sched >/dev/null
code=$(curl -s -o /dev/null -w '%{http_code}' "$PX_SERVER/v1/schedules/e2e-sched" \
  ${PX_TOKEN:+-H "Authorization: Bearer $PX_TOKEN"})
[[ $code == 404 ]] && ok "schedule definition gone" || bad "GET schedule after delete -> $code, want 404"
n=$(count_tasks e2e-sched-)
(( n == 2 )) && ok "generated tasks survived the schedule delete" || bad "generated tasks lost: count=$n"
sleep 35
n=$(count_tasks e2e-sched-)
(( n == 2 )) && ok "no new fire after the schedule is gone" || bad "fired after delete: count=$n"
# e2e-sched-win keeps firing; leave it for the restart procedure, stop it here.
"$PX" suspend schedule e2e-sched-win >/dev/null

printf '\n== results: %d passed, %d failed\n' "$pass" "$fail"
(( fail == 0 )) && echo "E2E schedules test PASSED" || { echo "E2E schedules test FAILED"; exit 1; }

cat <<'EOF'

Restart double-fire check (manual):
  e2e-sched-win is now suspended with its fire clock held. Restart
  px-server (see docs/e2e.md), then run:
    ./px resume schedule e2e-sched-win
  and within ~65s expect exactly ONE new e2e-sched-win-* task — the
  pre-restart lastScheduleTime must suppress a duplicate fire. Then
  delete the schedule and its tasks:
    ./px delete schedule e2e-sched-win
    ./px delete task $(./px get tasks | awk '$1 ~ /^e2e-sched-win-/ {print $1}')
EOF
