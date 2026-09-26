# Threat model

What px trusts, what it isolates, and where the lines are deliberately
drawn. Read this before exposing px-server beyond loopback or running
untrusted code in tasks.

## Trust model

Three parties:

- **The operator** runs px-server and owns the PVE node. px assumes the
  operator is root-equivalent; it defends tasks *from each other and
  from the outside*, not the operator from px.
- **The task** is the untrusted party: its command, its workspaces
  (cloned from public git), and anything a Model provider's API returns
  to it. The sandbox exists to confine this.
- **The template** (`spec.image`) is trusted. It must be a px-built
  template (`template/runner/build.sh`) — see the egress section for
  why this is load-bearing.

## Sandbox boundary

The sandbox is an LXC container: it shares the PVE host's kernel, so it
is strictly weaker isolation than a VM, and `nesting=1` (required by the
runner) widens the kernel surface a task can reach. This is a
deliberate trade for boot speed and image size — treat container
escapes as the platform's risk budget, and put PVE's own hardening
(apparmor, unprivileged mapping) underneath, which px does not
duplicate.

Containers are **unprivileged by construction**: the build script
creates the template with `--unprivileged 1`, and every clone is
**verified** to carry the flag before it is started — the clone
endpoint cannot set the property (PVE rejects it as an unknown
parameter; the flag only inherits from the source container), so px
reads the clone's config and fails the provision if it came out
privileged. Inside the container, uid 0 maps to an unprivileged host
uid, so a task that escapes the container does not land as host root.

## Network egress

**Default is full egress** — a Task without `spec.gateway` gets an
unrestricted sandbox, on the theory that px is an orchestrator, not a
firewall; restriction is opt-in through a Gateway (`policy_out=DROP`
plus allowlist).

The implicit holes and their rationale:

- **DNS (udp+tcp 53) and DHCP (udp 67) are always allowed.** Nearly
  every real egress is name-based, and the DHCP lease must survive the
  firewall. The cost is real: **a task can exfiltrate arbitrary data
  through DNS tunneling** (or through any allowlisted rule's
  destination). This is an accepted trade-off, not an oversight —
  closing it means hostname rules, which px rejects because apply-time
  DNS resolution goes stale (TOCTOU). If your tasks' queries leave the
  host, audit the resolver side instead.
- **The egress gate holds only the px runner.** PVE programs the
  firewall dataplane a few seconds *after* the container's veth
  appears, so the provisioner pauses the runner boot until the
  container's OUT chain is live — but the container's *init* (from the
  template image) runs during that window and can transmit. That is
  safe solely because `spec.image` is the trusted px-built template.
  **A user-supplied image would need the gate before start, which PVE
  cannot express** (the dataplane only exists once the veth does).
  Pinning the template is part of the security contract.
- Gateway rules are CIDR-only by design; hostname rules would embed the
  same TOCTOU above.

## Secrets and the database

**Model API keys live in SQLite** (`px.db`) in plaintext. That is the
single-binary axis doing its job — no external secret manager, no
KMS — and it makes the database file a credential store: px opens it
with `0600` enforced (the WAL/SHM siblings included), no matter the
umask that created it. Back it up like the credential store it is.

The API compensates where it can: keys are **write-only over the wire**
(GET/list/describe return `<redacted>` and re-applying the placeholder
is rejected), keys never appear in controller logs or provision errors,
and inside the container they land in owner-only files (`0600`) under
`/run/px/`, owned by the task's runner user (root unless
`spec.runner.user` says otherwise). A malicious task *can read its own
model's key* — from its environment, or as any process running as that
same uid inside the sandbox; that is the point of giving it one — but
cannot read another task's, since sandboxes share nothing.

## Agent runtime and skip-permissions

The default runner command (`px run` without `--`, and the
`px-agent-debian12` template) is
`IS_SANDBOX=1 claude --dangerously-skip-permissions -p "$GOAL"`. The
name of that flag reads alarming, and the choice is deliberate: a
permission prompt needs a human inside the container to answer it, and
there is none — the task is autonomous by definition. `IS_SANDBOX=1` is
the CLI's declared escape hatch for that arrangement (runners exec as
root, under which the CLI refuses the flag outright) — the environment
really is isolated here by construction. The isolation boundary is the
*container*, not the CLI's permission system: the LXC sandbox already
assumes the task is the untrusted party (see the trust model), and an
optional Gateway bounds what its egress can reach. Skipping the CLI's
prompts changes nothing about what the task could eventually do; it
only removes the fiction that someone is watching.

Two corollaries follow. First, the goal text (`spec.goal`, workspace
goals) is *instructions to the agent*, not a security boundary — a
prompt-injected goal gets exactly the authority the task already had:
its sandbox, its model key, its gateway. Second, anything the model
provider returns to the running agent is untrusted input, same as any
task output; px does not and cannot vet it. Neither is new attack
surface — both were already true of any task with a runner command —
but the first-class agent defaults make them the normal path rather
than an edge case, so they are named here.

The agent template also bakes in the CLI itself, fetched at build time
via the native installer from `claude.ai`; that is part of the
template supply chain entry under Out of scope.

## px-server exposure

px-server binds loopback and serves plain HTTP by default; the bearer
token (`-token-file`) is opt-in. The intended deployment is *on the
operator's machine or a jump host*, not a shared network — if you must
expose it, put TLS-terminating auth in front (px does none) and treat
the token as the only barrier, since every task's command is
apply-on-the-wire by design.

`px exec` raises that bar by exactly nothing and is deliberate about
it: the same token that already specifies an arbitrary runner command
at apply time can now run an arbitrary command in a running
container's sandbox, through `POST /v1/tasks/{name}/exec`. It is an
operator tool on the same trust level as apply — it runs only inside
the task's own container (never on the node shell: the argv crosses
two shells as single-quoted words), it is bounded (2-minute timeout,
1 MiB per output stream, request-size caps), and it adds no new
credential to protect. What it does change is the window: apply
specifies a command for a *future* container, exec runs one in a
*live* one — anything the task's runner wrote (keys in /run/px,
workspace state) is readable by an exec'd `cat`. That is already true
of the runner itself, so the boundary stays: the token is the
operator.

One residual race is accepted rather than engineered away: between
the request's entry and its dispatch, a delete (and PVE's later
recycling of the CTID) could in principle aim exec at a container
that no longer belongs to the task. exec therefore re-reads the task
and re-verifies hostname ownership immediately before dispatch — the
same discipline as `DestroyOwned` — which narrows the window to
milliseconds. Serializing exec fully against deletion is not
attempted; the small residual window stays, and closing it would
require blocking deletes on in-flight execs for no attacker that the
token does not already cover.

## Node SSH

px-server holds **root SSH access to the PVE node** — the largest
credential in the system, larger than any API key it manages. It is
required because PVE's REST API has no in-container exec; the boot
step rides `pct exec` over SSH. `ssh-key`/host-key pinning (M3) is
opt-in and stricter than TOFU — no first-use trust is ever recorded:
without a pin file, every host key is accepted on every connection, so
a MITM between px-server and the node gets node root. Pin it in
anything but a throwaway lab. Cluster mode widens this to one root SSH
per cluster node (dialed lazily, but with root the moment a task lands
there), and the host-key pin file becomes per host: a known_hosts
line scopes its key to the named hosts, an authorized_keys-format line
pins every node — a single-node pin file carries over verbatim. With a
pin file in use the check is fail-closed: a host the file never names
(missing or misspelled line) is refused at dial time, not silently
accepted.

## Suspend/resume

`px suspend` freezes the container's cgroup v2, which is a *pause*,
not a quiesce: every process and page of the task stays resident in
the node's memory, and the freeze is as authoritative as the runner
itself. Three properties follow. First, a suspended task's secret
material (model keys in /run/px, workspace state) sits in RAM of a
node an operator with root access can already read — the token
boundary does not change. Second, the freeze survives px-server
restarts (it lives in the container's cgroup, not in px-server's
memory), and the controller re-freezes a Suspended task that was
thawed out of band — so "suspended" is maintained, declaratively,
until resumed or deleted. Third, exec refuses a frozen task outright
(it would block inside the freezer), so a suspended task cannot be
probed into hanging; destroy thaws before stopping, since stop+destroy
of a frozen CT is undefined territory.

## Destroy guards

Lifecycle destroys (delete, TTL cleanup, interrupted-provision
recovery) verify the container's hostname matches `px-<task>`
(`DestroyOwned`) before touching it, so a VMID recycled by an external
party between crash and cleanup is never destroyed by px. Create's own
error paths are the exception: they destroy a container this
controller cloned seconds earlier, unverified — the window where that
VMID was recycled mid-provision (or where the destroy itself fails and
orphans the clone) is accepted. px's steady-state blast radius on the
node stays inside containers it named. The
corollary: px never touches containers it did not create, and the
operator's hand-written `/etc/pve/firewall/*.fw` entries are likewise
untouched (px writes firewall config only on containers it just
cloned, and removes it on destroy).

## Out of scope

- PVE host security itself (kernel patches, cgroup/namespace escape
  bugs, AppArmor policy of the distro template).
- Post-exploit lateral movement *from* a compromised PVE node — that
  node owns every task and every key by definition.
- Template supply chain beyond "build it yourself with
  `template/runner/build.sh`" (or `template/agent/build.sh` for the
  agent image): the Debian base image comes from Proxmox's repository,
  `apt` installs happen at build time, and the agent image additionally
  runs the Claude Code native installer fetched from `claude.ai` — px
  pins nothing further, so re-verify the templates if your threat model
  includes Proxmox's mirrors or the installer origin.
