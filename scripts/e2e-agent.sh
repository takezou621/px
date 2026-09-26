#!/usr/bin/env bash
# E2E for the first-class agent runtime (px run + the agent template) against
# a real px-server + PVE node (see docs/e2e.md).
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
#   PVE_SSH    ssh target of the PVE node, for the template and
#              container-cleanup checks (default root@192.168.2.100;
#              empty disables them)
#
# Checks, none of which need a working API credential:
#   1. px run applies and follows a task to Succeeded with an explicit
#      `--` command
#   1b. without `--`, px run puts the goal in spec.goal and the default
#      agent command in spec.runner.command (checked on the applied spec)
#   2. the Claude Code CLI is present in the template (claude --version)
#   3. the task-level goal reaches the runner as GOAL
#   4. a Model with a non-working key fails the task cleanly, with the
#      provider error visible in the task log — this sends ONE harmless
#      request to the real Anthropic endpoint, which rejects the dummy key
#      with a 401; no valid credential exists anywhere in this suite
# A live agent run with a real key is deliberately not automated here: it
# burns tokens and needs supervision (docs/roadmap.md, M6).
set -euo pipefail

export PX_SERVER="${PX_SERVER:-http://127.0.0.1:7420}"
PX="${PX:-./px}"
TIMEOUT="${TIMEOUT:-300}"
PVE_SSH="${PVE_SSH-root@192.168.2.100}"
MODEL_KEY=sk-ant-e2e-agent-invalid
MODEL=e2e-agent-model
TPL=px-agent-debian12

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
wait_gone() { # name
  local name=$1 waited=0 listing
  while (( waited < 120 )); do
    # A failed read is not proof of deletion — only an absent row is.
    if ! listing=$("$PX" get tasks 2>/dev/null); then
      bad "get tasks failed while waiting for $name"
      return 1
    fi
    if ! grep -q "^$name " <<<"$listing"; then
      ok "$name removed from the store"
      return 0
    fi
    sleep 2
    waited=$((waited+2))
  done
  bad "$name still present after delete"
  return 1
}
cleanup() {
  "$PX" delete task e2e-agent-cli    >/dev/null 2>&1 || true
  "$PX" delete task e2e-agent-default >/dev/null 2>&1 || true
  "$PX" delete task e2e-agent-goal >/dev/null 2>&1 || true
  "$PX" delete task e2e-agent-fail >/dev/null 2>&1 || true
  "$PX" delete model "$MODEL"      >/dev/null 2>&1 || true
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

say "1. px run applies and follows a task to Succeeded (explicit command)"
# `claude --version` proves the CLI runs without any credential. followRun
# is exercised too: the command must return on its own once the task goes
# terminal. The default-command path (no --) is checked in 1b.
if out=$("$PX" run -name e2e-agent-cli "sanity check" -- claude --version 2>&1); then
  if [[ $out == *"task.px.io/e2e-agent-cli applied"* && $out == *Succeeded* ]]; then
    ok "px run applied the task and followed it to Succeeded"
  else
    bad "px run output missing applied/Succeeded: $out"
  fi
else
  bad "px run failed: $out"
fi

say "1b. without --, px run puts the goal in spec.goal and the default agent command in spec.runner.command"
# Regression tripwire: an earlier bug let the whole argv (flags + goal)
# become the runner command when no -- was given.
"$PX" run -name e2e-agent-default -no-wait "sanity check"
desc=$("$PX" describe task e2e-agent-default)
if grep -q 'claude --dangerously-skip-permissions' <<<"$desc" \
    && grep -q '"goal": "sanity check"' <<<"$desc"; then
  ok "default agent command selected, goal carried in spec.goal"
else
  bad "applied spec wrong (default command and/or goal missing): $desc"
fi
"$PX" delete task e2e-agent-default >/dev/null

say "2. the Claude Code CLI is present and runnable in the template"
if logs=$("$PX" logs e2e-agent-cli); then
  if grep -qi "claude code" <<<"$logs"; then
    ok "claude --version ran inside the container"
  else
    bad "claude --version output missing: $logs"
  fi
else
  bad "logs for e2e-agent-cli unavailable"
fi

say "3. the task-level goal reaches the runner as GOAL"
"$PX" run -name e2e-agent-goal -no-wait "Ship the M6 fix" \
  -- sh -c 'echo "GOAL=[$GOAL]"'
if wait_phase e2e-agent-goal Succeeded; then
  logs=$("$PX" logs e2e-agent-goal)
  if [[ $logs == *"GOAL=[Ship the M6 fix]"* ]]; then
    ok "GOAL carries the task-level goal"
  else
    bad "task goal missing from GOAL: $logs"
  fi
fi

say "4. a Model with a non-working key fails cleanly, provider error in the log"
printf 'apiVersion: px.io/v1alpha1\nkind: Model\nmetadata:\n  name: %s\nspec:\n  provider: anthropic\n  apiKey: %s\n' "$MODEL" "$MODEL_KEY" | "$PX" apply -f - >/dev/null
"$PX" run -name e2e-agent-fail -no-wait -model "$MODEL" "write a haiku about sandboxes"
if wait_phase e2e-agent-fail Failed; then
  logs=$("$PX" logs e2e-agent-fail)
  if grep -qiE 'api error|401|authentic' <<<"$logs"; then
    ok "provider error surfaced in the task log"
  else
    bad "no provider error in the log (task failed silently?): $logs"
  fi
fi

say "5. cleanup: every task and the model are removed, nothing left on the node"
# || true: a task that never got applied (an earlier section failed hard)
# deletes 404 — that must not abort the run before the summary prints.
for t in e2e-agent-cli e2e-agent-default e2e-agent-goal e2e-agent-fail; do
  "$PX" delete task "$t" >/dev/null 2>&1 || true
  wait_gone "$t" || true
done
"$PX" delete model "$MODEL" >/dev/null
code=$(curl -s -o /dev/null -w '%{http_code}' "$PX_SERVER/v1/models/$MODEL")
if [[ $code == 404 ]]; then
  ok "model deleted (GET returns 404)"
else
  bad "model still present after delete (GET returned $code)"
fi
if [[ -n $PVE_SSH ]]; then
  if list=$(ssh -o BatchMode=yes -o ConnectTimeout=5 "$PVE_SSH" "pct list" 2>&1); then
    if grep -q "px-e2e-agent-" <<<"$list"; then
      bad "agent containers left on the node: $(grep 'px-e2e-agent-' <<<"$list")"
    else
      ok "no px-e2e-agent-* containers left on the node"
    fi
  else
    bad "ssh to $PVE_SSH failed, leftover check skipped: $list"
  fi
else
  echo "SKIP: leftover-container check (PVE_SSH is empty)"
fi

say "6. not covered here: a live agent run"
echo "SKIP: a real agent run needs a working API key and supervision, e.g.:"
echo "      px run -model <model> -workspace <name> \"<goal>\""

printf '\n== results: %d passed, %d failed\n' "$pass" "$fail"
(( fail == 0 )) || exit 1
echo "E2E agent test PASSED"
