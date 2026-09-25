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

## M2 — Workspaces & lifecycle polish

- [ ] Workspace kind: git clone into the container before runner exec
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
- [ ] API token auth (px-server is unauthenticated today; loopback-only by default)
- [ ] `px watch` (phase transition streaming)

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
