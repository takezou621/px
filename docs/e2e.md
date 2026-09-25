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
- The `px` and `px-server` binaries: `go build ./cmd/px ./cmd/px-server`.

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

## 3. Run the smoke test

```sh
./scripts/e2e-smoke.sh
# env: PX_SERVER (default http://127.0.0.1:7420), PX (default ./px),
#      TIMEOUT seconds per task (default 300)
```

It exercises: a task that succeeds (and its logs), a task that fails
(exit code recorded), goal delivery into the container, deleting a
running task, and duplicate-apply rejection — then cleans up its tasks.

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

## Troubleshooting

- `401` from PVE: token secret malformed or token disabled. The token
  string is `user@realm!tokenid=secret` — the secret is the part after
  the last `=`.
- Boot never completes: check the container console (`pct console
  <CTID>`) and `/run/px/task.log` inside it; the boot script writes
  runner output there.
- Template not found at clone: the `image` field must be the template
  **name** (`px-runner-debian12`), not the CTID.
