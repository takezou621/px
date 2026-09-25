#!/usr/bin/env bash
# E2E for the Model kind against a real px-server + PVE node (see docs/e2e.md).
#
# Requires:
#   - px-server running with a working PVE token and node SSH access
#   - the runner template built on the node (px-runner-debian12)
#
# Env:
#   PX_SERVER  px-server URL            (default http://127.0.0.1:7420)
#   PX         px CLI binary path       (default ./px)
#   TIMEOUT    seconds to wait per task (default 300)
#   PVE_SSH    ssh target of the PVE node, for the live file-mode and
#              container-cleanup checks (default root@192.168.2.100;
#              empty disables them)
#   MODEL_KEY  dummy key to apply       (default sk-e2e-px-model-0123456789;
#              must be YAML-plain-safe: no quotes, no ": ")
#
# Checks: the key is write-only (apply/get/list never echo it), re-applying
# the fetched <redacted> placeholder is rejected, a task referencing the
# model gets the credentials in its runner env (presence + length only —
# the key must never reach the logs), /run/px/model.* are 0600 inside the
# container, and task + model clean up with nothing left on the node.
set -euo pipefail

export PX_SERVER="${PX_SERVER:-http://127.0.0.1:7420}"
PX="${PX:-./px}"
TIMEOUT="${TIMEOUT:-300}"
PVE_SSH="${PVE_SSH-root@192.168.2.100}"
MODEL_KEY="${MODEL_KEY:-sk-e2e-px-model-0123456789}"
MODEL=e2e-model-claude
TASK=e2e-model

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
apply() { "$PX" apply -f - >/dev/null; }
model_yaml() { # $1 = apiKey value
  printf 'apiVersion: px.io/v1alpha1\nkind: Model\nmetadata:\n  name: %s\nspec:\n  provider: anthropic\n  apiKey: %s\n  baseUrl: https://proxy.example.com/v1\n' "$MODEL" "$1"
}

cleanup() { # best-effort: don't leave the model or its container behind
  "$PX" delete task "$TASK" >/dev/null 2>&1 || true
  "$PX" delete model "$MODEL" >/dev/null 2>&1 || true
}
trap cleanup EXIT

say "0. px-server is reachable"
if "$PX" get tasks >/dev/null 2>&1; then
  ok "GET /v1/tasks responded"
else
  echo "FATAL: cannot reach px-server at $PX_SERVER (start it first, see docs/e2e.md)" >&2
  exit 1
fi
cleanup

say "1. the API key is write-only: nothing echoes it back"
# Diagnostics may echo server responses — scrub the dummy key everywhere.
scrub() { printf '%s' "${1//"$MODEL_KEY"/<KEY>}"; }
if ! out=$(model_yaml "$MODEL_KEY" | "$PX" apply -f - 2>&1); then
  bad "model apply failed: $(scrub "$out")"
elif [[ $out == *"$MODEL_KEY"* ]]; then
  bad "apply response echoed the key"
else
  ok "apply response is key-free"
fi
if ! list=$("$PX" get models 2>&1); then
  bad "get models failed: $(scrub "$list")"
elif [[ $list == *"$MODEL_KEY"* ]]; then
  bad "key leaked via list"
else
  ok "list is key-free"
fi
if ! desc=$("$PX" describe model "$MODEL" 2>&1); then
  bad "describe model failed: $(scrub "$desc")"
elif [[ $desc == *"$MODEL_KEY"* ]]; then
  bad "key leaked via describe"
elif [[ $desc == *"<redacted>"* ]]; then
  ok "describe is key-free and shows the <redacted> placeholder"
else
  bad "describe missing the placeholder: $(scrub "$desc")"
fi

say "2. re-applying a fetched model (apiKey=<redacted>) is rejected"
if out=$(model_yaml '<redacted>' | "$PX" apply -f - 2>&1); then
  bad "placeholder apply was accepted"
else
  if [[ $out == *placeholder* ]]; then
    ok "rejected, error names the placeholder"
  else
    bad "rejected but the error does not name the placeholder: $out"
  fi
fi

say "3. a task referencing the model gets the credentials in its env"
apply <<'EOF'
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: e2e-model
spec:
  image: px-runner-debian12
  model: e2e-model-claude
  runner:
    command: ["sh", "-c", "if [ -n \"$ANTHROPIC_API_KEY\" ]; then echo KEY_PRESENT len=${#ANTHROPIC_API_KEY}; else echo KEY_MISSING; fi; echo \"URL=$ANTHROPIC_BASE_URL\""]
EOF
if wait_phase "$TASK" Succeeded; then
  logs=$("$PX" logs "$TASK")
  # Error messages below print logs — scrub the dummy key so a leak cannot
  # propagate into the test output.
  safe_logs=${logs//"$MODEL_KEY"/<KEY>}
  len=${#MODEL_KEY}
  if [[ $logs == *"$MODEL_KEY"* ]]; then
    bad "raw key reached the logs"
  else
    ok "logs carry no raw key"
  fi
  if [[ $logs == *"KEY_PRESENT len=$len"* ]]; then
    ok "ANTHROPIC_API_KEY injected (len=$len)"
  else
    bad "key env missing or wrong length: $safe_logs"
  fi
  if [[ $logs == *"URL=https://proxy.example.com/v1"* ]]; then
    ok "ANTHROPIC_BASE_URL injected"
  else
    bad "base URL env missing: $safe_logs"
  fi
fi

say "4. /run/px/model.* are 0600 inside the container"
if [[ -n $PVE_SSH ]]; then
  vmid=$("$PX" describe task "$TASK" | grep '"container"' | tr -dc '0-9' || true)
  if [[ -z $vmid ]]; then
    bad "no container id on the task (provision failed?)"
  elif modes=$(ssh -o BatchMode=yes -o ConnectTimeout=5 "$PVE_SSH" \
      "pct exec $vmid -- stat -c '%a %n' /run/px/model.key /run/px/model.env /run/px/boot.sh" 2>&1); then
    if [[ $(grep -c '^600 ' <<<"$modes") == 3 ]]; then
      ok "model.key/model.env/boot.sh are all 0600"
    else
      bad "expected 0600 on all three files, got: $modes"
    fi
  else
    bad "pct exec stat on $vmid failed: $modes"
  fi
else
  echo "SKIP: file-mode check (PVE_SSH is empty)"
fi

say "5. cleanup: task delete removes the container, model delete removes the model"
"$PX" delete task "$TASK" >/dev/null
wait_gone "$TASK"
if [[ -n $PVE_SSH ]]; then
  if list=$(ssh -o BatchMode=yes -o ConnectTimeout=5 "$PVE_SSH" "pct list" 2>&1); then
    if grep -q "px-$TASK" <<<"$list"; then
      bad "container px-$TASK left on the node"
    else
      ok "no px-$TASK container left on the node"
    fi
  else
    bad "ssh to $PVE_SSH failed, leftover check skipped: $list"
  fi
fi
"$PX" delete model "$MODEL" >/dev/null
code=$(curl -s -o /dev/null -w '%{http_code}' "$PX_SERVER/v1/models/$MODEL")
if [[ $code == 404 ]]; then
  ok "model deleted (GET returns 404)"
else
  bad "model still present after delete (GET returned $code)"
fi

printf '\n== results: %d passed, %d failed\n' "$pass" "$fail"
(( fail == 0 )) || exit 1
echo "E2E model test PASSED"
