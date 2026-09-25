#!/usr/bin/env bash
# E2E smoke test for px against a real px-server + PVE node.
#
# Requires (see docs/e2e.md for setup):
#   - px-server running with a working PVE token and node SSH access
#   - the runner template built on the node (px-runner-debian12)
#
# Env:
#   PX_SERVER  px-server URL            (default http://127.0.0.1:7420)
#   PX         px CLI binary path       (default ./px)
#   TIMEOUT    seconds to wait per task (default 300)
#
# Checks: success path, failure path, live delete, logs, duplicate rejection.
set -euo pipefail

# The CLI reads PX_SERVER; the -server flag must trail the subcommand, so the
# env var is the clean way to point every invocation at the same server.
export PX_SERVER="${PX_SERVER:-http://127.0.0.1:7420}"
PX="${PX:-./px}"
TIMEOUT="${TIMEOUT:-300}"

pass=0
fail=0
say() { printf '\n== %s\n' "$*"; }
ok()  { pass=$((pass+1)); echo "PASS: $*"; }
bad() { fail=$((fail+1)); echo "FAIL: $*"; }

phase() { "$PX" get tasks 2>/dev/null | awk -v n="$1" '$1==n {print $2}'; }
exists() {
  "$PX" get tasks 2>/dev/null | awk -v n="$1" '$1==n {found=1} END{exit !found}'
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
wait_gone() { # name
  local name=$1 waited=0
  while (( waited < 120 )); do
    if ! exists "$name"; then
      ok "$name removed from the store"
      return 0
    fi
    sleep 2
    waited=$((waited+2))
  done
  bad "$name still present after delete"
  return 1
}
apply() { "$PX" apply -f - >/dev/null; }

cleanup() { # best-effort: don't leave smoke-test containers behind on timeout
  for t in e2e-ok e2e-fail e2e-goal e2e-long e2e-dup; do
    "$PX" delete task "$t" >/dev/null 2>&1 || true
  done
  # workspace delete is not implemented (roadmap backlog); e2e-ws stays in
  # the store — upsert keeps re-runs idempotent.
}
trap cleanup EXIT

say "0. px-server is reachable"
if "$PX" get tasks >/dev/null 2>&1; then
  ok "GET /v1/tasks responded"
else
  echo "FATAL: cannot reach px-server at $PX_SERVER (start it first, see docs/e2e.md)" >&2
  exit 1
fi

say "1. successful task reaches Succeeded and logs carry its output"
apply <<'EOF'
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: e2e-ok
spec:
  image: px-runner-debian12
  runner:
    command: ["echo", "hello-from-px"]
EOF
if wait_phase e2e-ok Succeeded; then
  logs=$("$PX" logs e2e-ok)
  if [[ $logs == *hello-from-px* ]]; then
    ok "logs contain runner output"
  else
    bad "logs missing runner output, got: $logs"
  fi
fi

say "2. failing task reaches Failed with a non-zero exit"
apply <<'EOF'
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: e2e-fail
spec:
  image: px-runner-debian12
  runner:
    command: ["sh", "-c", "echo about-to-fail >&2; exit 7"]
EOF
if wait_phase e2e-fail Failed; then
  code=$("$PX" describe task e2e-fail | grep '"exitCode"' | tr -dc '0-9')
  if [[ $code == 7 ]]; then
    ok "exit code 7 recorded"
  else
    bad "exit code not 7: '$code'"
  fi
  logs=$("$PX" logs e2e-fail)
  [[ $logs == *about-to-fail* ]] && ok "stderr captured in logs" || bad "stderr missing from logs"
fi

say "3. workspace goal text reaches the container"
# task.spec.workspaces[].name references a Workspace resource, which the
# controller clones into /workspace/<name> before the runner starts.
apply <<'EOF'
apiVersion: px.io/v1alpha1
kind: Workspace
metadata:
  name: e2e-ws
spec:
  git:
    repo: https://github.com/takezou621/px.git
EOF
apply <<'EOF'
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: e2e-goal
spec:
  image: px-runner-debian12
  workspaces:
    - name: e2e-ws
      goal: Fix the flux capacitor
  runner:
    command: ["sh", "-c", "cat /run/px/goal; test -d /workspace/e2e-ws/.git"]
EOF
if wait_phase e2e-goal Succeeded; then
  logs=$("$PX" logs e2e-goal)
  [[ $logs == *"Fix the flux capacitor"* ]] && ok "GOAL contains the workspace goal" || bad "goal missing: $logs"
fi

say "4. delete removes a running task and its container"
apply <<'EOF'
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: e2e-long
spec:
  image: px-runner-debian12
  runner:
    command: ["sleep", "600"]
EOF
wait_phase e2e-long Running
"$PX" delete task e2e-long >/dev/null
wait_gone e2e-long

say "5. duplicate apply is rejected"
out=$(apply <<'EOF' 2>&1 || true
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: e2e-dup
spec:
  image: px-runner-debian12
  runner:
    command: ["true"]
EOF
)
if "$PX" apply -f - >/dev/null 2>&1 <<'EOF'
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: e2e-dup
spec:
  image: px-runner-debian12
  runner:
    command: ["true"]
EOF
then
  bad "second apply of e2e-dup was accepted"
else
  ok "second apply rejected (first apply: ${out:-ok})"
fi
"$PX" delete task e2e-dup >/dev/null || true

say "6. cleanup of smoke-test tasks"
for t in e2e-ok e2e-fail e2e-goal; do
  "$PX" delete task "$t" >/dev/null 2>&1 || true
  wait_gone "$t" || true
done

printf '\n== results: %d passed, %d failed\n' "$pass" "$fail"
(( fail == 0 )) || exit 1
echo "E2E smoke test PASSED"
