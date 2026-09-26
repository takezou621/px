# px templates

px clones every task's sandbox from an LXC template on the PVE node —
`spec.image` names one, the provisioner finds it by name (same name on
several nodes is fine: scheduling picks a node that holds it). PVE storage
is the registry; px keeps no template list of its own, it discovers
templates through the cluster view and checks them against the contract
below.

## The contract

What px requires of any LXC template it clones. The first two are
API-visible — `px get templates` verifies them and `px describe template
NAME` says which one fails. The rest live inside the template and only
surface at provision.

| # | Requirement | Verified how |
|---|-------------|--------------|
| 1 | Created unprivileged (`--unprivileged 1`) | `px get templates`; also re-checked after every clone — a privileged template fails the provision instead of shipping |
| 2 | `net0` carries `ip=dhcp` | `px get templates`; px resolves the address in-CT after boot and cannot wait for a static config |
| 3 | `/bin/sh` exists (POSIX sh, dash is fine) | provision time — the boot script runs under it |
| 4 | Writable `/run` (tmpfs is fine) | provision time — px creates `/run/px` itself for goal, command, logs and the boot marker |
| 5 | The user in `spec.runner.user` exists (default root) | provision time |

Sizing (`cores`, `memory`, `rootfs`) is not part of the contract: a task's
`spec.resources` is applied after the clone, so the template's values are
just defaults.

An **agent** template is a runner template plus whatever the agent CLI
needs. The default agent command (`px run` without `--`) runs
`claude --dangerously-skip-permissions`, so the agent template carries the
Claude Code CLI, git, and enough toolchain for real work — that part is
optional: a task that sets its own runner command needs none of it, which
is how the fake-session E2E runs on the plain runner template.

## Building your own

The two shipped build scripts are the reference:

- `template/runner/build.sh` — Debian 12 + bash, git, curl; result:
  `px-runner-debian12`
- `template/agent/build.sh` — the runner plus the Claude Code CLI
  (native release binary, checksum-verified); result: `px-agent-debian12`

Both run on the PVE node (or via ssh) as root:

```sh
scp template/agent/build.sh root@<node>:/root/px-agent-build.sh
ssh root@<node> /root/px-agent-build.sh <CTID>   # a free container id
```

For your own: adapt a script (swap the apt layer, keep the
create-with-`--unprivileged 1` + `ip=dhcp` then `pct stop && pct template`
shape), or craft one by hand with `pct create`/`pct template`. Name it
anything — `spec.image` matches by name. Then:

```sh
px get templates                 # your template should list with PX-OK true
px describe template <name>      # facts and any failed requirement
```

A template that lists PX-OK false is still clonable from PVE, and a task
naming it will still be scheduled — the verdict is a pre-flight check, not
a gate. Apply stays control-plane-only: a task naming an unknown image is
rejected at provision (`ProvisionFailed`, reason names the template), the
same way kubectl applies before an image exists.
