#!/usr/bin/env bash
# E2E for the Gateway kind against a real px-server + PVE node (see docs/e2e.md).
#
# Requires:
#   - px-server running with a working PVE token and node SSH access
#   - the runner template built on the node (px-runner-debian12)
#   - outbound internet from the task sandbox (for the deny/control checks)
#
# Env:
#   PX_SERVER   px-server URL        (default http://127.0.0.1:7420)
#   PX          px CLI binary path   (default ./px)
#   TIMEOUT     seconds to wait per task (default 300)
#   PVE_SSH     ssh target of the PVE node, for the firewall-config checks
#               (default root@192.168.2.100; empty disables them)
#   ALLOW_DEST  an IP reachable from the sandbox on tcp/8006, used as the
#               allow rule's cidr (default 192.168.2.100, the PVE node's
#               web/API port). Not a hostname: egress rules are CIDR-only.
#
# Checks: gateway CRUD is upsert-only, a task behind the allowlist reaches
# ALLOW_DEST:8006 but not the wider internet (DNS always allowed), a task
# without a gateway reaches both (control: distinguishes "firewall works"
# from "the sandbox has no network"), an empty egress list leaves only DNS,
# a missing gateway reference fails provisioning before any container is
# created, and the node shows net0 firewall=1 + policy_out DROP + the
# implicit DNS/DHCP rules while the task is up.
set -euo pipefail

export PX_SERVER="${PX_SERVER:-http://127.0.0.1:7420}"
PX="${PX:-./px}"
TIMEOUT="${TIMEOUT:-300}"
PVE_SSH="${PVE_SSH-root@192.168.2.100}"
ALLOW_DEST="${ALLOW_DEST:-192.168.2.100}"
GW_LOCKED=e2e-gw-locked
GW_DNS=e2e-gw-dns
TASK_LOCKED=e2e-gw-locked
TASK_OPEN=e2e-gw-open
TASK_DNS=e2e-gw-dns
TASK_MISSING=e2e-gw-missing

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
# GET against the raw API must authenticate the same way the CLI does.
api_get_code() { # path
  local -a curl_args=(curl -s -o /dev/null -w '%{http_code}')
  if [[ -n ${PX_TOKEN:-} ]]; then
    curl_args+=( -H "Authorization: Bearer $PX_TOKEN" )
  fi
  "${curl_args[@]}" "$PX_SERVER$1"
}

gateway_yaml() { # $1 name; $2 = "empty" for the DNS-only variant
  local variant="${2:-}"
  if [[ $variant == empty ]]; then
    cat <<EOF
apiVersion: px.io/v1alpha1
kind: Gateway
metadata:
  name: $1
spec:
  egress: []
EOF
  else
    cat <<EOF
apiVersion: px.io/v1alpha1
kind: Gateway
metadata:
  name: $1
spec:
  egress:
    - cidr: $ALLOW_DEST
      ports: "8006"
EOF
  fi
}

task_yaml() { # $1 task name, $2 gateway name ("" for none)
  cat <<EOF
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: $1
spec:
  image: px-runner-debian12
EOF
  if [[ -n $2 ]]; then
    printf '  gateway: %s\n' "$2"
  fi
  cat <<EOF
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
EOF
}

# Verifies the task's single EGRESS_CHECK line against the wanted verdicts.
# The grep must not abort the script when the line is missing — that case is
# a counted FAIL, and later sections must still run.
check_egress() { # task want_pve want_ext want_dns
  local task=$1 logs line
  if ! logs=$("$PX" logs "$task" 2>&1); then
    bad "logs for $task failed: $logs"
    return
  fi
  line=$(grep -o 'EGRESS_CHECK.*' <<<"$logs" | tail -1) || true
  if [[ $line == "EGRESS_CHECK pve=$2 ext=$3 dns=$4" ]]; then
    ok "$task egress verdict: $line"
  else
    bad "$task expected pve=$2 ext=$3 dns=$4, got: ${line:-<no EGRESS_CHECK line>} (logs: $(echo "$logs" | tail -5))"
  fi
}

cleanup() { # best-effort: no task, gateway or container left behind
  for t in "$TASK_LOCKED" "$TASK_OPEN" "$TASK_DNS" "$TASK_MISSING"; do
    "$PX" delete task "$t" >/dev/null 2>&1 || true
  done
  for g in "$GW_LOCKED" "$GW_DNS"; do
    "$PX" delete gateway "$g" >/dev/null 2>&1 || true
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

say "1. gateway CRUD: apply, list, describe, upsert"
if apply < <(gateway_yaml "$GW_LOCKED"); then
  ok "gateway $GW_LOCKED applied"
else
  bad "gateway apply failed"
fi
apply < <(gateway_yaml "$GW_DNS" empty) || true
if list=$("$PX" get gateways 2>&1); then
  if grep -q "^$GW_LOCKED " <<<"$list" && grep -q "^$GW_DNS " <<<"$list"; then
    ok "both gateways listed"
  else
    bad "gateway list incomplete: $list"
  fi
else
  bad "get gateways failed: $list"
fi
if desc=$("$PX" describe gateway "$GW_LOCKED" 2>&1); then
  if grep -q "\"cidr\": \"$ALLOW_DEST\"" <<<"$desc" && grep -q '"ports": "8006"' <<<"$desc"; then
    ok "describe shows the egress rule"
  else
    bad "describe missing the rule: $desc"
  fi
else
  bad "describe gateway failed: $desc"
fi
# Gateways upsert (like Workspaces/Models), unlike Tasks which conflict.
if apply < <(gateway_yaml "$GW_LOCKED"); then
  ok "re-apply upserts without conflict"
else
  bad "re-apply of an existing gateway failed"
fi

say "2. locked task: ALLOW_DEST:8006 allowed, internet denied, DNS up"
apply < <(task_yaml "$TASK_LOCKED" "$GW_LOCKED") || true
if wait_phase "$TASK_LOCKED" Succeeded; then
  check_egress "$TASK_LOCKED" ok blocked ok
fi

say "3. node-side firewall config on the locked task's container"
if [[ -n $PVE_SSH ]]; then
  vmid=$("$PX" describe task "$TASK_LOCKED" | grep '"container"' | tr -dc '0-9' || true)
  if [[ -z $vmid ]]; then
    bad "no container id on the task (provision failed?)"
  else
    ssh_opts=(-o BatchMode=yes -o ConnectTimeout=5)
    if cfg=$(ssh "${ssh_opts[@]}" "$PVE_SSH" "pct config $vmid" 2>&1); then
      # net0 firewall=1 is the only switch an LXC has — there is no
      # top-level firewall config property (that is QEMU-only, and pct
      # rejects it with "Unknown option: firewall").
      if grep -q 'firewall=1' <<<"$cfg"; then
        ok "net0 carries firewall=1"
      else
        bad "firewall flag missing from net0: $(grep '^net0' <<<"$cfg")"
      fi
    else
      bad "pct config on $vmid failed: $cfg"
    fi
    if fw=$(ssh "${ssh_opts[@]}" "$PVE_SSH" "cat /etc/pve/firewall/$vmid.fw" 2>&1); then
      for want in "policy_out: DROP" "px: dns" "px: dhcp" "px: gateway $GW_LOCKED"; do
        if grep -qF "$want" <<<"$fw"; then
          ok ".fw has: $want"
        else
          bad ".fw missing: $want"
        fi
      done
    else
      bad "reading /etc/pve/firewall/$vmid.fw failed: $fw"
    fi
  fi
else
  echo "SKIP: node-side firewall checks (PVE_SSH is empty)"
fi

say "4. control task without a gateway: everything reachable"
# Distinguishes "the firewall blocks" from "the sandbox has no network":
# if this one cannot reach the internet either, the deny verdicts above
# prove nothing about the allowlist.
apply < <(task_yaml "$TASK_OPEN" "") || true
if wait_phase "$TASK_OPEN" Succeeded; then
  check_egress "$TASK_OPEN" ok ok ok
fi

say "5. empty egress: DNS only"
apply < <(task_yaml "$TASK_DNS" "$GW_DNS") || true
if wait_phase "$TASK_DNS" Succeeded; then
  check_egress "$TASK_DNS" blocked blocked ok
fi

say "6. a task referencing a missing gateway fails before provisioning"
apply < <(task_yaml "$TASK_MISSING" "$GW_LOCKED-nonexistent") || true
if wait_phase "$TASK_MISSING" ProvisionFailed; then
  if reason=$("$PX" describe task "$TASK_MISSING" | grep '"reason"'); then
    ok "reason: $(tr -d ' ' <<<"$reason")"
  fi
  # A resolved-but-missing gateway must be caught before any container
  # work: "container" is omitempty in the status JSON, so its absence
  # proves no VMID was ever persisted.
  if desc=$("$PX" describe task "$TASK_MISSING"); then
    if grep -q '"container"' <<<"$desc"; then
      bad "ProvisionFailed task has a container id: $(grep '"container"' <<<"$desc")"
    else
      ok "no container was created for the failed task"
    fi
  else
    bad "describe task $TASK_MISSING failed"
  fi
else
  "$PX" logs "$TASK_MISSING" 2>/dev/null | tail -5 || true
fi

say "7. cleanup: tasks and gateways go away with nothing left on the node"
for t in "$TASK_LOCKED" "$TASK_OPEN" "$TASK_DNS" "$TASK_MISSING"; do
  "$PX" delete task "$t" >/dev/null || true
  wait_gone "$t"
done
if [[ -n $PVE_SSH ]]; then
  if list=$(ssh -o BatchMode=yes -o ConnectTimeout=5 "$PVE_SSH" "pct list" 2>&1); then
    for t in "$TASK_LOCKED" "$TASK_OPEN" "$TASK_DNS" "$TASK_MISSING"; do
      if grep -q "px-$t" <<<"$list"; then
        bad "container px-$t left on the node"
      else
        ok "no px-$t container left on the node"
      fi
    done
  else
    bad "ssh to $PVE_SSH failed, leftover check skipped: $list"
  fi
fi
for g in "$GW_LOCKED" "$GW_DNS"; do
  "$PX" delete gateway "$g" >/dev/null || true
  code=$(api_get_code "/v1/gateways/$g")
  if [[ $code == 404 ]]; then
    ok "gateway $g deleted (GET returns 404)"
  else
    bad "gateway $g still present after delete (GET returned $code)"
  fi
done

printf '\n== results: %d passed, %d failed\n' "$pass" "$fail"
(( fail == 0 )) || exit 1
echo "E2E gateway test PASSED"
