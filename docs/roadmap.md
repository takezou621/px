# px roadmap

Status: 2026-09-25. Owner: Claude (acting PO).

## M0 — Foundation (done)

- [x] Vision, architecture doc, repo layout
- [x] `px.io/v1alpha1` types (Task, Workspace)
- [x] Manifest parsing + validation

## M1 — Single task on one node (MVP)

Goal: `px apply` a Task and watch it run in an LXC container.

- [x] Proxmox VE API client (token auth, clone/start/stop/delete, task exitstatus verification)
- [x] Task controller reconcile loop (Pending→Provisioning→Running→Succeeded/Failed/ProvisionFailed)
- [x] Runner boot via node SSH + `pct exec`, base64-embedded inputs, `runner.user` support
- [x] px-server: REST API + SQLite store (413 body cap, atomic create, loopback default)
- [x] px CLI: `apply`, `get tasks`, `describe task`, `logs -f`, `delete task`
- [x] `template/runner/`: script to build the LXC runner template
- [x] Deletion: persisted `deletionTimestamp`, destroy-retry, restart-safe
- [x] Crash recovery: interrupted provisioning → adopt (booted) or fail + partial-clone cleanup; dead container → Failed
- [x] E2E against a real PVE node (PVE 9.2, node `third`): smoke 14/14 (success/failure/goal delivery/delete-running/duplicate rejection), crash-recovery paths verified live (re-provision after kill, unbooted clone destroyed with no orphans, adopted booted container resumes). Fixes surfaced by the run: LXC clone takes `hostname`, PVE string-form API errors parsed, `-tls-insecure` for PVE's self-signed cert.

## M2 — Workspaces & lifecycle polish (done)

- [x] Workspace kind: git clone into the container before runner exec
  - Design: `task.spec.workspaces[].name` references a Workspace resource
    (`spec.git.repo`, optional `spec.git.branch`). The controller resolves
    the references at provision time — an unknown name is a ProvisionFailed
    reason like any other provision error. The boot script clones each repo
    (depth 1) into `/workspace/<name>` before spawning the runner, so a
    clone failure dies before the `booted` marker and rides the existing
    provision-failure path (partial clone destroyed, no orphans). A
    Workspace is declarative config, not a reconciled object: apply upserts
    it, there is no status; one apply batch commits in a single SQLite
    transaction, so the reconcile tick never sees a Task whose Workspace is
    still missing. Public repos only in M2 — credentials belong to
    Model (M3). The runner starts in the container's root directory —
    commands that work inside a repo must `cd /workspace/<name>` explicitly
    (no implicit cd: surprise-free is better than convenient). Manifest
    caps cover workspace values too (MaxWorkspaces/MaxRepoBytes/
    MaxBranchBytes), keeping the boot command within SSH's MAX_ARG_STRLEN.
    Server: GET /v1/workspaces and /v1/workspaces/{name}; CLI:
    `px get workspaces`, `px describe workspace NAME`.
- [x] API token auth (px-server is unauthenticated today; loopback-only by default)
  - Design: opt-in static bearer token — `px-server -token-file F` requires
    `Authorization: Bearer <t>` on every route except `/healthz` (constant-time
    compare, 401 with `WWW-Authenticate`). Without the flag the loopback-only
    default stays, so M1/M2 workflows keep working. The CLI sends the token
    from `-token` / env `PX_TOKEN`; a 401 prints a hint about PX_TOKEN. The
    token file lives outside the repo (0600), never logged.
- [x] `px watch` (phase transition streaming)
  - Design: `GET /v1/watch` streams NDJSON snapshots
    (`{"tasks": [...], "workspaces": [...]}`) — first one immediately on
    connect, then one per second until the client disconnects (plain
    chunked net/http, no SSE/WebSocket dependency). `px watch` prints only
    phase transitions (`ws-order Pending -> Running`), treats a task
    disappearing from a snapshot as `Deleted`, and exits when every task is
    terminal (Succeeded/Failed/ProvisionFailed) or marked for deletion —
    so scripts and the E2E can just `px watch` to completion.
- [x] E2E against the real PVE node (token auth on/off): 401 without/with a
    wrong token and 200 with the right one; Workspace-first apply ran a task
    to `Succeeded` with `px watch` detecting terminal in ~5s (`WS_ORDER_OK`
    in logs); delete surfaced as `Succeeded -> Deleted`; no containers left
    on the node. Dual-agent review fixes: first watch snapshot loads before
    the 200 commits (store errors are a 500, not an empty stream), CLI
    treats HTTP errors and mid-stream drops as failures, frames decode with
    json.Decoder (no 1 MiB line cap), and an empty snapshot keeps waiting
    so a watch started before `px apply` still works.

## M3 — Sandbox hardening

- [ ] SSH host key pinning (today the first-seen key is accepted unverified)
- [ ] Destroy guards by container name: PVE nextid is a suggestion, not a reservation. Residual M1 window: a crash between VMID persist and Create can leave a VMID that destroy later targets after an external party reuses it; and a failed Create that clears its VMID can orphan a clone that succeeded late (PVE tasks outlive their waiter) — verify `px-<task>` name ownership before destroy
- [ ] Distinguish pct exec transient failures from "no marker" in Booted(): today any non-zero pct exec is treated as unbooted, so a transient pct failure could destroy a live runner of an already-interrupted task; re-evaluate against real pct behavior in the PVE E2E (container restarts wiping the /run tmpfs are out of scope — external intervention)
- [ ] Gateway kind: egress allowlist via LXC firewall
- [ ] Model kind: LLM credentials injected as per-task env/secrets
- [ ] unprivileged CT default; docs on threat model

## M4 — Beyond one node

- [ ] Multi-node scheduling (PVE cluster, pick node by free resources)
- [ ] `px suspend` / `px resume` (PVE snapshot / CT freeze)
- [ ] `px exec` (debug shell into a running task)

## Explicitly deferred

- Kubernetes backend, gRPC API, multi-cloud — see architecture doc non-goals.
