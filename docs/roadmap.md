# px roadmap

Status: 2026-09-25. Owner: Claude (acting PO).

## M0 — Foundation (this repo, now)

- [x] Vision, architecture doc, repo layout
- [x] `px.io/v1alpha1` types (Task, Workspace)
- [x] Manifest parsing + validation

## M1 — Single task on one node (MVP)

Goal: `px apply` a Task and watch it run in an LXC container.

- [ ] Proxmox VE API client (token auth, clone/start/stop/delete, console log)
- [ ] Task controller reconcile loop (Pending→Provisioning→Running→Done)
- [ ] px-server: REST API + SQLite store
- [ ] px CLI: `apply`, `get tasks`, `describe task`, `logs -f`, `delete task`
- [ ] `template/runner/`: script to build the LXC runner template

## M2 — Workspaces & lifecycle polish

- [ ] Workspace kind: git clone into the container before runner exec
- [ ] `ttlSecondsAfterFinished` cleanup
- [ ] `px watch` (phase transition streaming)
- [ ] exit-code → Succeeded/Failed mapping, failure reasons in `describe`

## M3 — Sandbox hardening

- [ ] Gateway kind: egress allowlist via LXC firewall
- [ ] Model kind: LLM credentials injected as per-task env/secrets
- [ ] unprivileged CT default; docs on threat model

## M4 — Beyond one node

- [ ] Multi-node scheduling (PVE cluster, pick node by free resources)
- [ ] `px suspend` / `px resume` (PVE snapshot / CT freeze)
- [ ] `px exec` (debug shell into a running task)

## Explicitly deferred

- Kubernetes backend, gRPC API, multi-cloud — see architecture doc non-goals.
