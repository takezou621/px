#!/usr/bin/env bash
# E2E: px exec — one-shot command in a running task's container.
#
# Requires (see docs/e2e.md for setup):
#   - px-server running with a working PVE token and node SSH access
#   - the runner template built on the node (px-runner-debian12)
#
# Env:
#   PX_SERVER  px-server URL            (default http://127.0.0.1:7420)
#   PX         px CLI binary path       (default ./px)
#   TIMEOUT    seconds to wait per task (default 300)
#   PVE_SSH    ssh target for node-level leftover checks
#              (default root@<node>; empty disables the checks)
#
# Checks: stream separation, exit-code propagation, byte-exact quoting,
# empty argv, output truncation, 404/409 gating, and the request
# validation added with the ownership re-check (NUL, trailing garbage),
# then cleanup.
set -euo pipefail

export PX_SERVER="${PX_SERVER:-http://127.0.0.1:7420}"
PX="${PX:-./px}"
TIMEOUT="${TIMEOUT:-300}"
# pve-node is not plumbed through the CLI; the ssh target carries the host.
# Colonless default: an explicitly empty PVE_SSH disables the node checks.
PVE_SSH="${PVE_SSH-root@192.168.2.100}"

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
    # A failed read is not proof of deletion — only an absent row is.
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

cleanup() { # best-effort: don't leave the exec-test container behind
  "$PX" delete task e2e-exec >/dev/null 2>&1 || true
  "$PX" delete task e2e-exec-gone >/dev/null 2>&1 || true
}
trap cleanup EXIT

say "0. px-server is reachable"
if "$PX" get tasks >/dev/null 2>&1; then
  ok "GET /v1/tasks responded"
else
  echo "FATAL: cannot reach px-server at $PX_SERVER (start it first, see docs/e2e.md)" >&2
  exit 1
fi

say "1. exec in a running task: streams are separated"
apply <<'EOF'
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: e2e-exec
spec:
  image: px-runner-debian12
  runner:
    command: ["sleep", "900"]
EOF
wait_phase e2e-exec Running
out=$(mktemp) err=$(mktemp)
if "$PX" exec e2e-exec -- sh -c 'echo to-out; echo to-err >&2' >"$out" 2>"$err"; then
  [[ $(cat "$out") == to-out ]] && ok "stdout carried stdout only" || bad "stdout wrong: $(cat "$out")"
  [[ $(cat "$err") == to-err ]] && ok "stderr carried stderr only" || bad "stderr wrong: $(cat "$err")"
else
  bad "exec itself failed"
fi

say "2. exit code propagates to the CLI, non-zero is not a client error"
set +e
"$PX" exec e2e-exec -- sh -c 'exit 3' >/dev/null 2>&1
code=$?
set -e
[[ $code == 3 ]] && ok "exit 3 propagated" || bad "exit code was $code, want 3"

say "3. arguments arrive byte-exact through both shells"
got=$("$PX" exec e2e-exec -- printf '%s\n' '$HOME' '*' 'a b' "it's")
[[ $got == '$HOME
*
a b
it'"'"'s' ]] && ok "metacharacters, spaces and quotes untouched" || bad "quoting mangled: '$got'"

say "4. empty argv elements are legal"
got=$("$PX" exec e2e-exec -- printf '%s-%s' '' x)
[[ $got == -x ]] && ok "empty argument delivered as ''" || bad "empty argument lost: '$got'"

say "5. oversized output is truncated and flagged"
# head writes 1100000 bytes; the server caps stdout at 1 MiB (1048576), so
# the CLI's stdout must carry exactly that many, with a truncation note on
# stderr. (Piping head into wc *inside* the container would observe the
# container's own byte count, which the cap does not touch.)
err=$(mktemp)
bytes=$("$PX" exec e2e-exec -- head -c 1100000 /dev/zero 2>"$err" | wc -c | tr -d ' ')
grep -q "output truncated at 1 MiB" "$err" && ok "truncation flagged on stderr" || bad "no truncation note: $(cat "$err")"
[[ $bytes == 1048576 ]] && ok "output capped at 1048576 bytes" || bad "cap wrong: $bytes"

say "6. gating: 404 unknown task, 409 task not running"
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$PX_SERVER/v1/tasks/does-not-exist/exec" \
  -H 'Content-Type: application/json' -d '{"command":["true"]}')
[[ $code == 404 ]] && ok "unknown task -> 404" || bad "unknown task -> $code, want 404"
# A task with an unresolvable image lands in ProvisionFailed with no
# container — permanently not-running, so the 409 verdict is deterministic.
apply <<'EOF'
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: e2e-exec-gone
spec:
  image: no-such-template-xyz
  runner:
    command: ["true"]
EOF
wait_phase e2e-exec-gone ProvisionFailed
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$PX_SERVER/v1/tasks/e2e-exec-gone/exec" \
  -H 'Content-Type: application/json' -d '{"command":["true"]}')
[[ $code == 409 ]] && ok "ProvisionFailed task -> 409" || bad "ProvisionFailed task -> $code, want 409"

say "7. request validation: NUL and trailing garbage are rejected"
code=$(printf '{"command":["a\\u0000b"]}' | curl -s -o /dev/null -w '%{http_code}' -X POST \
  "$PX_SERVER/v1/tasks/e2e-exec/exec" -H 'Content-Type: application/json' --data-binary @-)
[[ $code == 400 ]] && ok "NUL byte in argv -> 400" || bad "NUL byte -> $code, want 400"
code=$(printf '{"command":["true"]} trailing' | curl -s -o /dev/null -w '%{http_code}' -X POST \
  "$PX_SERVER/v1/tasks/e2e-exec/exec" -H 'Content-Type: application/json' --data-binary @-)
[[ $code == 400 ]] && ok "trailing garbage after JSON -> 400" || bad "trailing garbage -> $code, want 400"

say "8. cleanup"
"$PX" delete task e2e-exec-gone >/dev/null
"$PX" delete task e2e-exec >/dev/null
wait_gone e2e-exec-gone
wait_gone e2e-exec
# Store removal precedes the asynchronous destroy; the node-level check
# must wait the destroy out or it races the controller.
if [[ -n $PVE_SSH ]]; then
  waited=0
  while (( waited < 120 )); do
    # An ssh failure must not read as "no leftovers".
    if ! list=$(ssh -o BatchMode=yes "$PVE_SSH" 'pct list' 2>&1); then
      bad "node check failed: $(tail -n1 <<<"$list")"
      break
    fi
    if ! grep -q 'px-e2e-exec' <<<"$list"; then
      ok "no px-e2e-exec container left on the node"
      break
    fi
    sleep 2
    waited=$((waited+2))
  done
  (( waited < 120 )) || bad "container left on the node"
fi

printf '\n== results: %d passed, %d failed\n' "$pass" "$fail"
(( fail == 0 )) && echo "E2E exec test PASSED" || { echo "E2E exec test FAILED"; exit 1; }
