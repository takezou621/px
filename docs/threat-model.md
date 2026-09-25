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

## px-server exposure

px-server binds loopback and serves plain HTTP by default; the bearer
token (`-token-file`) is opt-in. The intended deployment is *on the
operator's machine or a jump host*, not a shared network — if you must
expose it, put TLS-terminating auth in front (px does none) and treat
the token as the only barrier, since every task's command is
apply-on-the-wire by design.

## Node SSH

px-server holds **root SSH access to the PVE node** — the largest
credential in the system, larger than any API key it manages. It is
required because PVE's REST API has no in-container exec; the boot
step rides `pct exec` over SSH. `ssh-key`/host-key pinning (M3) is
opt-in and stricter than TOFU — no first-use trust is ever recorded:
without a pin file, every host key is accepted on every connection, so
a MITM between px-server and the node gets node root. Pin it in
anything but a throwaway lab.

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
  `template/runner/build.sh`": the Debian base image comes from
  Proxmox's repository, `apt` installs happen at build time, and px
  pins nothing further — re-verify the template if your threat model
  includes Proxmox's mirrors.
