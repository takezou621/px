# px onboarding

Welcome. This document gets you from clone to first contribution in
about 30 minutes. It is a map, not the territory: every section names
the file that owns the detail. Read order: this page → the files it
points at → [roadmap.md](roadmap.md) when you need a design decision's
"why".

## 1. What px is (3 min)

px runs autonomous AI agent workloads (Claude Code, Codex CLI,
OpenCode, ...) in ephemeral Proxmox LXC sandboxes, declared in plain
YAML and driven by a kubectl-style CLI. It is for a homelab or a small
team's Proxmox box — the opposite end of the spectrum from
[google/ax](https://github.com/google/ax), whose conceptual model px
borrows while discarding its operational weight.

Four design axes, from `CLAUDE.md` — every implementation decision
should be checked against them:

1. **One binary** — `px-server` embeds SQLite (WAL). No external DB,
   no message bus, no gRPC.
2. **Proxmox is the substrate** — sandboxes are LXC link-clones from a
   template. Docker/K8s are not brought in.
3. **ax-compatible mental model** — declarative manifests
   (`px.io/v1alpha1`), four primitives + Schedule, kubectl-style verbs.
4. **Minimal dependencies** — net/http for REST, modernc.org/sqlite
   (CGO-free), x/crypto for SSH. If a feature needs a new dependency,
   that is a design conversation first.

Non-goals (deliberate): Kubernetes backend, gRPC API, multi-cloud,
a custom agent framework. px runs *existing* agent CLIs; it does not
implement agents.

## 2. The big picture (5 min)

```
px CLI ──HTTP──▶ px-server (single binary) ──PVE API──▶ Proxmox VE
                   ├─ REST API              │            └─ LXC sandboxes
                   ├─ controller (2s tick)  └─ node SSH ─▶ pct exec / cgroup freeze
                   └─ store (SQLite, WAL)
```

**The five kinds** (`internal/apis/v1alpha1/types.go`): Task (run agent
code in a sandbox), Workspace (wire a git repo in), Gateway (egress
allowlist via LXC firewall), Model (LLM credentials injected as env),
Schedule (cron that stamps out Tasks — Kubernetes CronJob semantics:
missed fires while down compress to one).

**Task lifecycle** — every phase transition is driven by the reconcile
loop against the declarative record:

```
Pending ──▶ Provisioning ──▶ Running ──▶ Succeeded | Failed
                │  (clone → verify unprivileged →      │
                │   gateway fence → session restore    │
                │   → boot → poll exit code)           ▼
                ▼                              TTL cleanup / delete
        ProvisionFailed                        (capture session first)
Running ⇄ Suspending ⇄ Suspended ⇄ Resuming   (cgroup-v2 freeze,
                                                declarative like delete)
```

Three properties to internalize before reading code:

- **Declare, reconcile.** `spec` is desired state; the controller polls
  every 2s and drives reality toward it. Suspend and port forwards are
  re-enforced every tick, so an out-of-band thaw or a killed socat is
  repaired, not just detected.
- **Apply is control-plane-only.** Applying never talks to PVE (the
  kubectl precedent: apply does not verify the image exists). Unknown
  images, missing templates, deleted references resolve to
  `ProvisionFailed` at provision time, with the reason named on the
  record.
- **Status is rewritten every tick.** Anything heavy or
  once-per-lifetime lives in its own SQLite table, not Task status
  (`sessions`, `events`). Status carries flags and pointers only.

**Where state lives**: seven SQLite tables (`tasks`, `workspaces`,
`models`, `gateways`, `schedules`, `sessions`, `events`) in one file;
PVE remains the source of truth for container state (px re-observes,
never caches); the runner's conversation lives as files inside the
container until capture. Full boundary map: [threat-model.md](threat-model.md).

## 3. Code tour (10 min)

~17k lines of non-test Go, 17 files. Biggest first:

| Package | Files | What it does | Start at |
|---|---|---|---|
| `internal/controller` | provisioner.go (1412), controller.go (1244), schedule.go (182) | the reconcile loop and everything it does to PVE | `Controller.Run` in controller.go |
| `cmd/px` | main.go (1151) | the whole CLI: apply/get/describe/logs/exec/run/watch | `main`, then one verb |
| `internal/store` | store.go (858) | SQLite: 7 tables, the Mark\* single-statement json_set discipline | the schema block at the top |
| `internal/server` | server.go (841) + metrics/watch/auth | REST routing, handlers, NDJSON watch, bearer auth | the mux in `New` |
| `internal/apis/v1alpha1` | types.go (632), manifest.go (258), cron.go (188), templates.go | the vocabulary: kinds, phases, validation caps, cron parser | types.go |
| `internal/sshexec` | exec.go (547) | node SSH executor: pinning, redial, one-shot vs retry paths | the `Executor` type |
| `internal/proxmox` | client.go (431) | PVE REST client (token auth, clone/start/stop/delete, exitstatus) | `Client` |
| `cmd/px-server` | main.go (204) | flags, wiring, fail-fast dial at boot | `main` |

The best way in is to follow one task's life:

1. `types.go` — the vocabulary (kinds, phases, validation caps).
2. `manifest.go` — YAML in, validated types out; multi-doc apply.
3. `server.go` `handleApply` → `store` — how a record is born
   (apply-batch in one transaction, secret redaction, placeholder
   rejection).
4. `controller.go` `Run` → `reconcileAll` — the 2s tick; quota gate →
   provision → poll → terminal settle → TTL → session capture →
   schedule fire.
5. `provisioner.go` `provision` — the long one: template resolve,
   node pick (cluster mode), link-clone, unprivileged verification,
   gateway fence, port forwards, session restore, base64 boot script,
   `booted` marker probe. This file owns most of the failure reasons
   you will see in `ProvisionFailed`.
6. `sshexec` and `proxmox` — the two transports underneath (SSH for
   `pct exec`/cgroup freeze, REST for lifecycle). Note the split:
   config-ish writes go over the PVE API (no quoting pitfalls), exec
   goes over SSH — that boundary is deliberate.

Two conventions you will meet everywhere:

- **Boot-script embedding**: every input the runner needs crosses the
  two shells (SSH → `pct exec`) as one base64 blob — no quoting
  pitfalls, no `MAX_ARG_STRLEN` surprises. Session restore is the
  exception that proves it: megabytes do not fit in argv, so it writes
  in chunked base64.
- **Destroy guards**: px only ever destroys a container whose hostname
  is `px-<task>` (`DestroyOwned`/`ErrNotOwned`) — a reused VMID is
  never px's to destroy. Reads of "not found" are structural
  (`IsNotFound`), never substring matches.

## 4. The development loop (5 min)

```sh
go build ./...      # build; binaries: go build -o px ./cmd/px && go build -o px-server ./cmd/px-server
go test ./...       # unit tests — no PVE required, everything faked
go vet ./...
```

- **Unit tests** run against fakes (an in-process PVE HTTP server, an
  in-process SSH server, a temp-file SQLite store). If your feature can be
  pinned by a test, it goes here first.
- **Real-node E2E** is a milestone-closing step, not optional — several
  bugs (`pct exec` PATH, pve-firewall timing, yaml tag casing) existed
  only there. Fourteen scripts under `scripts/e2e-*.sh`; the setup,
  what each pins, and the troubleshooting table are in
  [e2e.md](e2e.md). Read §3 (smoke) even if you never run it — it is
  the fastest tour of what the system actually does.
- **Process**: update [roadmap.md](roadmap.md) *before* implementing
  (it is the design record, and the next reader finds the why there);
  after landing a non-trivial change, run the dual-agent review (an
  independent Codex pass + an independent Claude pass over the same
  diff, evaluated by the author — fixes are adopted on merit, and the
  adopted/rejected list is recorded in the milestone section).

## 5. Where to dig deeper (5 min)

- [roadmap.md](roadmap.md) — the design record. Each milestone states
  its problem, the chosen design, the explicit "not doing" list, and
  what the reviews caught. M13 (quota) is the most recent and reads as
  a worked example.
- [e2e.md](e2e.md) — real-node verification, per-script.
- [threat-model.md](threat-model.md) — the accepted trade-offs (LXC
  shared kernel, DNS-tunnel exfil through the implicit DNS allow, the
  image-init window ahead of the egress gate). Read it before touching
  anything security-adjacent.
- [template/README.md](../template/README.md) — the contract any LXC
  template must satisfy to be cloneable by px.

A good first contribution: run the smoke E2E on a real node, then read
`provisioner.go`'s `provision` alongside the boot script it generates
(`template/runner/`). The gap between "a YAML manifest" and "a frozen
Debian container running Claude" is the heart of the system, and it is
all in those two places.
