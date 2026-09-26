#!/usr/bin/env bash
# E2E for task events + metrics (M10) against a real px-server + PVE node
# (see docs/e2e.md).
#
# Requires:
#   - px-server running with a working PVE token and node SSH access
#   - the agent template on the node (px-agent-debian12)
#
# Env:
#   PX_SERVER  px-server URL            (default http://127.0.0.1:7420)
#   PX         px CLI binary path       (default ./px)
#   TIMEOUT    seconds to wait per check (default 300)
#
# Checks:
#   1. a succeeding task records the full lifecycle in order
#      (Provisioning -> Scheduled -> Running -> Succeeded -> SessionSaved)
#      and `px describe task` appends the Events section
#   2. a task naming an unknown image records ProvisionFailed
#   3. suspend + resume record the request and the settle, in order
#   4. deleting a task drops its history (events read 404 afterwards)
#   5. /v1/metrics renders the Prometheus text shape
# Everything runs through the px API; no node SSH is needed and no
# hand-written VM or firewall config is touched.
set -euo pipefail

export PX_SERVER="${PX_SERVER:-http://127.0.0.1:7420}"
PX="${PX:-./px}"
TIMEOUT="${TIMEOUT:-300}"

pass=0
fail=0
say() { printf '\n== %s\n' "$*"; }
ok()  { pass=$((pass+1)); echo "PASS: $*"; }
bad() { fail=$((fail+1)); echo "FAIL: $*"; }

phase() {
  # An erroring CLI must not read as "task gone" (check 4 depends on absence).
  if ! out=$("$PX" get tasks 2>/dev/null); then
    echo "CLI_ERROR"
    return
  fi
  awk -v n="$1" '$1==n {print $2}' <<<"$out"
}
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

# reasons prints one task's event reasons, oldest first (px events' order).
# The table's TIME column contains a space, so REASON is field 4.
reasons() { "$PX" events "$1" 2>/dev/null | awk 'NR>1 {print $4}'; }
wait_reason() { # name reason — settle events may lag their phase by a tick
  local name=$1 want=$2 waited=0
  while (( waited < TIMEOUT )); do
    if grep -qx "$want" <(reasons "$name"); then
      ok "$name recorded $want (${waited}s)"
      return 0
    fi
    sleep 2
    waited=$((waited+2))
  done
  bad "$name never recorded $want (has: $(reasons "$name" | tr '\n' ' '))"
  return 1
}
order_ok() { # name r1 r2... — reasons appear in this order in the history
  local name=$1; shift
  reasons "$name" | awk -v want="$*" '
    { got[NR]=$1 }
    END {
      n = split(want, w, " ")
      j = 1
      for (i = 1; i <= NR && j <= n; i++)
        if (got[i] == w[j]) j++
      exit (j > n) ? 0 : 1
    }'
}

CLEANUP_TASKS=""
cleanup() {
  for t in $CLEANUP_TASKS; do
    "$PX" delete task "$t" >/dev/null 2>&1 || true
  done
}
# INT/TERM/HUP too: bash skips the EXIT trap when it dies from an
# unhandled signal (a dropped CI runner), and the leaked workload here is
# a real container sleeping on the node.
trap cleanup EXIT INT TERM HUP

say "0. px-server is reachable"
if ! "$PX" get tasks >/dev/null 2>&1; then
  echo "FATAL: cannot reach px-server at $PX_SERVER (start it first, see docs/e2e.md)" >&2
  exit 1
fi
ok "GET /v1/tasks responded"

say "1. a succeeding task records the full lifecycle in order"
"$PX" run -name e2e-ev-ok -no-wait "say hello and finish" -- sh -c 'echo hello' >/dev/null
CLEANUP_TASKS="e2e-ev-ok $CLEANUP_TASKS"
if wait_phase e2e-ev-ok Succeeded; then
  wait_reason e2e-ev-ok SessionSaved || true
  if order_ok e2e-ev-ok Provisioning Scheduled Running Succeeded SessionSaved; then
    ok "lifecycle in order: $(reasons e2e-ev-ok | tr '\n' ' ')"
  else
    bad "lifecycle out of order: $(reasons e2e-ev-ok | tr '\n' ' ')"
  fi
  # Capture, don't pipe: under pipefail a piped px can die of SIGPIPE
  # when `grep -q` exits early on its match.
  desc=$("$PX" describe task e2e-ev-ok)
  if grep -q '^Events:' <<<"$desc"; then
    ok "describe task appends the Events section"
  else
    bad "describe task has no Events section"
  fi
fi

say "2. a provision failure is recorded"
"$PX" run -image no-such-tmpl-e2e -name e2e-ev-gone -no-wait "must not run" >/dev/null
CLEANUP_TASKS="e2e-ev-gone $CLEANUP_TASKS"
if wait_phase e2e-ev-gone ProvisionFailed; then
  wait_reason e2e-ev-gone ProvisionFailed \
    && grep -q "no-such-tmpl-e2e" <("$PX" events e2e-ev-gone 2>/dev/null) \
    && ok "ProvisionFailed names the missing template" \
    || bad "ProvisionFailed event missing or unnamed"
fi

say "3. suspend and resume record request and settle, in order"
"$PX" run -name e2e-ev-susp -no-wait "stay alive for a while" -- sh -c 'sleep 600' >/dev/null
CLEANUP_TASKS="e2e-ev-susp $CLEANUP_TASKS"
if wait_phase e2e-ev-susp Running; then
  "$PX" suspend task e2e-ev-susp >/dev/null
  if wait_phase e2e-ev-susp Suspended; then
    "$PX" resume task e2e-ev-susp >/dev/null
    if wait_phase e2e-ev-susp Running; then
      wait_reason e2e-ev-susp Resumed || true
      if order_ok e2e-ev-susp SuspendRequested Suspended ResumeRequested Resumed; then
        ok "suspend/resume events in order"
      else
        bad "suspend/resume events out of order: $(reasons e2e-ev-susp | tr '\n' ' ')"
      fi
    fi
  fi
fi

say "4. deleting a task drops its history"
"$PX" delete task e2e-ev-ok >/dev/null
waited=0
while (( waited < TIMEOUT )); do
  [[ -z $(phase e2e-ev-ok) ]] && break
  sleep 2
  waited=$((waited+2))
done
if [[ -z $(phase e2e-ev-ok) ]]; then
  ok "task record gone (${waited}s)"
  # `px events` exits 1 on the expected 404; capture with || true so the
  # exit code can't poison the check (pipefail would surface it).
  evout=$("$PX" events e2e-ev-ok 2>&1 || true)
  if grep -q "404" <<<"$evout"; then
    ok "events die with the task record (404 after delete)"
  else
    bad "events still readable after delete"
  fi
else
  bad "task record never disappeared"
fi

say "5. /v1/metrics renders the Prometheus text"
m=$("$PX" metrics)
missing=0
for line in 'px_tasks{phase=' 'px_reconcile_tick_seconds_sum' 'px_reconcile_tick_seconds_count' 'px_provision_failures_total' 'px_events ' 'px_sessions_bytes ' 'px_store_bytes '; do
  if ! grep -q "$line" <<<"$m"; then
    bad "metrics missing $line"
    missing=1
  fi
done
(( missing == 0 )) && ok "all expected metric families present"
if (( $(awk '$1 == "px_provision_failures_total" {print $2}' <<<"$m") >= 1 )); then
  ok "provision failure counted"
else
  bad "px_provision_failures_total did not count the failed probe: $(grep px_provision_failures_total <<<"$m")"
fi
if (( $(awk '$1 == "px_reconcile_tick_seconds_count" {print $2}' <<<"$m") >= 1 )); then
  ok "reconcile ticks counted"
else
  bad "px_reconcile_tick_seconds_count is 0: $(grep px_reconcile_tick_seconds_count <<<"$m")"
fi

printf '\n== results: %d passed, %d failed\n' "$pass" "$fail"
(( fail == 0 )) || exit 1
echo "E2E events test PASSED"
