# px

**Lightweight agent orchestration on Proxmox VE.**

px runs autonomous AI agent workloads (Claude Code, Codex CLI, OpenCode, ...) in
ephemeral Proxmox LXC sandboxes, declared in plain YAML and driven by a
`kubectl`-style CLI — with no Kubernetes, no Redis, no external control plane.

Inspired by [google/ax](https://github.com/google/ax). Where ax targets
billion-agent clusters on Kubernetes + Agent Substrate, px targets the other end
of the spectrum: a homelab or a small team's Proxmox box, a single Go binary,
and an embedded SQLite store.

```
px CLI ──HTTP──▶ px-server (single binary, SQLite) ──PVE API──▶ Proxmox VE
                                                                    └─ LXC sandboxes
```

## Design principles

1. **One binary.** `px-server` embeds its datastore (SQLite/WAL). `px` is a
   static client. No cluster, no message bus, no service mesh.
2. **Proxmox is the substrate.** Sandboxes are LXC containers link-cloned from
   a template: copy-on-write, seconds to boot, kernel-level isolation.
3. **Declare, don't script.** Workloads are YAML manifests (`px.io/v1alpha1`),
   applied and inspected with `px apply` / `px get` / `px describe`.
4. **Sandbox by default.** Tasks get resource limits and (eventually) network
   fences. Agent code is untrusted until proven otherwise.

## Primitives

Same mental model as ax, own API group (`px.io/v1alpha1`):

| Kind | Role |
|---|---|
| **Task** | Run agent code in a sandboxed LXC container with CPU/memory limits |
| **Workspace** | Wire a git repository into the task's container |
| **Gateway** | Restrict egress traffic to an allowlist *(planned)* |
| **Model** | Configure the LLM credentials a task may use *(planned)* |

## Example

```yaml
apiVersion: px.io/v1alpha1
kind: Workspace
metadata:
  name: myapp
spec:
  git:
    repo: https://github.com/example/myapp.git
    branch: main
---
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: fix-bug-123
spec:
  image: px-runner-debian12        # LXC template on your PVE node
  workspaces:
    - name: myapp
      goal: "Fix bug #123 and make the tests pass"
  runner:
    command: ["claude", "-p", "$GOAL", "--dangerously-skip-permissions"]
  resources:
    cores: 4
    memoryMB: 8192
  ttlSecondsAfterFinished: 3600
```

```console
$ px apply -f task.yaml
workspace.px.io/myapp created
task.px.io/fix-bug-123 created

$ px get tasks
NAME          PHASE    CONTAINER   AGE
fix-bug-123   Running  142         5s

$ px logs -f fix-bug-123
```

## Quick start (target)

```console
$ go install ./cmd/px ./cmd/px-server
$ px-server --listen :7420 \
    --pve-endpoint https://pve.example.com:8006 \
    --pve-token PVE@px=<token> \
    --pve-node pve1
$ px apply -f examples/hello-task.yaml
```

## Status

Pre-alpha. The API (`px.io/v1alpha1`) will change. See
[docs/roadmap.md](docs/roadmap.md) for what exists and what's next.

## License

Apache-2.0 (matching google/ax, the project that inspired this one).
