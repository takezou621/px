# px

**Lightweight agent orchestration on Proxmox VE.**

English | [日本語](README.ja.md)

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
4. **Sandbox by default.** Tasks get resource limits and, with a Gateway
   attached, an egress fence. Agent code is untrusted until proven otherwise.

## Primitives

Same mental model as ax, own API group (`px.io/v1alpha1`):

| Kind | Role |
|---|---|
| **Task** | Run agent code in a sandboxed LXC container with CPU/memory limits |
| **Workspace** | Wire a git repository into the task's container |
| **Gateway** | Restrict egress traffic to an allowlist |
| **Model** | Configure the LLM credentials a task may use |

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
    # $GOAL is an env var set from the workspaces' goals. Because commands run
    # in exec form (each argv element is passed literally), wrap in a shell to
    # expand variables.
    command: ["bash", "-lc", "IS_SANDBOX=1 claude -p \"$GOAL\" --dangerously-skip-permissions"]
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

$ px watch
fix-bug-123 Provisioning -> Running
fix-bug-123 Running -> Succeeded

$ px logs -f fix-bug-123

$ px exec fix-bug-123 -- ps aux     # peek into the live sandbox

$ px suspend fix-bug-123            # freeze the container (cgroup v2)
$ px resume fix-bug-123             # unfreeze it
```

## Running an agent

`px run` is the one-command path: it turns flags into a Task manifest,
applies it, and follows the logs until the task goes terminal
(Ctrl-C detaches — the task keeps running):

```console
$ px run -model my-claude -workspace myapp="Fix bug #123" "make the tests pass"
```

It assumes the `px-agent-debian12` template
(`template/agent/build.sh` — the runner template plus the Claude Code
CLI). The default runner command is
`IS_SANDBOX=1 claude --dangerously-skip-permissions -p "$GOAL"`: the
sandbox is the isolation boundary, not the CLI's permission prompts
(see [docs/threat-model.md](docs/threat-model.md)). Pass
`-- COMMAND...` to run something else.

## Quick start (target)

```console
$ go install ./cmd/px ./cmd/px-server
$ px-server \
    --pve-endpoint https://pve.example.com:8006 \
    --pve-token PVE@px=<token> \
    --pve-node pve1
$ px apply -f examples/hello-task.yaml
```

Omit `--pve-node` to run against a whole PVE cluster: tasks schedule
onto whichever online node holds the image and has the most free
memory (`--ssh-host-override "node1=10.0.0.1,node2=10.0.0.2"` maps
PVE node names to SSH hosts, since node names are not DNS names).

## Documentation

- [docs/onboarding.md](docs/onboarding.md) — start here: a 30-minute
  tour from clone to first contribution.
- [docs/architecture.md](docs/architecture.md) — how the pieces fit
  together, as built.
- [docs/roadmap.md](docs/roadmap.md) — milestone-by-milestone design
  record: what exists, why, and what was explicitly not done.
- [docs/e2e.md](docs/e2e.md) — real-node verification, per script.
- [docs/threat-model.md](docs/threat-model.md) — the sandbox boundary
  and the trade-offs px accepts.

## Status

Pre-alpha. The API (`px.io/v1alpha1`) will change. See
[docs/roadmap.md](docs/roadmap.md) for what exists and what's next.

Known limitations on the current milestone:

- The HTTP API is unauthenticated unless `px-server` runs with
  `-token-file <file>` (then every request needs `Authorization: Bearer`, and
  clients pass the value via `-token` / `PX_TOKEN`). Either way the server
  binds to `127.0.0.1` by default — don't expose the port without TLS.
- SSH host keys are accepted unverified unless `px-server` runs with
  `-ssh-host-key <file>` (pinned public host keys, one per line — generate a
  pin file with `ssh-keyscan -t ed25519 <host>`).
- `px delete` is asynchronous: the record and container are gone by the next
  reconcile tick (~2s), after the container has been destroyed. A failed
  destroy is retried until it succeeds, so records never orphan a container.

## License

Apache-2.0 (matching google/ax, the project that inspired this one).
