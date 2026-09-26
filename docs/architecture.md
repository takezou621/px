# px architecture

Status: current (2026-09-27, through M13). For the design record behind
each decision, see [roadmap.md](roadmap.md); for a guided tour of the
code, [onboarding.md](onboarding.md).

## Positioning

| | google/ax | px |
|---|---|---|
| Target scale | billions of agent tasks, clusters | tens of tasks, one PVE cluster |
| Control plane | Go services + Redis, on K8s | single Go binary + SQLite |
| Sandbox substrate | Agent Substrate | Proxmox VE LXC (link-clone from template) |
| API | `ax.io/v1alpha1`, gRPC | `px.io/v1alpha1`, plain HTTP/JSON |
| CLI | `ax` (apply/get/describe/ssh/...) | `px` (same verbs, fewer) |

px borrows ax's *conceptual model* (declarative manifests, four primitives
plus Schedule, kubectl-style verbs) and deliberately discards its
*operational weight*.

## Components

```
┌──────────┐  HTTP/JSON   ┌─────────────────────────────┐ PVE API (8006)
│  px CLI  │ ───────────▶ │  px-server (single binary)  │ ─────────────▶ Proxmox VE
└──────────┘              │  ├─ REST API                │                 └─ LXC sandboxes
                          │  ├─ controller (2s tick)    │
                          │  └─ store (SQLite, WAL)     │ node SSH (22)
                          └─────────────────────────────┘ ─────────────▶ pct exec /
                                                                          cgroup freeze
```

### px-server

- **API**: REST, JSON in/out, bearer-token auth (loopback bind by
  default). One group of endpoints per kind, mirroring the CLI verbs:
  `POST /v1/apply` (multi-doc, one transaction),
  CRUD on `/v1/tasks`, `/v1/workspaces`, `/v1/models`, `/v1/gateways`,
  `/v1/schedules`, `/v1/sessions`, plus task `logs`/`events`/`exec`/
  `suspend`/`resume`, schedule `suspend`/`resume`, `GET /v1/templates`,
  `GET /v1/metrics`, and an NDJSON `GET /v1/watch`. No gRPC — one less
  codegen dependency, curl-debuggable.
- **Controller**: a single reconcile loop ticking every 2s over
  non-terminal records. Each tick: schedule fires → quota gate →
  provision → poll running tasks → settle terminal states → TTL
  cleanup → suspend/resume enforcement. Suspend and port forwards are
  re-enforced every tick, so out-of-band drift is repaired, not just
  detected. No event bus; PVE is the source of truth for container
  state (px re-observes, never caches).
- **Store**: SQLite in WAL mode. Seven tables: `tasks`, `workspaces`,
  `models`, `gateways`, `schedules`, `sessions`, `events`. Status is
  written back from reconcile results with single-statement
  `json_set` updates (`Mark*` helpers) — no read-modify-write races.
  Anything once-per-lifetime (session captures, events) gets its own
  table, not a slot in Task status.

### Task lifecycle

```
Pending ──▶ Provisioning ──▶ Running ──▶ Succeeded | Failed
                │  (resolve template → pick node →    │
                │   link-clone → verify unprivileged  │
                │   → gateway fence → port forwards   │
                │   → session restore → boot script   ▼
                │   → poll `booted` marker)    TTL cleanup / delete
                ▼                              (capture session first)
        ProvisionFailed
Running ⇄ Suspending ⇄ Suspended ⇄ Resuming   (cgroup-v2 freeze,
                                                declarative like delete)
```

- **Provisioning**: link-clone from the template VMID
  (`POST /nodes/{node}/lxc?clone=1`), then `start`. In cluster mode the
  node is picked per task: among online nodes that hold the image and
  are under the per-node cap, the one with the most free memory wins.
  Resource limits
  map 1:1 to LXC `cores`/`memory`. Any failure here names its reason on
  the record (`ProvisionFailed`); apply never talks to PVE, so unknown
  images and deleted references surface here, not at apply time.
- **Running**: the px agent shim inside the container clones declared
  workspaces, applies the gateway egress allowlist, and execs the
  runner command; its exit code decides Succeeded/Failed. `logs` streams
  the CT console; `exec` runs commands via SSH + `pct exec`.
- **Suspending**: `cgroup v2` freezer on the node over SSH. Declarative
  like delete: `spec.suspend: true` converges to `Suspended` and
  `resume` un-converges it; state survives reboots, a killed server
  picks the drift up on the next tick.
- **Cleanup**: `ttlSecondsAfterFinished` — the controller deletes the
  CT after TTL, capturing the runner conversation into a named session
  first. Delete follows the same discipline: capture, then destroy.

### Why LXC and not Docker/VM

- LXC is Proxmox-native: one API surface for lifecycle, snapshots,
  console, and firewall. No extra daemon to install on nodes.
- link-clone from a template is copy-on-write: boot in seconds, cheap
  at tens of concurrent tasks (the px target scale).
- Stronger isolation than a shared Docker daemon; far cheaper than a
  VM per task.

### Template

`template/runner/` ships a script that builds the runner LXC template
(Debian + agent CLIs + px agent shim). Equivalent of ax's
`Dockerfile.task-runner`. The shim inside the template:

1. reads its full input (goal, env, workspaces, session payload) as one
   base64 blob from the boot script — every value crosses SSH and
   `pct exec` without ever entering a shell's quoting logic;
2. clones the declared workspaces and applies the egress rules;
3. touches the `booted` marker (the controller polls for it);
4. execs the runner command;
5. writes exit status to a well-known file the controller polls.

The template contract (users, packages, `px-runner` identity) is in
`template/README.md`.

## Security model

- **Isolation**: every CT is unprivileged — the template is built
  unprivileged and each clone is verified unprivileged before start.
- **Egress**: Gateway kinds enforce an allowlist via LXC firewall rules
  on the PVE node; a task without a gateway has no network by default.
- **Control plane**: px-server binds loopback by default and expects a
  bearer token (`-token-file`). Model credentials are stored redacted
  and injected as env only inside the sandbox.
- **Ownership**: px only destroys containers whose hostname is
  `px-<task>`; a reused VMID is never px's to destroy.

Accepted trade-offs (LXC shared kernel, DNS-tunnel exfiltration through
the implicit DNS allow, the image-init window before the egress gate)
are catalogued in `docs/threat-model.md`.

## Non-goals

- Multi-cloud / non-Proxmox backends.
- Kubernetes backend, gRPC API.
- Fighting ax for large-cluster orchestration.
- A custom agent framework: px runs *existing* agent CLIs, it does not
  implement agents.

## Known limits (as of M13)

- Schedule concurrency policy is fixed (missed fires while down
  compress to one); no `forbid`/`replace` policies.
- Quota caps are cluster-wide (`-max-running-tasks`) and per-node
  (`-max-containers-per-node`); no per-user or per-namespace quotas.
- `delete session` drops the row unconditionally: a task still running
  with that capture name writes it back on its next destroy.
- `px run --continue-session` copies the last writer's spec wholesale:
  reusing the task name for an unrelated goal makes the new task
  inherit that stale spec.
- px re-observes container state every tick; a container deleted
  out-of-band is noticed within one reconcile interval, not instantly.
