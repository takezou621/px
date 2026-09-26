#!/usr/bin/env bash
# E2E for named sessions (M11) against a real px-server + PVE node
# (see docs/e2e.md). Same fake-session trick as e2e-session.sh: the runner
# plants/reads a JSONL file under ~/.claude/projects, so no API credential
# is involved — the capture/restore transport moves that directory around
# regardless of what the conversation contains.
#
# Requires:
#   - px-server running with a working PVE token and node SSH access
#   - the agent template on the node (px-agent-debian12; build it with
#     template/agent/build.sh) — checked up front, with instructions if absent
#
# Env:
#   PX_SERVER  px-server URL            (default http://127.0.0.1:7420)
#   PX         px CLI binary path       (default ./px)
#   TIMEOUT    seconds to wait per task (default 300)
#   PVE_SSH    ssh target of the PVE node, for the template check
#              (default root@192.168.2.100; empty disables it)
#
# Checks:
#   1. a task with spec.session.name writes its capture under the explicit
#      name (the turn-1 runner finds no session file, plants one)
#   2. `px run --continue-session` restores the capture into a fresh task
#      (its log says SESSION-RESTORED-OK), and after it finishes the
#      capture's LAST-TASK has moved to the new writer
#   3. the first writer's record is deleted — the named capture SURVIVES
#      (px get sessions still lists it, LAST-TASK still the second writer)
#   4. `px describe session` lists the referencing task
#   5. `px delete session` drops the capture; a session:NAME continuation
#      of the deleted capture then fails at provision
# The first task is applied as YAML because spec.session.name has no run
# flag by design (named captures are a manifest feature; the CLI sugar
# covers the continuation path).
set -euo pipefail

export PX_SERVER="${PX_SERVER:-http://127.0.0.1:7420}"
PX="${PX:-./px}"
TIMEOUT="${TIMEOUT:-300}"
PVE_SSH="${PVE_SSH-root@192.168.2.100}"
TPL=px-agent-debian12
CONV=e2e-sessions-conv
W1=e2e-sessions-w1
W2=e2e-sessions-w2
GONE=e2e-sessions-gone

pass=0
fail=0
say() { printf '\n== %s\n' "$*"; }
ok()  { pass=$((pass+1)); echo "PASS: $*"; }
bad() { fail=$((fail+1)); echo "FAIL: $*"; }

phase() { # prints the task's phase; fails when the CLI itself does
  local out
  if ! out=$("$PX" get tasks 2>/dev/null); then
    return 1
  fi
  awk -v n="$1" '$1==n {print $2}' <<<"$out"
}
wait_phase() { # name want
  local name=$1 want=$2 waited=0 p
  while (( waited < TIMEOUT )); do
    if p=$(phase "$name") && [[ $p == "$want" ]]; then
      ok "$name -> $want (${waited}s)"
      return 0
    fi
    sleep 2
    waited=$((waited+2))
  done
  bad "$name did not reach $want within ${TIMEOUT}s (last phase: '$(phase "$name" || true)')"
  return 1
}
wait_record_gone() { # name
  local name=$1 waited=0 p
  while (( waited < TIMEOUT )); do
    # A CLI failure is not "the record is gone": retry, don't conclude.
    if p=$(phase "$name") && [[ -z $p ]]; then
      ok "$name record gone (${waited}s)"
      return 0
    fi
    sleep 1
    waited=$((waited+1))
  done
  bad "$name record did not disappear within ${TIMEOUT}s (last phase: '$(phase "$name" || true)')"
  return 1
}
# sess_row prints a session row from `px get sessions` (or empty).
sess_row() { "$PX" get sessions 2>/dev/null | awk -v n="$1" '$1==n'; }

# The turn runner: report whether the capture's file was restored, then
# plant the turn for the next writer. Every task in the conversation runs
# this same command, so `px run --continue-session` (which copies the last
# writer's spec) exercises a real restore on every turn after the first.
RUNNER_CMD='mkdir -p "$HOME/.claude/projects/-root-e2e"
if [ -s "$HOME/.claude/projects/-root-e2e/e2e.jsonl" ]; then
  echo "SESSION-RESTORED-OK"
else
  echo "NO-SESSION-FILE"
fi
printf "%s\n" "{\"type\":\"user\",\"message\":{\"content\":\"e2e-sessions planted turn\"}}" > "$HOME/.claude/projects/-root-e2e/e2e.jsonl"
echo PLANTED'

apply_task() { # name extra-yaml...
  { printf 'apiVersion: px.io/v1alpha1\nkind: Task\nmetadata:\n  name: %s\nspec:\n  image: %s\n  goal: advance the conversation\n  runner:\n    command:\n      - sh\n      - -c\n      - |\n' "$1" "$TPL"
    sed 's/^/        /' <<<"$RUNNER_CMD"
    shift
    printf '%s\n' "$@"
  } | "$PX" apply -f - >/dev/null
}

cleanup() {
  for t in "$W1" "$W2" "$GONE"; do
    "$PX" delete task "$t" >/dev/null 2>&1 || true
  done
  # Named captures outlive tasks by design — drop ours explicitly.
  "$PX" delete session "$CONV" >/dev/null 2>&1 || true
  # Delete is async (the controller reconciles on its next tick), so wait
  # briefly for the records and the capture to actually go: leftovers from
  # a previous run would corrupt the next one's assertions.
  local t waited row
  for t in "$W1" "$W2" "$GONE"; do
    waited=0
    while (( waited < 30 )) && { row=$(phase "$t" || true); [[ -n $row ]]; }; do
      sleep 1
      waited=$((waited+1))
    done
  done
  waited=0
  while (( waited < 30 )) && { row=$(sess_row "$CONV" || true); [[ -n $row ]]; }; do
    sleep 1
    waited=$((waited+1))
  done
}
trap cleanup EXIT

say "0. px-server is reachable and the agent template exists on the node"
if ! "$PX" get tasks >/dev/null 2>&1; then
  echo "FATAL: cannot reach px-server at $PX_SERVER (start it first, see docs/e2e.md)" >&2
  exit 1
fi
ok "GET /v1/tasks responded"
cleanup
if [[ -n $PVE_SSH ]]; then
  if list=$(ssh -o BatchMode=yes -o ConnectTimeout=5 "$PVE_SSH" "pct list" 2>&1); then
    if grep -q "$TPL" <<<"$list"; then
      ok "agent template $TPL found on the node"
    else
      echo "FATAL: $TPL is not on the node. Build it first (use a free CTID, e.g. 998):" >&2
      echo "  scp template/agent/build.sh $PVE_SSH:/root/px-agent-build.sh" >&2
      echo "  ssh $PVE_SSH /root/px-agent-build.sh CTID   # CTID = a free container id" >&2
      exit 1
    fi
  else
    bad "ssh to $PVE_SSH failed, template check skipped: $list"
  fi
else
  echo "SKIP: template check (PVE_SSH is empty)"
fi

say "1. the turn-1 writer plants the conversation under its explicit name"
apply_task "$W1" "  session:
    name: $CONV"
if wait_phase "$W1" Succeeded; then
  if grep -q "NO-SESSION-FILE" <("$PX" logs "$W1"); then
    ok "turn 1 started with a fresh HOME (no session file)"
  else
    bad "turn 1 unexpectedly restored a session: $("$PX" logs "$W1")"
  fi
  if [[ -n $(sess_row "$CONV") ]]; then
    ok "px get sessions lists $CONV ($(sess_row "$CONV" | tr -s ' ' | cut -d' ' -f2-) bytes)"
  else
    bad "px get sessions does not list $CONV"
  fi
fi

say "2. px run --continue-session restores the capture and moves LAST-TASK"
"$PX" run -continue-session "$CONV" -name "$W2" -no-wait "advance the conversation" >/dev/null
if wait_phase "$W2" Succeeded; then
  if grep -q "SESSION-RESTORED-OK" <("$PX" logs "$W2"); then
    ok "turn 2 restored turn 1's session into a fresh container"
  else
    bad "turn 2 did not see the restored session: $("$PX" logs "$W2")"
  fi
  row=$(sess_row "$CONV" || true)
  if [[ -n $row ]] && grep -q "$W2" <<<"$row"; then
    ok "last writer moved to $W2: $row"
  else
    bad "capture's LAST-TASK did not move to $W2 (row: '$row')"
  fi
fi

say "3. the first writer's record is deleted; the named capture survives"
"$PX" delete task "$W1" >/dev/null
if wait_record_gone "$W1"; then
  row=$(sess_row "$CONV" || true)
  if [[ -n $row ]] && grep -q "$W2" <<<"$row"; then
    ok "capture outlives its first writer: $row"
  else
    bad "capture did not survive the writer's deletion (row: '$row')"
  fi
fi

say "4. px describe session lists the referencing task"
desc=$("$PX" describe session "$CONV" || true)
if grep -q "^Referenced by:" <<<"$desc" && grep -q "$W2" <<<"$desc"; then
  ok "describe session names $W2 as a referencer"
else
  bad "describe session missing the referencer section: $desc"
fi

say "5. deleting the session breaks session:NAME continuations loudly"
"$PX" delete session "$CONV" >/dev/null
waited=0
while (( waited < TIMEOUT )); do
  [[ -z $(sess_row "$CONV" || true) ]] && break
  sleep 1
  waited=$((waited+1))
done
if [[ -z $(sess_row "$CONV" || true) ]]; then
  ok "px delete session dropped the capture (${waited}s)"
else
  bad "capture still listed after px delete session"
fi
apply_task "$GONE" "  session:
    continueFrom: session:$CONV"
if wait_phase "$GONE" ProvisionFailed; then
  if grep -q "$CONV" <("$PX" events "$GONE"); then
    ok "continuation of a deleted session failed at provision, naming it"
  else
    bad "ProvisionFailed event does not name the session: $("$PX" events "$GONE")"
  fi
fi

say "6. cleanup"
for t in "$W2" "$GONE"; do
  "$PX" delete task "$t" >/dev/null 2>&1 || true
done

printf '\n== results: %d passed, %d failed\n' "$pass" "$fail"
(( fail == 0 )) || exit 1
echo "E2E sessions test PASSED"
