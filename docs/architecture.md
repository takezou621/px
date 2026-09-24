# px architecture

Status: draft v1 (2026-09-25)

## Positioning

| | google/ax | px |
|---|---|---|
| Target scale | billions of agent tasks, clusters | tens of tasks, one PVE cluster |
| Control plane | Go services + Redis, on K8s | single Go binary + SQLite |
| Sandbox substrate | Agent Substrate | Proxmox VE LXC (link-clone from template) |
| API | `ax.io/v1alpha1`, gRPC | `px.io/v1alpha1`, plain HTTP/JSON |
| CLI | `ax` (apply/get/describe/ssh/...) | `px` (same verbs, fewer) |

px borrows ax's *conceptual model* (declarative manifests, four primitives,
kubectl-style verbs) and deliberately discards its *operational weight*.

## Components

```
┌──────────┐   HTTP/JSON   ┌────────────────────────────┐   PVE API (8006)
│  px CLI  │ ────────────▶ │  px-server                 │ ───────────────▶ Proxmox VE
└──────────┘               │  ├─ REST API               │
                           │  ├─ task controller        │        LXC template
                           │  │   (reconcile loop)      │        └─link-clone─▶ CT 142 ...
                           │  └─ store (SQLite, WAL)    │
                           └────────────────────────────┘
```

### px-server

- **API**: REST, JSON in/out. Endpoints mirror the CLI verbs:
  `POST /v1/apply`, `GET /v1/tasks`, `GET /v1/tasks/{name}`, `DELETE /v1/tasks/{name}`,
  `GET /v1/tasks/{name}/logs`. No gRPC — one less codegen dependency, curl-debuggable.
- **Controller**: single reconcile loop. Polls SQLite for non-terminal tasks,
  drives them toward the desired phase by calling the Proxmox client.
  (No event bus; PVE is the source of truth for container state.)
- **Store**: SQLite in WAL mode. Tables: `tasks`, `workspaces`. Status is
  written back from reconcile results.

### Task lifecycle

```
Pending ──▶ Provisioning ──▶ Running ──▶ Succeeded | Failed
                │  (link-clone template,     │
                │   start CT, wire goal)     └─ logs streamed from CT console
                ▼
             Failed (PVE error)
```

- **Provisioning**: `POST /nodes/{node}/lxc` with `clone=1` from the template
  VMID, then `start`. Resource limits map 1:1 to LXC `cores`/`memory`.
- **Running**: the runner command (e.g. `claude -p "$GOAL"`) is executed inside
  the container via `pct exec` semantics; its exit code decides
  Succeeded/Failed.
- **Cleanup**: `ttlSecondsAfterFinished` — controller deletes the CT (and
  optionally keeps the workspace git checkout) after TTL.

### Why LXC and not Docker/VM

- LXC is Proxmox-native: one API surface for lifecycle, snapshots, console,
  and firewall. No extra daemon to install on nodes.
- link-clone from a template is copy-on-write: boot in seconds, cheap at tens
  of concurrent tasks (the px target scale).
- Stronger isolation than a shared Docker daemon; far cheaper than a VM per task.

### Template

`template/runner/` ships a script that builds the runner LXC template
(Debian + agent CLIs + px agent shim). Equivalent of ax's `Dockerfile.task-runner`.
The shim inside the template:
1. reads the goal from a task-specific mount/env,
2. clones the declared workspaces,
3. execs the runner command,
4. writes exit status to a well-known file the controller polls.

## Security model (staged)

1. **M1**: resource limits only (cores/memory), rootless-by-config runner user.
2. **M2**: Gateway kind — egress allowlist enforced via LXC firewall rules on
   the PVE node.
3. **M3**: per-task unprivileged CT (default), optional privileged for
   docker-in-sandbox use cases.

## Non-goals

- Multi-cloud / non-Proxmox backends.
- Fighting ax for large-cluster orchestration.
- A custom agent framework: px runs *existing* agent CLIs, it does not
  implement agents.
