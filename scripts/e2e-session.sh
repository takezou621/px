#!/usr/bin/env bash
# E2E for session continuation (M8) against a real px-server + PVE node
# (see docs/e2e.md). Everything runs with a planted fake session JSONL, so
# no API credential is needed anywhere: the capture/restore transport moves
# ~/.claude/projects around regardless of what the conversation contains.
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
#   1. a source task writes a fake session into ~/.claude/projects and
#      finishes — the terminal tick captures it (sessionSaved true,
#      sessionBytes > 0 on the record)
#   2. a continuation task applied with spec.session.continueFrom runs in a
#      fresh container whose ~/.claude/projects carries the source's file
#   3. `px run --continue` builds the right spec (continueFrom set, the
#      default command swapped to its --continue variant) — spec-only,
#      the task is deleted before its runner boots
#   4. after the source record is deleted, a new continuation fails at
#      provision (the capture died with the source)
# A live agent run with a real key is deliberately not automated here: it
# burns tokens and needs supervision (docs/roadmap.md, M6).
set -euo pipefail

export PX_SERVER="${PX_SERVER:-http://127.0.0.1:7420}"
PX="${PX:-./px}"
TIMEOUT="${TIMEOUT:-300}"
PVE_SSH="${PVE_SSH-root@192.168.2.100}"
TPL=px-agent-debian12
SRC=e2e-sess-src
CONT=e2e-sess-cont
CLI=e2e-sess-cli
CLS=e2e-sess-clisrc
GONE=e2e-sess-gone

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
wait_record_gone() { # name — a record disappears only after its async destroy
  local name=$1 waited=0
  while (( waited < TIMEOUT )); do
    if [[ -z $(phase "$name") ]]; then
      ok "$name record gone (${waited}s)"
      return 0
    fi
    sleep 1
    waited=$((waited+1))
  done
  bad "$name record did not disappear within ${TIMEOUT}s (last phase: '$(phase "$name")')"
  return 1
}
cleanup() {
  "$PX" delete task "$SRC"  >/dev/null 2>&1 || true
  "$PX" delete task "$CONT" >/dev/null 2>&1 || true
  "$PX" delete task "$CLI"  >/dev/null 2>&1 || true
  "$PX" delete task "$CLS"  >/dev/null 2>&1 || true
  "$PX" delete task "$GONE" >/dev/null 2>&1 || true
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

say "1. the source task plants a fake session; the terminal tick captures it"
# The runner writes one JSONL line in the layout Claude Code uses under
# ~/.claude/projects. Its exact shape does not matter to the transport —
# the capture tars the whole directory — only to check 2's grep.
"$PX" run -name "$SRC" -no-wait "seed the session" -- sh -c \
  'mkdir -p "$HOME/.claude/projects/-root-e2e" && printf "%s\n" "{\"type\":\"user\",\"message\":{\"content\":\"hello from e2e-session source\"}}" > "$HOME/.claude/projects/-root-e2e/e2e.jsonl" && echo seeded'
if wait_phase "$SRC" Succeeded; then
  desc=$("$PX" describe task "$SRC")
  if grep -q '"sessionSaved": true' <<<"$desc"; then
    ok "record marks the capture settled (sessionSaved: true)"
  else
    bad "sessionSaved not set on the record: $desc"
  fi
  if grep -qE '"sessionBytes": [1-9]' <<<"$desc"; then
    ok "capture stored bytes > 0"
  else
    bad "sessionBytes missing or zero (capture saved nothing): $desc"
  fi
fi

say "2. a continuation applied with spec.session.continueFrom finds the source's file"
# Applied as YAML (not via px run --continue) so the runner command can be
# a verifier instead of the Claude CLI — no credential involved. The
# controller's restore lands the archive in the fresh HOME before boot.
printf 'apiVersion: px.io/v1alpha1\nkind: Task\nmetadata:\n  name: %s\nspec:\n  image: %s\n  goal: verify the restored session\n  runner:\n    command:\n      - sh\n      - -c\n      - |\n        f=$(find "$HOME/.claude/projects" -name "*.jsonl" 2>/dev/null | head -1)\n        [ -n "$f" ] || { echo "NO-SESSION-FILES"; exit 1; }\n        grep -q "hello from e2e-session source" "$f" || { echo "SESSION-CONTENT-MISSING"; exit 1; }\n        echo "SESSION-RESTORED-OK ($f)"\n  session:\n    continueFrom: %s\n' "$CONT" "$TPL" "$SRC" | "$PX" apply -f - >/dev/null
if wait_phase "$CONT" Succeeded; then
  if grep -q "SESSION-RESTORED-OK" <("$PX" logs "$CONT"); then
    ok "session archive restored into the continuation's HOME"
  else
    bad "verifier did not see the restored session: $("$PX" logs "$CONT")"
  fi
fi

say "3. px run --continue builds the continuation spec (deleted before it boots)"
# The swap to the --continue agent command is only visible from a source
# whose runner command IS the default, and check 1's source ran a custom
# verifier instead. So this check plants its own default-command source.
# Session validation at apply checks the name only, so the source does not
# need to be finished first; both tasks are deleted before a runner boots.
"$PX" run -name "$CLS" -no-wait "smoke source (default command)" >/dev/null
"$PX" run -continue "$CLS" -name "$CLI" -no-wait "smoke the CLI path" >/dev/null
desc=$("$PX" describe task "$CLI")
if grep -q "\"continueFrom\": \"$CLS\"" <<<"$desc" \
    && grep -q -- '--continue' <<<"$desc" \
    && grep -q 'claude --dangerously-skip-permissions' <<<"$desc"; then
  ok "px run --continue set continueFrom and swapped in the --continue agent command"
else
  bad "CLI continuation spec wrong: $desc"
fi
"$PX" delete task "$CLI" >/dev/null
"$PX" delete task "$CLS" >/dev/null

say "4. after the source record is gone, a new continuation fails at provision"
"$PX" delete task "$SRC" >/dev/null
# Deletion is async — the record disappears only after the controller
# destroys the container and drops it, and the capture row dies with the
# record. The continuation below may only land in ProvisionFailed once
# that is true, so wait for the disappearance first.
if wait_record_gone "$SRC"; then
  printf 'apiVersion: px.io/v1alpha1\nkind: Task\nmetadata:\n  name: %s\nspec:\n  image: %s\n  goal: must not run\n  runner:\n    command: ["true"]\n  session:\n    continueFrom: %s\n' "$GONE" "$TPL" "$SRC" | "$PX" apply -f - >/dev/null
  if wait_phase "$GONE" ProvisionFailed; then
    ok "continuation without its source failed at provision (no container)"
  fi
fi

say "5. cleanup"
for t in "$CONT" "$CLI" "$GONE" "$SRC"; do
  "$PX" delete task "$t" >/dev/null 2>&1 || true
done

printf '\n== results: %d passed, %d failed\n' "$pass" "$fail"
(( fail == 0 )) || exit 1
echo "E2E session test PASSED"
