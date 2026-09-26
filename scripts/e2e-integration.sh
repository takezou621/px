#!/usr/bin/env bash
# E2E integration: one realistic cycle across every kind in one flow.
#
# The other e2e scripts pin one feature each; this one walks the user's
# actual path through the system — a single multi-document apply, then
# the kinds cooperating — so a regression that only shows up in the
# combination is caught here.
#
# Requires (see docs/e2e.md for setup):
#   - px-server running with a working PVE token and node SSH access
#   - the runner template built on the node (px-runner-debian12)
#   - outbound internet from the task sandbox (the workspace clone, DNS)
#
# Env:
#   PX_SERVER   px-server URL            (default http://127.0.0.1:7420)
#   PX          px CLI binary path       (default ./px)
#   TIMEOUT     seconds to wait per task (default 300)
#   PVE_SSH     ssh target for node-level checks
#               (default root@192.168.2.100; empty disables the checks)
#   ALLOW_DEST  IP the fenced task may reach on tcp/8006
#               (default 192.168.2.100)
#   REPO        git repo the workspace clones
#               (default https://github.com/takezou621/px.git)
#
# The flow:
#   1. a single multi-document apply creates Model + Gateway + Workspace +
#      Schedule + two Tasks; the tasks reference the other kinds by name
#   2. the main task clones the workspace, sees the model env, survives a
#      suspend/resume mid-flight, and finishes; its finish captures a
#      named session
#   3. px run --continue-session restores that capture into a fresh task
#   4. the fenced task proves the gateway: PVE endpoint allowed, wider
#      internet blocked, DNS allowed through
#   5. the schedule fires at the minute boundary; suspend is declarative
#   6. metrics agree with what just happened
#   7. everything is deleted; no px- containers left on the node
set -euo pipefail

export PX_SERVER="${PX_SERVER:-http://127.0.0.1:7420}"
PX="${PX:-./px}"
TIMEOUT="${TIMEOUT:-300}"
PVE_SSH="${PVE_SSH-root@192.168.2.100}"
ALLOW_DEST="${ALLOW_DEST:-192.168.2.100}"
REPO="${REPO:-https://github.com/takezou621/px.git}"

MAIN=e2e-int-main
FENCED=e2e-int-fenced
NEXT=e2e-int-next
WS=e2e-int-ws
MODEL=e2e-int-model
GW=e2e-int-gw
SCHED=e2e-int-sched
CONV=e2e-int-conv
TPL=px-runner-debian12

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
describe_json() { curl -sf "$PX_SERVER/v1/tasks/$1" ${PX_TOKEN:+-H "Authorization: Bearer $PX_TOKEN"}; }
sess_row() { "$PX" get sessions 2>/dev/null | awk -v n="$1" '$1==n'; }
sched_count() { "$PX" get tasks 2>/dev/null | awk -v p="$SCHED-" '$1 ~ ("^" p "[0-9]+$") {c++} END {print c+0}'; }
wait_sched_count() { # want max-seconds
  local want=$1 limit=$2 waited=0
  while (( waited < limit )); do
    if (( $(sched_count) >= want )); then
      ok "schedule has fired $want task(s) (${waited}s)"
      return 0
    fi
    sleep 5
    waited=$((waited+5))
  done
  bad "schedule fired $(sched_count) task(s), want >= $want within ${limit}s"
  return 1
}
metrics_get() { curl -sf "$PX_SERVER/v1/metrics" ${PX_TOKEN:+-H "Authorization: Bearer $PX_TOKEN"}; }
succ_metric() { metrics_get | awk '$1 ~ /^px_tasks\{.*phase="Succeeded"/ {s+=$2} END{print s+0}'; }
quota_waiting() { metrics_get | awk '$1=="px_quota_waiting"{print $2}'; }

# node_frozen vmid — reads frozen from the container's cgroup v2
# cgroup.events through the same node-level path the controller uses
# (API pid -> /proc/<pid>/cgroup -> cgroup.freeze), never pct exec,
# mirroring the provisioner's reason for avoiding pct exec.
node_frozen() { # vmid -> echoes 0/1, 2 means the check itself failed
  ssh -o BatchMode=yes -o ConnectTimeout=5 "$PVE_SSH" '
    pid=$(pct status '"$1"' --verbose 2>/dev/null | sed -nE "s/^[pP][iI][dD]:[[:space:]]*//p")
    [[ -n $pid ]] || exit 2
    cg=$(sed -n "s/^0:://p" /proc/$pid/cgroup)
    [[ -n $cg ]] || exit 2
    awk "\$1==\"frozen\"{print \$2}" "/sys/fs/cgroup$cg/cgroup.events" 2>/dev/null || exit 2
  '
}

# The turn runner: report whether the capture's file was restored, prove
# the workspace clone and the model env landed, then plant the turn for
# the next writer. `px run --continue-session` copies the last writer's
# spec wholesale, so the continued task runs this same command and its
# SESSION-RESTORED-OK line is a real restore, not a planted echo.
RUNNER_CMD='mkdir -p "$HOME/.claude/projects/-root-int"
if [ -s "$HOME/.claude/projects/-root-int/int.jsonl" ]; then
  echo "SESSION-RESTORED-OK"
else
  echo "NO-SESSION-FILE"
fi
test -d /workspace/'"$WS"'/.git && echo "WORKSPACE-OK"
[ -n "$ANTHROPIC_API_KEY" ] && echo "MODEL-ENV-OK"
echo "GOAL-SEEN:$GOAL"
printf "%s\n" "{\"type\":\"user\",\"message\":{\"content\":\"integration planted turn\"}}" > "$HOME/.claude/projects/-root-int/int.jsonl"
sleep 45'

cleanup() {
  "$PX" delete task "$MAIN" >/dev/null 2>&1 || true
  "$PX" delete task "$FENCED" >/dev/null 2>&1 || true
  "$PX" delete task "$NEXT" >/dev/null 2>&1 || true
  "$PX" delete schedule "$SCHED" >/dev/null 2>&1 || true
  for t in $("$PX" get tasks 2>/dev/null | awk -v p="$SCHED-" '$1 ~ ("^" p "[0-9]+$") {print $1}'); do
    "$PX" delete task "$t" >/dev/null 2>&1 || true
  done
  # explicit sessions outlive their writing task's deletion, so the
  # named capture must be deleted on its own
  "$PX" delete session "$CONV" >/dev/null 2>&1 || true
  "$PX" delete gateway "$GW" >/dev/null 2>&1 || true
  "$PX" delete workspace "$WS" >/dev/null 2>&1 || true
  "$PX" delete model "$MODEL" >/dev/null 2>&1 || true
}
trap cleanup EXIT

say "0. px-server is reachable and the runner template exists on the node"
if ! "$PX" get tasks >/dev/null 2>&1; then
  echo "FATAL: cannot reach px-server at $PX_SERVER (start it first, see docs/e2e.md)" >&2
  exit 1
fi
ok "GET /v1/tasks responded"
cleanup
if [[ -n $PVE_SSH ]]; then
  if list=$(ssh -o BatchMode=yes -o ConnectTimeout=5 "$PVE_SSH" "pct list" 2>&1); then
    if grep -q "$TPL" <<<"$list"; then
      ok "runner template $TPL found on the node"
    else
      echo "FATAL: $TPL is not on the node. Build it first (see template/runner/)." >&2
      exit 1
    fi
  else
    bad "ssh to $PVE_SSH failed, template check skipped: $list"
  fi
else
  echo "SKIP: template check (PVE_SSH is empty)"
fi

say "1. one multi-document apply creates every kind, cross-referenced"
# Every step below is written so one failure does not kill the script:
# later sections are diagnostic value even when an earlier one broke.
if ! apply <<EOF
apiVersion: px.io/v1alpha1
kind: Model
metadata:
  name: $MODEL
spec:
  provider: anthropic
  apiKey: sk-ant-e2e-integration-invalid
---
apiVersion: px.io/v1alpha1
kind: Gateway
metadata:
  name: $GW
spec:
  egress:
    - cidr: $ALLOW_DEST
      ports: "8006"
---
apiVersion: px.io/v1alpha1
kind: Workspace
metadata:
  name: $WS
spec:
  git:
    repo: $REPO
---
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: $MAIN
spec:
  image: $TPL
  goal: integrate the flux capacitor
  workspaces:
    - name: $WS
      goal: study the main branch
  model: $MODEL
  session:
    name: $CONV
  runner:
    command:
      - sh
      - -c
      - |
$(sed 's/^/        /' <<<"$RUNNER_CMD")
---
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: $FENCED
spec:
  image: $TPL
  gateway: $GW
  runner:
    command:
      - sh
      - -c
      - |
        for i in \$(seq 1 15); do
          awk '\$2=="00000000"{f=1} END{exit !f}' /proc/net/route && break
          sleep 1
        done
        pve=blocked ext=blocked dns=blocked
        curl -sk --connect-timeout 5 -o /dev/null https://$ALLOW_DEST:8006/ && pve=ok
        curl -sk --connect-timeout 5 -o /dev/null https://1.1.1.1/ && ext=ok
        getent hosts deb.debian.org >/dev/null 2>&1 && dns=ok
        echo "EGRESS_CHECK pve=\$pve ext=\$ext dns=\$dns"
---
apiVersion: px.io/v1alpha1
kind: Schedule
metadata:
  name: $SCHED
spec:
  schedule: "* * * * *"
  taskTemplate:
    image: $TPL
    runner:
      command: ["true"]
EOF
then
  bad "multi-document apply failed"
fi
if refs=$(curl -sf "$PX_SERVER/v1/tasks/$MAIN" ${PX_TOKEN:+-H "Authorization: Bearer $PX_TOKEN"} | python3 -c 'import json,sys; s=json.load(sys.stdin)["spec"]; print(s.get("model",""), s.get("session",{}).get("name",""), s["workspaces"][0]["name"])'); then
  if [[ $refs == "$MODEL $CONV $WS" ]]; then
    ok "task spec carries the model, session and workspace references ($refs)"
  else
    bad "task references wrong: '$refs', want '$MODEL $CONV $WS'"
  fi
else
  bad "could not read back $MAIN's spec"
fi

say "2. the main task runs, execs, and survives suspend/resume mid-flight"
if wait_phase "$MAIN" Running; then
  if got=$("$PX" exec "$MAIN" -- sh -c 'test -d /workspace/'"$WS"'/.git && echo WS-EXEC-OK'); then
    [[ $got == WS-EXEC-OK ]] && ok "exec sees the cloned workspace" || bad "exec output '$got', want WS-EXEC-OK"
  else
    bad "exec into $MAIN failed: $got"
  fi
  vmid=$(describe_json "$MAIN" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"].get("container",0))' || echo 0)
  if ! "$PX" suspend task "$MAIN" >/dev/null 2>&1; then
    bad "suspend $MAIN failed (task finished before the suspend landed?)"
  elif wait_phase "$MAIN" Suspended; then
    if [[ -n $PVE_SSH ]]; then
      f=$(node_frozen "$vmid" || echo err)
      [[ $f == 1 ]] && ok "cgroup actually frozen on the node" || bad "cgroup frozen state '$f' after suspend, want 1"
    fi
    code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$PX_SERVER/v1/tasks/$MAIN/exec" \
      ${PX_TOKEN:+-H "Authorization: Bearer $PX_TOKEN"} \
      -H 'Content-Type: application/json' -d '{"command":["true"]}')
    [[ $code == 409 ]] && ok "exec on Suspended -> 409" || bad "exec on Suspended -> $code, want 409"
    # a refused resume surfaces as the Running wait below timing out
    "$PX" resume task "$MAIN" >/dev/null 2>&1 || true
    if wait_phase "$MAIN" Running; then
      if got=$("$PX" exec "$MAIN" -- sh -c 'echo alive'); then
        [[ $got == alive ]] && ok "exec works after resume" || bad "exec after resume output '$got'"
      else
        bad "exec after resume failed: $got"
      fi
    fi
  fi
fi

say "3. the main task finishes and its capture lands under the session name"
if wait_phase "$MAIN" Succeeded; then
  logs=$("$PX" logs "$MAIN" 2>/dev/null || true)
  for marker in NO-SESSION-FILE WORKSPACE-OK MODEL-ENV-OK; do
    grep -q "$marker" <<<"$logs" && ok "logs show $marker" || bad "logs missing $marker"
  done
  if grep -q '^GOAL-SEEN:.' <<<"$logs"; then
    ok "logs show the goal reached the runner: $(grep '^GOAL-SEEN:' <<<"$logs")"
  else
    bad "logs missing GOAL-SEEN:<goal>"
  fi
  if row=$(sess_row "$CONV"); [[ -n $row ]]; then
    if grep -q "$MAIN" <<<"$row"; then
      ok "px get sessions lists $CONV with last writer $MAIN"
    else
      bad "session's last writer is not $MAIN (row: '$row')"
    fi
  else
    bad "px get sessions does not list $CONV"
  fi
  if d=$("$PX" describe session "$CONV" 2>&1) && [[ -n $d ]] && python3 -c 'import json,sys; json.JSONDecoder().raw_decode(sys.stdin.read())' <<<"$d" 2>/dev/null; then
    ok "describe session $CONV returns structured output"
  else
    bad "describe session failed: ${d:-<empty>}"
  fi
fi

say "4. px run --continue-session restores the capture into a fresh task"
if ! "$PX" run -continue-session "$CONV" -name "$NEXT" -no-wait "advance the conversation" >/dev/null; then
  bad "px run -continue-session failed"
fi
if wait_phase "$NEXT" Succeeded; then
  logs=$("$PX" logs "$NEXT" 2>/dev/null || true)
  if grep -q "SESSION-RESTORED-OK" <<<"$logs"; then
    ok "the continued task restored turn 1's session in a fresh container"
  else
    bad "no restore marker in $NEXT logs: $(echo "$logs" | tail -5)"
  fi
  row=$(sess_row "$CONV" || true)
  if [[ -n $row ]] && grep -q "$NEXT" <<<"$row"; then
    ok "last writer moved to $NEXT"
  else
    bad "capture's last writer did not move to $NEXT (row: '$row')"
  fi
fi

say "5. the fenced task proves the gateway allowlist"
if wait_phase "$FENCED" Succeeded; then
  logs=$("$PX" logs "$FENCED" 2>/dev/null || true)
  line=$(grep -o 'EGRESS_CHECK.*' <<<"$logs" | tail -1) || true
  if [[ $line == "EGRESS_CHECK pve=ok ext=blocked dns=ok" ]]; then
    ok "egress verdict as declared: $line"
  else
    bad "egress verdict wrong: ${line:-<no EGRESS_CHECK line>} (logs: $(echo "$logs" | tail -5))"
  fi
fi

say "6. the schedule fires at the minute boundary; suspend is declarative"
if wait_sched_count 1 90; then
  first=$("$PX" get tasks 2>/dev/null | awk -v p="$SCHED-" '$1 ~ ("^" p "[0-9]+$") {print $1}' | sort | sed -n 1p || true)
  if owner=$("$PX" describe task "$first" | python3 -c 'import json,sys; print(json.JSONDecoder().raw_decode(sys.stdin.read())[0]["status"].get("scheduleOwner",""))') && [[ $owner == "$SCHED" ]]; then
    ok "fired task $first carries scheduleOwner=$SCHED"
  else
    bad "fired task's scheduleOwner wrong: '$owner'"
  fi
  if wait_phase "$first" Succeeded; then
    ok "fired task runs the template command to completion"
  fi
  "$PX" suspend schedule "$SCHED" >/dev/null 2>&1 || true
  if sus=$("$PX" describe schedule "$SCHED" | python3 -c 'import json,sys; print(json.JSONDecoder().raw_decode(sys.stdin.read())[0]["spec"].get("suspend", False))') && [[ $sus == True ]]; then
    ok "suspend schedule is declarative: spec.suspend=$sus"
  else
    bad "schedule suspend flag wrong: '$sus', want True"
  fi
fi

say "7. metrics agree with what just happened"
if (( $(succ_metric) >= 3 )); then
  ok "px_tasks{phase=\"Succeeded\"} = $(succ_metric) (main, next, fenced, fired)"
else
  bad "px_tasks Succeeded = $(succ_metric), want >= 3"
fi
qw=$(quota_waiting || true)
[[ $qw == 0 ]] && ok "px_quota_waiting is 0" || bad "px_quota_waiting = $qw, want 0"

say "8. cleanup: everything deletes, nothing is left behind"
"$PX" delete task "$MAIN" >/dev/null 2>&1 || true
"$PX" delete task "$FENCED" >/dev/null 2>&1 || true
"$PX" delete task "$NEXT" >/dev/null 2>&1 || true
wait_gone "$MAIN" || true
wait_gone "$FENCED" || true
wait_gone "$NEXT" || true
"$PX" delete schedule "$SCHED" >/dev/null 2>&1 || true
for t in $("$PX" get tasks 2>/dev/null | awk -v p="$SCHED-" '$1 ~ ("^" p "[0-9]+$") {print $1}'); do
  "$PX" delete task "$t" >/dev/null 2>&1 || true
done
"$PX" delete session "$CONV" >/dev/null 2>&1 || true
if [[ -z $(sess_row "$CONV") ]]; then
  ok "session capture $CONV deleted"
else
  bad "session capture $CONV survived delete"
fi
"$PX" delete gateway "$GW" >/dev/null 2>&1 || true
"$PX" delete workspace "$WS" >/dev/null 2>&1 || true
"$PX" delete model "$MODEL" >/dev/null 2>&1 || true
if [[ -n $PVE_SSH ]]; then
  sleep 3
  if list=$(ssh -o BatchMode=yes -o ConnectTimeout=5 "$PVE_SSH" "pct list" 2>&1); then
    # task containers are the only px- things this test creates; the
    # node's own templates (px-runner-*, px-agent-*) are not leftovers
    if leftover=$(grep 'px-' <<<"$list" | grep -vE '[[:space:]](px-runner-|px-agent-)'); then
      bad "leftover containers: $(echo "$leftover" | tr '\n' ' ')"
    else
      ok "no px- containers left on the node"
    fi
  else
    bad "final pct list failed: $list"
  fi
fi

say "results"
echo "PASS: $pass  FAIL: $fail"
(( fail == 0 ))
