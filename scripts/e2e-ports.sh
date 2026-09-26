#!/usr/bin/env bash
# E2E for Task spec.ports (port exposure) against a real px-server + PVE node
# (see docs/e2e.md).
#
# Requires:
#   - px-server running with a working PVE token and node SSH access
#   - the runner template built on the node (px-runner-debian12, has python3)
#
# Env:
#   PX_SERVER   px-server URL        (default http://127.0.0.1:7420)
#   PX          px CLI binary path   (default ./px)
#   TIMEOUT     seconds to wait per task (default 300)
#   PVE_SSH     ssh target of the PVE node, for the socat/listener checks
#               (default root@192.168.2.100; empty disables them)
#   NODE_HOST   address to dial the published ports on (default: the host
#               part of PVE_SSH). The socat listeners bind every node
#               interface, so any LAN address of the node works.
#
# Checks: an auto-assigned hostPort and an explicit one both serve the
# container's HTTP server from the node, a second apply claiming the same
# explicit hostPort is refused with 409, a live download survives several
# reconcile ticks (socat forks a child per connection — the controller must
# not read that as drift and kill the forward), a forward killed on the node
# is rebuilt by the next tick, and deleting the task removes the listener.
set -euo pipefail

export PX_SERVER="${PX_SERVER:-http://127.0.0.1:7420}"
PX="${PX:-./px}"
TIMEOUT="${TIMEOUT:-300}"
PVE_SSH="${PVE_SSH-root@192.168.2.100}"
NODE_HOST="${NODE_HOST-${PVE_SSH#*@}}"
EXPLICIT_HP=31234
TASK_AUTO=e2e-ports-auto
TASK_EXPL=e2e-ports-expl
TASK_CONF=e2e-ports-conflict

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
hostport() { # task: the first resolved hostPort from the task's status
  "$PX" describe task "$1" 2>/dev/null | grep '"hostPort"' | head -1 | tr -dc '0-9'
}
# GET/POST against the raw API must authenticate the same way the CLI does.
api_code() { # method path [body]
  local -a curl_args=(curl -s -o /dev/null -w '%{http_code}' -X "$1")
  if [[ -n ${PX_TOKEN:-} ]]; then
    curl_args+=( -H "Authorization: Bearer $PX_TOKEN" )
  fi
  if (( $# >= 3 )); then
    curl_args+=( -H 'Content-Type: application/json' -d "$3" )
  fi
  "${curl_args[@]}" "$PX_SERVER$2"
}
# The controller reconciles every 2s; ticksleep(6) covers ~3 of them.
ticksleep() { sleep "$1"; }

serve_yaml() { # task name, hostPort (0 = auto)
  local hp_arg=""
  if [[ $2 != 0 ]]; then
    hp_arg=$'\n'"      hostPort: $2"
  fi
  cat <<EOF
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: $1
spec:
  image: px-runner-debian12
  ports:
    - name: http
      port: 8080$hp_arg
  runner:
    command:
      - sh
      - -c
      - |
        mkdir -p /tmp/www
        dd if=/dev/urandom of=/tmp/www/bigfile bs=1M count=1 2>/dev/null
        cd /tmp/www
        exec python3 -m http.server 8080
EOF
}

# The service must answer through the published port from the LAN side.
# Running alone doesn't mean reachable yet: the container may still be
# waiting on DHCP when the phase flips (the controller retries until the
# lease lands and only then sets the socat forwards), and the runner's HTTP
# server comes up a beat after that — so poll briefly instead of firing one
# curl and reading that boot gap as a product failure. A connection error
# must surface as code=000 and a FAIL verdict, not as a set -e abort —
# hence the || true.
await_serves() { # hp label
  local code tries=0
  while :; do
    code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time 15 "http://$NODE_HOST:$1/bigfile" || true)
    if [[ $code == 200 ]]; then
      ok "$2: http://$NODE_HOST:$1/bigfile -> 200"
      return 0
    fi
    if (( tries >= 14 )); then
      bad "$2: http://$NODE_HOST:$1/bigfile -> HTTP $code (want 200, waited ~30s)"
      return 1
    fi
    sleep 2
    tries=$((tries+1))
  done
}

cleanup() { # best-effort: no task or forward left behind
  for t in "$TASK_AUTO" "$TASK_EXPL" "$TASK_CONF"; do
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
cleanup

say "1. auto-assigned hostPort serves from the node"
apply < <(serve_yaml "$TASK_AUTO" 0) || true
if wait_phase "$TASK_AUTO" Running; then
  hp=$(hostport "$TASK_AUTO")
  if (( hp >= 30000 && hp <= 32767 )); then
    ok "$TASK_AUTO got hostPort $hp (in the px range)"
  else
    bad "$TASK_AUTO hostPort out of range: $hp"
  fi
  await_serves "$hp" "auto port"
else
  "$PX" logs "$TASK_AUTO" 2>/dev/null | tail -5 || true
fi

say "2. explicit hostPort honored verbatim"
apply < <(serve_yaml "$TASK_EXPL" "$EXPLICIT_HP") || true
if wait_phase "$TASK_EXPL" Running; then
  hp=$(hostport "$TASK_EXPL")
  if [[ $hp == "$EXPLICIT_HP" ]]; then
    ok "$TASK_EXPL kept hostPort $EXPLICIT_HP"
  else
    bad "$TASK_EXPL hostPort: $hp, want $EXPLICIT_HP"
  fi
  await_serves "$EXPLICIT_HP" "explicit port"
fi

say "3. a second apply claiming the same hostPort is refused (409)"
# The API refuses the claim before any container work; the CLI surfaces the
# conflict on stderr with a non-zero exit.
if err=$(printf '%s' "$(serve_yaml "$TASK_CONF" "$EXPLICIT_HP")" | "$PX" apply -f - 2>&1 >/dev/null); then
  bad "conflicting apply unexpectedly succeeded"
else
  if grep -qiE "conflict|409" <<<"$err"; then
    ok "conflicting apply refused: $(head -1 <<<"$err")"
  else
    bad "conflicting apply failed with an unexpected error: $err"
  fi
fi

say "4. a live download survives several reconcile ticks"
# socat forks a child per active connection; the controller must treat that
# as a healthy forward. A 1 MiB file at 100 KB/s spans ~5 ticks at 2s each —
# the pre-fix controller killed the forward on the first tick.
# The transfer starts only once forward and service are confirmed up, so it
# exercises an established forward and races nothing but the ticks.
if await_serves "$EXPLICIT_HP" "service up before download"; then
  dl_pid=""
  curl -s --limit-rate 100k --max-time 60 -o /dev/null \
    "http://$NODE_HOST:$EXPLICIT_HP/bigfile" &
  dl_pid=$!
  if kill -0 "$dl_pid" 2>/dev/null; then
    ticksleep 6
    if kill -0 "$dl_pid" 2>/dev/null; then
      ok "download still in flight after ~3 ticks (no mid-transfer kill)"
    else
      wait "$dl_pid" || true
      bad "download died before the file finished (forward was torn down mid-transfer?)"
    fi
    if wait "$dl_pid" 2>/dev/null; then
      ok "download completed across the ticks"
    else
      bad "download failed to complete"
    fi
  else
    bad "could not start the download at all"
  fi
fi

say "5. a forward killed on the node is rebuilt by the next tick"
if [[ -n $PVE_SSH ]]; then
  ssh -o BatchMode=yes -o ConnectTimeout=5 "$PVE_SSH" \
    "pkill -f 'socat .*TCP-LISTEN:$EXPLICIT_HP,'" 2>/dev/null || true
  if n=$(ssh -o BatchMode=yes -o ConnectTimeout=5 "$PVE_SSH" \
       "pgrep -fc 'socat .*TCP-LISTEN:$EXPLICIT_HP,'" 2>/dev/null) && [[ $n == 0 ]]; then
    ok "listener gone from the node after pkill"
  else
    echo "note: listener check after pkill: n=${n:-<ssh failed>}" >&2
  fi
  ticksleep 6
  await_serves "$EXPLICIT_HP" "rebuilt forward"
else
  echo "SKIP: node-side kill/rebuild checks (PVE_SSH is empty)"
fi

say "6. deleting the task removes the listener"
"$PX" delete task "$TASK_AUTO" >/dev/null || true
"$PX" delete task "$TASK_EXPL" >/dev/null || true
wait_gone "$TASK_AUTO" && wait_gone "$TASK_EXPL"
if [[ -n $PVE_SSH ]]; then
  # Scan by port pattern: the tasks are already gone from the store, so
  # their resolved hostPorts are no longer readable from describe.
  leftovers=$(ssh -o BatchMode=yes -o ConnectTimeout=5 "$PVE_SSH" \
    "pgrep -af 'socat .*TCP-LISTEN:3[0-9]{4},'" 2>/dev/null || true)
  if [[ -z $leftovers ]]; then
    ok "no socat listeners left in the px range on the node"
  else
    bad "leftover socat listeners on the node: $leftovers"
  fi
  check_refused() { # hp
    if curl -s --connect-timeout 3 -o /dev/null "http://$NODE_HOST:$1/"; then
      bad "port $1 still accepting connections after delete"
    else
      ok "port $1 refuses connections after delete"
    fi
  }
  check_refused "$EXPLICIT_HP"
fi
# the conflict task was refused at apply, so it must not exist
code=$(api_code GET "/v1/tasks/$TASK_CONF")
if [[ $code == 404 ]]; then
  ok "$TASK_CONF was never created (GET returns 404)"
else
  bad "$TASK_CONF exists despite the 409 apply (GET returned $code)"
fi

printf '\n== results: %d passed, %d failed\n' "$pass" "$fail"
(( fail == 0 )) || exit 1
echo "E2E ports test PASSED"
