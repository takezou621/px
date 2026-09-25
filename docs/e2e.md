# E2E: running px against a real Proxmox VE node

Unit tests run without PVE; this is the manual path that closes M1.

## Prerequisites

- A Proxmox VE node (single node is fine) reachable over HTTPS from the
  machine that will run px-server.
- A PVE API token with permissions to clone/start/stop/delete LXC
  containers (`PVEVMUser` + `PVEVMAdmin` on `/vms` and `/storage` is
  enough for one node). Format: `user@realm!tokenid=secret`.
- SSH from the px-server host to the PVE node as root (`root@pam`),
  either with a key file or a loaded agent. px uses SSH + `pct exec`
  for the runner boot step.
- The `px` and `px-server` binaries. Build each package separately —
  `go build` with multiple packages compiles them but writes no
  binaries:
  `go build -o px ./cmd/px && go build -o px-server ./cmd/px-server`.

## 1. Build the runner template (once per node)

On your workstation, copy the build script to the node and run it as root:

```sh
scp template/runner/build.sh root@<PVE_HOST>:/root/
ssh root@<PVE_HOST> /root/build.sh <CTID>          # e.g. 999
# optional args: /root/build.sh <CTID> <bridge> <rootfs-storage> <tpl-storage>
# defaults:      vmbr0, local-lvm, local
```

This downloads the Debian 12 standard image, creates an unprivileged
container with `nesting=1`, installs the base tools, and converts it to
the template `px-runner-debian12`. Reference that name as `spec.image`
in Task manifests. Note: the image file must go on a `vztmpl`-capable
storage (dir/NFS, default `local`); lvmthin pools like `local-lvm`
reject template files, so keep them separate.

## 2. Start px-server

```sh
export PX_PVE_ENDPOINT="https://<PVE_HOST>:8006"
export PX_PVE_NODE="<NODE_NAME>"
export PX_PVE_TOKEN="root@pam!px=<SECRET>"    # or your user@realm!tokenid

go run ./cmd/px-server -tls-insecure
# flags: -listen 127.0.0.1:7420 -db px.db -pve-endpoint ... -pve-node ...
#        -pve-token ... -tls-insecure -ssh-user root -ssh-key <path>
#        -reconcile-interval 2s
```

A stock PVE node presents a self-signed cert (`pve-ssl`); unless you
imported the PVE root CA into the machine running px-server, pass
`-tls-insecure` (or `PX_PVE_TLS_INSECURE=1`) — the same trade-off as
kubectl's `--insecure-skip-tls-verify`.

On startup px-server dials the PVE API and the node SSH once and fails
fast if either is unreachable; it then serves the (unauthenticated,
loopback-bound) REST API and runs the reconcile loop.

### Cluster mode

Leave `-pve-node` unset and px-server schedules each new task onto a
cluster node instead of pinning one: candidates are nodes that are
online AND hold the task's `spec.image` as an LXC template (from one
`GET /cluster/resources`), and the pick is most free memory, tie-broken
by lower CPU load then node name. Every node is reached through the
same API endpoint (PVE proxies cross-node requests) and one SSH
connection per node, dialed lazily — a node no task ever lands on is
never SSH'd.

PVE node names are not DNS names, so map each node to its SSH host:

```sh
unset PX_PVE_NODE
go run ./cmd/px-server -tls-insecure \
  -ssh-host-override "third=192.168.2.100,second=192.168.2.183"
```

Host key pinning stays per host in cluster mode: a pin-file line in
known_hosts format (`host key...`) pins only that host, while an
authorized_keys-format line (no hostname field) pins every host px
dials, so a single-node pin file keeps working verbatim.

## 3. Run the smoke test

```sh
./scripts/e2e-smoke.sh
# env: PX_SERVER (default http://127.0.0.1:7420), PX (default ./px),
#      TIMEOUT seconds per task (default 300)
```

It exercises: a task that succeeds (and its logs), a task that fails
(exit code recorded), goal delivery into the container, deleting a
running task, and duplicate-apply rejection — then cleans up its tasks.
Section 3's workspace check clones a public git repository, so the
sandbox needs outbound git access (point `spec.git.repo` at a local
mirror if it does not). Workspace deletion does not exist yet (see
roadmap backlog), so the applied `e2e-ws` stays in the store.

## 3b. Model-kind test

With the same server running:

```sh
./scripts/e2e-model.sh
# env: PX_SERVER, PX, TIMEOUT as above; PVE_SSH (default root@<node>,
#      empty disables the node-level checks); MODEL_KEY (default a dummy)
```

It exercises: the API key is write-only (apply/list/describe never echo
it, describe shows `<redacted>`), re-applying the placeholder is
rejected, a task referencing the model gets `ANTHROPIC_API_KEY`
(presence + length only — the key never reaches the logs) and
`ANTHROPIC_BASE_URL`, `/run/px/model.*` are 0600 inside the container,
and task + model delete cleanly with no container left on the node.

What you should see while it runs (from another terminal):

```sh
./px get tasks                 # phase transitions every reconcile tick
./px describe task e2e-ok      # full status JSON
./px logs -f e2e-ok            # tail runner output
```

## 3c. Gateway-kind test

With the same server running:

```sh
./scripts/e2e-gateway.sh
# env: PX_SERVER, PX, TIMEOUT as above; PVE_SSH (default root@<node>,
#      empty disables the node-level checks); ALLOW_DEST (default
#      192.168.2.100 — an IP reachable from the sandbox on tcp/8006,
#      used as the allow rule's cidr)
```

It exercises: gateway CRUD is upsert-only, a task behind the allowlist
reaches `ALLOW_DEST:8006` but not the wider internet (DNS stays
allowed, `policy_out=DROP` denies the rest), a task **without** a
gateway reaches both (control — if this one fails too, the sandbox
just has no network and the deny verdicts mean nothing), an empty
`egress: []` leaves the container DNS-only, a task referencing a
missing gateway lands in `ProvisionFailed` before any container is
created, and while the locked task runs the node shows the container's
`net0` interface carrying `firewall=1` (the only firewall switch an LXC
has — the top-level `firewall` option is QEMU-only), and
`/etc/pve/firewall/<CTID>.fw` with `policy_out: DROP` plus the implicit
`px: dns` / `px: dhcp` rules and the gateway's own rule. Cleanup
removes all tasks and gateways.

The empty-egress section is also a regression tripwire for a subtle
timing property: pve-firewall programs the dataplane a few seconds
*after* the container starts, so a runner that boots immediately gets a
short unrestricted window (measured ~3s with a per-second egress
probe). The provisioner closes it by gating the boot on the container's
`veth<CTID>i0-OUT` chain appearing in the node's iptables — in both the
v4 and v6 rulesets, which pve-firewall loads in separate passes — which
is why the DNS-only verdicts are deterministic.

The deny check targets `https://1.1.1.1/`, so the control task in
section 4 needs outbound internet from the sandbox; on a fully
air-gapped lab every "blocked" would pass vacuously — the control task
exists to catch exactly that.

## 3d. Exec test

With the same server running:

```sh
./scripts/e2e-exec.sh
# env: PX_SERVER, PX, TIMEOUT as above; PVE_SSH (default root@<node>,
#      empty disables the node-level checks)
```

It exercises: `px exec` into a running task separates stdout/stderr and
propagates the exit code, arguments arrive byte-exact through both
shells (metacharacters, spaces, embedded quotes, empty argv elements),
oversized stdout is capped at 1 MiB (the suite checks the stdout side;
both streams are capped server-side) with a note on stderr,
gating returns 404 for unknown tasks and 409 for a task that is not
running (a `ProvisionFailed` task has no container, so the 409 is
deterministic), and request validation rejects a NUL byte in argv and
trailing garbage after the JSON body. Cleanup waits the asynchronous
destroy out before checking the node for leftover containers.

## 3e. Suspend/resume test

With the same server running (single-node or cluster mode both
exercise the same node-level freeze path):

```sh
./scripts/e2e-suspend.sh
# env: PX_SERVER, PX, TIMEOUT as above; PVE_SSH (default root@<node>,
#      empty disables the node-level checks)
```

It exercises: suspend lands `Suspended` with the container's cgroup v2
cgroup.events reading `frozen 1` on the node (verified through the same
node-level path the controller uses — API pid → `/proc/<pid>/cgroup` →
`cgroup.events`, never `pct exec`, which would hang inside a frozen
cgroup), exec refuses with 409 while frozen (it would hang otherwise),
second suspend and resume-while-running are idempotent 200s, after
resume the runner is alive again (process tree and memory never left),
a Suspended task thawed out of band is re-frozen by the controller
within a few ticks (the phase is declarative, not a one-shot command),
deleting a frozen task thaws first and leaves no container, and a
terminal (`ProvisionFailed`) task refuses suspend.

## 3f. Multi-node test

With a px-server running in **cluster mode** (no `-pve-node`, node
names mapped via `-ssh-host-override`):

```sh
./scripts/e2e-multi-node.sh
# env: PX_SERVER, PX, TIMEOUT as above; PVE_SSH (default
#      root@192.168.2.100, empty disables node checks);
#      NODE_HOST_MAP (default mirrors the -ssh-host-override example);
#      TEMPLATE_NODES (default "third second" — nodes holding the image)
```

It exercises: the task schedules onto an online, template-bearing node
(`px get tasks` carries a NODE column; nodes without the template are
never picked), the NODE column is stable across polls, the container
really exists on that node (resolved through the same node→host map),
exec and logs cross the node boundary over the lazy per-node SSH
connection, suspend freezes and resume unfreezes the task's cgroup on
its own node, and delete removes the container from that same node.
The scheduling order itself (most free memory first) is environment
dependent and not asserted — pinning it deterministically needs
control over each node's load, which is what the unit tests cover with
fake cluster views.

## 4. Restart-recovery check (manual)

Crash safety is the part unit tests can only simulate, so watch it
happen once:

1. **Mid-provisioning kill**: apply a task and immediately `kill -9`
   px-server while its phase is `Provisioning`. Restart px-server.
   The controller re-observes the half-provisioned task: if the clone
   got far enough to boot (`/run/px/booted` exists) it adopts the
   container and the task continues to `Running`; otherwise it fails
   the task and destroys the partial clone. No orphan containers
   should remain on the node (`pct list`).
2. **Delete-across-restart**: apply a long-running task, mark it for
   deletion (`px delete task NAME`), and `kill -9` px-server before
   the destroy completes. Restart: the deletion timestamp is
   persisted in SQLite, so the controller resumes and finishes the
   destroy.
3. **Suspend-across-restart**: suspend a running task, wait for
   `Suspended`, and restart px-server. The freeze lives in the
   container's cgroup (not in px-server's memory), so the task stays
   frozen, and the controller re-verifies it on its next ticks —
   `Suspended` stays `Suspended`, `px resume` brings it back.

## Troubleshooting

- `401` from PVE: token secret malformed or token disabled. The token
  string is `user@realm!tokenid=secret` — the secret is the part after
  the last `=`.
- Boot never completes: check the container console (`pct console
  <CTID>`) and `/run/px/task.log` inside it; the boot script writes
  runner output there.
- Template not found at clone: the `image` field must be the template
  **name** (`px-runner-debian12`), not the CTID.
