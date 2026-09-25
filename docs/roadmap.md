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
- [ ] E2E against a real PVE node (blocks M1 sign-off)

## M2 — Workspaces & lifecycle polish

- [ ] Workspace kind: git clone into the container before runner exec
- [ ] API token auth (px-server is unauthenticated today; loopback-only by default)
- [ ] `px watch` (phase transition streaming)

## M3 — Sandbox hardening

- [ ] SSH host key pinning (today the first-seen key is accepted unverified)
- [ ] Destroy guards by container name: PVE nextid is a suggestion, not a reservation, so a VMID recorded after a crash mid-provision could in theory collide with an externally created container — verify `px-<task>` name ownership before destroy
- [ ] Gateway kind: egress allowlist via LXC firewall
- [ ] Model kind: LLM credentials injected as per-task env/secrets
- [ ] unprivileged CT default; docs on threat model

## M4 — Beyond one node

- [ ] Multi-node scheduling (PVE cluster, pick node by free resources)
- [ ] `px suspend` / `px resume` (PVE snapshot / CT freeze)
- [ ] `px exec` (debug shell into a running task)

## Explicitly deferred

- Kubernetes backend, gRPC API, multi-cloud — see architecture doc non-goals.
