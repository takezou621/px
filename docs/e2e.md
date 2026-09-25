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
# optional args: /root/build.sh <CTID> <bridge> <storage>
# defaults:      vmbr0, local-lvm
```

This downloads the Debian 12 standard image, creates an unprivileged
container with `nesting=1`, installs the base tools, and converts it to
the template `px-runner-debian12`. Reference that name as `spec.image`
in Task manifests.

## 2. Start px-server

```sh
export PX_PVE_ENDPOINT="https://<PVE_HOST>:8006"
export PX_PVE_NODE="<NODE_NAME>"
export PX_PVE_TOKEN="root@pam!px=<SECRET>"    # or your user@realm!tokenid

go run ./cmd/px-server
# flags: -listen 127.0.0.1:7420 -db px.db -pve-endpoint ... -pve-node ...
#        -pve-token ... -ssh-user root -ssh-key <path> -reconcile-interval 2s
```

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

What you should see while it runs (from another terminal):

```sh
./px get tasks                 # phase transitions every reconcile tick
./px describe task e2e-ok      # full status JSON
./px logs -f e2e-ok            # tail runner output
```

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
