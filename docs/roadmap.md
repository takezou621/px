# px roadmap

Status: 2026-09-26. Owner: Claude (acting PO).

## M6 — First-class agent runtime (current)

Chosen 2026-09-26 from four candidates (agent runtime, port exposure,
observability, user-defined templates): the primitives M3 assembled
(Model credentials, Gateway egress, Workspace code) only pay off once an
actual agent runs through them end to end — the missing piece is the
experience of launching one. Scope:

- [ ] Task-level goal: `spec.goal` joins the per-workspace goals in the
  runner's GOAL env (task goal first, then workspace blocks), so a task
  without workspaces can carry an instruction — today goal only exists
  per workspace, and an agent task is goal-first by nature.
- [ ] Agent template `px-agent-debian12` (`template/agent/build.sh`):
  the runner template plus the Claude Code CLI baked in via the native
  installer (a self-contained binary — no Node runtime in the image).
  `px run` defaults `spec.image` to it; `--image` overrides.
- [ ] `px run`: client-side sugar that builds and applies a Task from
  flags (`--model`, `--workspace NAME[=goal]`, `--gateway`, `--ttl`,
  `--name`, `--image`, `--cores`/`--memory`) with the positional goal,
  then polls the task and follows its log to a terminal phase (2s
  polling; new log output echoed as it lands), and exits with the
  task's exit code; `--no-wait` returns after apply, Ctrl-C detaches
  with the task left running. The default runner command is
  `sh -c 'claude --dangerously-skip-permissions -p "$GOAL"'`: inside an
  LXC sandbox behind a Gateway allowlist there is no human to answer
  permission prompts, so the isolation boundary is the container, not
  the CLI's permission system (documented in the threat model).
- [ ] E2E on the real node: template builds, an agent task boots, the
  CLI is present, and a task whose Model holds a non-working key fails
  cleanly with the provider error in the log (no credential needed);
  a live agent run with a real key is a user-supervised step.
- Explicitly out: session continuation across tasks (needs snapshots or
  a shared volume — deferred), interactive TTY exec (M4 deferral
  stands), port exposure (M7 candidate).

## M5 — API completeness & operational robustness (done)

Pulled up from the backlog after M4: small items that close visible holes
rather than add new surfaces. Order: workspace deletion first (user-visible
404 bug), then exec result layering, then the sshexec work (ctx, tests).

- [x] Workspace deletion (`DELETE /v1/workspaces/{name}` + `px delete
  workspace NAME`) — previously the CLI routed `delete workspace X` to the
  task endpoint and reported a misleading 404 (found in the M2 E2E).
  Deletion is safe: running tasks keep their cloned copy; only future
  applies referencing the name fail to resolve. Declarative config, so
  deletion is immediate — no reconcile, no status. The store keeps the
  delete single-statement like the other deletes; the CLI routes on the
  resource word, not the noun order.
- [x] Distinguish "pct exec itself failed" from "command exited non-zero"
  in `Exec` results, so the CLI can say which layer failed instead of
  printing an empty stdout with an unrelated exit code. The node-side
  wrapper guards on `pct status` output (`status: running` — the exit code
  alone is zero for a stopped container too) and prints a verdict line
  (`PX_PCT:<n>` / `PX_PCT_ABSENT`) on stderr after a leading newline, so it
  is an exact final line even when the command's stderr lacks one;
  `ExecResult` carries `pctFailed`/`pctReason`, stdout stays byte-exact,
  and the verdict is trimmed off stderr only on an exact tail match and
  only while stderr survived the cap uncropped. No verdict at all is
  fail-closed (pctFailed, ExitCode -1).
- [x] sshexec accepts a `context.Context`: the HTTP handler's request
  context propagates, so a client disconnect cancels the in-flight exec
  (previously it ran to the 2-minute cap). Cancelation closes the session
  but not the shared client, and is never a redial — it races `NewSession`
  too, so a node that stalls mid-handshake cannot hang a canceled call.
  `redial` no longer closes whatever client is current: it only replaces
  the client the failed attempt actually used (runStreams returns it), so
  a concurrent re-dial by another caller is kept, not torn down.
- [x] sshexec fake-server tests: an in-process SSH server (x/crypto/ssh,
  per-exec handler) covers dial/auth, redial after a mid-run drop, the
  one-shot no-retry path, the per-call timeout, ctx cancel + shared-client
  survival, and stdout/stderr separation — previously covered only by the
  in-memory executor fake at the server layer. The redial tests also count
  TCP connections at the server, so a "retry" that reused the failed
  connection (or tore down another caller's re-dial) would fail the count.

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

- [x] SSH host key pinning (today the first-seen key is accepted unverified)
  - Design: opt-in pinning, same shape as the M2 token auth. `-ssh-host-key F`
    (env `PX_SSH_HOST_KEY`) names a file of public host keys, one per line in
    authorized_keys format (`ssh-ed25519 AAAA... comment`) — several lines are
    allowed so a key rotation window keeps working. sshexec parses the file at
    Dial and installs a HostKeyCallback that accepts only those keys
    (comparing `ssh.PublicKey.Marshal` bytes); a mismatch fails the dial with
    the offending key's fingerprint in the error. Without the flag behavior is
    unchanged (first key accepted) and px-server logs a warning naming the
    flag — TOFU state tracking is deliberately not built; an explicit pin file
    is simpler and fits the single-binary axis. Getting the key:
    `ssh-keyscan -t ed25519 HOST` on the px-server host, or read
    `/etc/ssh/ssh_host_*_key.pub` on the PVE node. Found in the E2E: PVE
    offers ECDSA/RSA/ED25519 host keys and the handshake negotiates whatever
    the server prefers (ECDSA here), so pinning ED25519 alone never matches —
    the dial therefore also restricts `HostKeyAlgorithms` to the pinned key
    types, making the negotiated key one the pin can actually accept. A
    pinned RSA key additionally advertises as rsa-sha2-256/512 (modern sshd
    no longer negotiates bare ssh-rsa; rsa-sha2 host keys decode to the same
    public key blob the pin holds). `@cert-authority`/`@revoked` marker lines
    are rejected at parse time: they pin CA trust, not a host key, so
    accepting them as plain pins would silently never match.
- [x] Destroy guards by container name: PVE nextid is a suggestion, not a reservation. Residual M1 window: a crash between VMID persist and Create can leave a VMID that destroy later targets after an external party reuses it; and a failed Create that clears its VMID can orphan a clone that succeeded late (PVE tasks outlive their waiter) — verify `px-<task>` name ownership before destroy
  - Design: the PVE client grows `ContainerHostname(vmid)` (GET
    `/nodes/{node}/lxc/{vmid}/config`), and the Provisioner grows
    `DestroyOwned(taskName, vmid)` which stops + destroys only when the
    container's hostname is exactly `px-<taskName>`; a mismatch returns the
    sentinel `ErrNotOwned`. "Container does not exist" from the hostname read
    counts as already-destroyed (nil) — deletes stay idempotent. Not-found is
    detected structurally (`IsNotFound`: PVE's 500 whose body says the config
    file is gone), never by substring accidents — a DNS failure's "no such
    host" must not read as "absent", or a destroy would silently skip and
    orphan the real container. The Provisioner also grows `Owned(taskName,
    vmid)` — the same hostname check without destroying — so the adoption
    path can refuse a reused VMID before running its foreign runner as the
    task. The three
    controller destroy paths (deletion, TTL cleanup, interrupted-provision
    cleanup) move to `DestroyOwned`; Create's own partial cleanup keeps the
    bare `Destroy` since it runs seconds after this controller cloned that
    exact VMID. On `ErrNotOwned` the controller clears `Status.Container`,
    logs loudly, and lets the record proceed (deletion drops it, TTL/interrupt
    fail the task): a VMID that now belongs to someone else is not px's to
    destroy, and retrying forever would wedge the record.
- [x] Distinguish pct exec transient failures from "no marker" in Booted(): today any non-zero pct exec is treated as unbooted, so a transient pct failure could destroy a live runner of an already-interrupted task; re-evaluate against real pct behavior in the PVE E2E (container restarts wiping the /run tmpfs are out of scope — external intervention)
- [x] E2E against the real PVE node (pinning on): a wrong pin fails the dial with
    the real key's fingerprint; the negotiation surprise above surfaced and was
    fixed (`pinnedAlgos`); with the correct pin a task ran `Pending ->
    Provisioning -> Succeeded` (`M3_PIN_OK` in logs, ~9s). Destroy guard
    verified live by renaming a px-created container (`pct set <vmid>
    --hostname not-px`): `px delete task` dropped the record in ~4s, left the
    container running, and logged `destroy skipped: container is not owned by
    the task` with the offending hostname. Test container destroyed afterwards;
    no containers left on the node.
  - Design: `Booted()` uses a two-step probe —
    `test -f /run/px/booted; echo PX_PROBE:$?` — so a successful exec is
    distinguishable from pct exec itself failing. `pct exec` runs as an
    independent process: it can die transiently while the container (pid 1)
    stays running, so treating any non-zero exec as "no marker" would destroy
    a runner that is mid-boot, and treating "running" as "transient" would
    permanently wedge on a partial clone (the container survives its dead
    boot script). The probe splits the two: a printed `PX_PROBE:0` means
    booted (adopt), `PX_PROBE:1` means no marker (destroy the partial
    clone), and no trailer is a retryable error. When pct exec itself fails
    (non-zero with no verdict), the container status decides: gone counts as
    unbooted (a record whose clone was already destroyed recovers instead of
    wedging), stopped counts as unbooted (a runner cannot exist there), and
    running is an error (retry next tick). The same probe-shaped fix covers
    the adoption path: before adopting, the controller verifies ownership
    with `Owned()` so a reused VMID carrying a foreign boot marker fails the
    task instead of running someone else's runner.
- [x] E2E against the real PVE node (Model kind, `scripts/e2e-model.sh`):
    a dummy-key Model applied write-only (apply/list/describe never echo
    the key, describe shows `<redacted>`); re-applying the placeholder
    rejected with the placeholder-naming error; a task with `spec.model`
    reached `Succeeded` in ~5s with `ANTHROPIC_API_KEY` (checked by
    length, never the value) and `ANTHROPIC_BASE_URL` in the runner env;
    `model.key`/`model.env`/`boot.sh` all `0600` inside the container
    (the umask fix verified live); delete left no container on the node.
    Surfaced by the run: the CLI's `describe` HTML-escaped `<` and `>` in
    the placeholder — it now encodes with
    `SetEscapeHTML(false)` like the server, so describe output re-applied
    as YAML still trips the placeholder guard.
- [x] Gateway kind: egress allowlist via LXC firewall
  - Design: `spec.egress` is a list of destination rules — each has `cidr`
    (required, IP or CIDR) plus optional `ports` (`"443"`, `"80,443"`,
    `"8000:9000"`) and `proto` (`tcp` default, or `udp`; a rule with neither
    ports nor proto allows everything to that cidr). A Task references at
    most one Gateway (`task.spec.gateway`, by name), resolved at provision
    time exactly like Workspaces/Models — an unknown name is a
    ProvisionFailed reason, so deleting a Gateway only affects tasks applied
    later. At provision, after clone and before start, the controller
    enables the LXC firewall on the clone — net0 gains `firewall=1` (read
    the cloned net0 first: the template's carries hwaddr/type the clone
    must keep), CT option `firewall=1`, `policy_out=DROP` — and inserts one
    ACCEPT rule per spec rule: default-deny egress, replies ride
    conntrack's ESTABLISHED,RELATED and policy_in stays at its default.
    DNS (udp+tcp 53, any dest) and DHCP (udp 67) are always allowed: nearly
    every real egress is name-based, and the lease must survive the
    firewall — the DNS-tunnel exfil caveat goes to the threat-model doc.
    An empty egress list is valid: a DNS-only sandbox. CIDR-only by design —
    hostname rules would need apply-time DNS resolution that goes stale
    (TOCTOU); pin a proxy's IP or point `model.baseUrl` at one instead.
    Caps: MaxEgressRules 32, per-field byte caps, ports regex-validated,
    cidr must parse as IP or CIDR. Declarative config like
    Workspace/Model: apply upserts, no status. Applied via the PVE REST API
    (GET config → PUT net0/firewall/policy_out → POST firewall/rules), not
    SSH — config endpoints are JSON with no quoting pitfalls; SSH+pct stays
    exec-only per the architecture note. Server: GET /v1/gateways,
    /v1/gateways/{name}, DELETE /v1/gateways/{name}; CLI: `px get
    gateways`, `px describe gateway NAME`, `px delete gateway NAME`.
    Landed 2026-09-25, E2E-verified live (`scripts/e2e-gateway.sh`, see
    docs/e2e.md §3c). Two live findings fixed along the way: the runner's
    model.env source used `2>/dev/null` to ignore a missing file, but dash
    exits on a failed dot-builtin — model-less tasks froze in Running; and
    pve-firewall programs the dataplane ~3s after start (measured with a
    per-second egress probe), so the provisioner now gates the boot on the
    container's `veth<vmid>i0-OUT` chain appearing in both the node's
    iptables and ip6tables rulesets (pve-firewall loads the two families in
    separate passes) — the runner never boots ahead of enforcement.
- [x] Model kind: LLM credentials injected as per-task env/secrets
  - Design: `spec.provider` is a closed set (`anthropic`, `openai`) that maps
    to the well-known env names (`ANTHROPIC_API_KEY`/`ANTHROPIC_BASE_URL`,
    `OPENAI_API_KEY`/`OPENAI_BASE_URL`) — no free-form env mapping to keep
    apply-time validation total. `spec.apiKey` is required; `spec.baseUrl`
    overrides the default endpoint (proxies, Azure-style gateways). A Task
    references at most one Model (`task.spec.model`, optional, by name);
    resolution happens at provision time exactly like Workspaces — an
    unknown name is a ProvisionFailed reason, so a Model deleted mid-run
    only affects the next task. The APIKey is a write-only secret: the
    store keeps it in SQLite (single-binary axis; the DB file is operator
    territory — 0600 ownership is documented in the threat model), but GET
    /v1/models and describe return the spec with `apiKey` replaced by
    `<redacted>`, and the key never appears in controller logs or provision
    errors. Injection rides the existing base64 boot path: the boot script
    decodes the key into /run/px/model.key and writes a static
    /run/px/model.env that exports the provider's env names from it, and
    the runner spawn sources model.env before cmd.sh — so the key crosses
    SSH as base64 argv (no quoting, no MAX_ARG_STRLEN surprises) and lands
    in the runner's environment, not in the boot log (task.log captures the
    runner only). Manifest caps: MaxAPIKeyBytes 4KiB, MaxBaseURLBytes 512.
    Server: GET /v1/models, /v1/models/{name}, DELETE /v1/models/{name};
    CLI: `px get models`, `px describe model NAME`, `px delete model NAME`.
    Review hardening: the boot script and the pct exec wrapper set
    `umask 077` before any write, so model.key/model.env/boot.sh (which
    embeds the key base64-encoded) never fall back to the container's
    default 0644; apply rejects apiKey/baseUrl values with surrounding
    whitespace or control bytes (command substitution on the container side
    strips trailing newlines — such a value would reach the runner mutated);
    and apply rejects the literal `<redacted>` placeholder, so re-applying
    a fetched Model fails loudly instead of silently replacing the real key.
- [x] unprivileged CT default; threat model doc (`docs/threat-model.md`)
  - Design: the build script already created the template with
    `--unprivileged 1`, but the clone inherited the flag implicitly — and
    live testing showed the clone endpoint cannot *set* it (PVE's clone_vm
    schema rejects unknown parameters; the flag only inherits from the
    source), so px verifies the inheritance instead: after clone, before
    start, the provision reads the container config and fails the task
    (destroying the clone) if `unprivileged` is not set. The guarantee rests
    on px's verification, not on the template author's discipline or the
    PVE release's default. The doc covers the three accepted
    trade-offs noted below plus the full boundary map: LXC's shared-kernel
    weakness and `nesting=1` as the platform's risk budget; DNS-tunnel exfil
    through the implicit DNS allow (accepted — closing it needs stale
    hostname rules); image-init traffic runs ahead of the egress gate (the
    gate holds only the px runner — during the dataplane-loading window the
    container's init can transmit, which is safe solely because
    `spec.image` is the trusted px-built template; a user-supplied image
    would need the gate before start, which PVE cannot do since the
    dataplane only exists once the veth does); px.db is a credential
    store — px enforces 0600 on it (and its WAL/SHM siblings) at open —
    Model APIKeys sit in SQLite by the single-binary axis; node
    root SSH as the largest credential (pin it); destroy guards keep px's
    blast radius to containers it named.

## M4 — Beyond one node

- [x] `px exec` (run a debug command in a running task's container)
  - Design: `px exec NAME -- COMMAND [ARG...]` posts to
    `POST /v1/tasks/{name}/exec` with `{"command": [...]}`; the server
    requires a live sandbox (phase Running, container set — anything else
    is a 409) and runs the command via the existing node SSH + `pct exec`
    path, returning `{"stdout", "stderr", "exitCode", "truncated"}` in one
    JSON response. The command crosses both shells (the SSH transport's
    remote shell, then pct's argv) safely: each argument is single-quoted
    POSIX-style (`'` → `'\''`), so metacharacters and spaces reach the
    container byte-exact — a debug tool must not be the quoting hole the
    boot script's base64 embedding was built to avoid. Output is capped at
    1 MiB per stream (a hung `cat /dev/zero` must not fill px-server's
    memory; overflow sets `truncated` and keeps going), the whole call is
    bounded by a 2-minute timeout, and a non-zero command exit is a result,
    not an error — the CLI exits with the command's code, like kubectl.
    Request caps match the apply style: 16 arguments, 4 KiB each, 32 KiB
    total. `px exec` is an operator tool on the same trust level as apply
    (the token holder already specifies arbitrary commands at apply time);
    noted in the threat model. stdin/TTY relay is deliberately out: it
    needs a second channel or raw HTTP plumbing that no other verb uses —
    one-shot commands cover debugging (`px exec NAME -- sh -c 'ps aux'`).
    Post-review hardening (double-agent review): exec dispatches through
    `RunStreamsOnce` — sshexec's retry path must never re-run a one-shot
    user command (a retry repeats side effects and splices the first
    attempt's partial output into the result) — and re-verifies task state
    plus hostname ownership right before dispatch, so a delete during the
    body read cannot aim exec at a recycled CTID (residual race accepted,
    threat model). Empty argv elements are legal; NUL bytes and trailing
    JSON garbage are rejected.
- [x] Multi-node scheduling (PVE cluster, pick node by free resources)
  - Landed 2026-09-26, E2E-verified live against the lab cluster
    (`akebono-cluster`, nodes third/second; `scripts/e2e-multi-node.sh`,
    docs/e2e.md §3f, 13/13): the NODE column is populated and stable,
    the schedule lands on a template-bearing online node, the clone
    really lives on that node, exec/logs/suspend/resume cross the node
    boundary, and delete removes the container from that same node.
    Single-node mode unchanged (smoke 14/14 regression on `third`).
  - Design: `-pve-node` becomes optional. Set, px-server is single-node
    (today's behavior: one proxmox.Client, one SSH executor, fail-fast
    dial at boot). Empty, px-server runs in cluster mode: the PVE API
    proxies every node through one endpoint (the GUI depends on this),
    so one token reaches all of them; SSH is per node. Scheduling
    happens at provision time from one `GET /cluster/resources`:
    candidates are nodes that are both `online` and carry the task's
    `spec.image` template (`FindTemplateVMID` per candidate) —
    restricting to template-bearing nodes also sidesteps cross-node
    clone (the clone `target` parameter's behavior varies across PVE
    versions; cloning from the template's own node does not). The pick
    is most free memory (`maxmem-mem`), tie-broken by lower `cpu` load
    then node name, so it is deterministic. No candidate is a
    ProvisionFailed reason like any other provision error. The winning
    node persists to a new `Status.Node` (status is a JSON blob in
    SQLite, so no schema migration); every later provisioner call
    (poll, logs, exec, destroy) carries it, and legacy records
    persisted before this change hold an empty Node that the controller
    repairs once via a vmid→node lookup, so a pre-cluster px-server
    keeps managing its tasks across upgrade. Connections: a small pool
    keys one `*proxmox.Client` and one `*sshexec.Executor` per node —
    the SSH dial is lazy, so a node no task ever lands on is never
    dialed; single-node mode keeps its boot fail-fast, cluster mode
    fails fast on the API only. Node names are not DNS names (the lab
    cluster's `third` has no /etc/hosts entry for `second`), so
    `-ssh-host-override "second=192.168.2.183,forth=192.168.2.214"`
    maps node→SSH host; unset means the node's own name. Host key
    pinning becomes per host: pin-file lines in known_hosts format
    carry a hostname field — a line with one pins only that host;
    authorized_keys-format lines (no hostname) pin every host, so a
    single-node pin file keeps working verbatim. `px get tasks` gains
    a NODE column.
- [x] `px suspend` / `px resume` (CT freeze)
  - Landed 2026-09-26, E2E-verified live in both cluster mode (task on
    `second`, checked through its own node's SSH) and single-node mode
    (`scripts/e2e-suspend.sh`, docs/e2e.md §3e, 22/22 each): Suspended
    lands with the cgroup actually frozen, exec refuses 409 while
    frozen, resume restores Running with exec working, idempotent
    suspend/resume are 200s, an out-of-band thaw is re-frozen by the
    controller, deleting a frozen task thaws first and leaves no
    container, terminal tasks refuse suspend. Suspend-across-restart
    verified live (px-server killed and restarted while a task was
    Suspended: phase held, cgroup stayed frozen, resume ran the
    container back to Running). The first live run caught a real bug:
    `/proc/<pid>/cgroup`'s v2 line is `0::<path>` with no spaces, so
    the awk field-split never matched — the parser is now
    `sed -n 's/^0:://p'` on both sides (provisioner and E2E).
  - Design: cgroup-v2 freeze, not snapshots — PVE snapshots are a QEMU
    feature; freezing keeps the process tree and memory in place,
    resumes instantly, and survives px-server restarts (the CT holds
    its state; only the phase record needs the controller). Constraint
    discovered while designing: `pct exec` cannot enter a frozen
    cgroup — the spawned process blocks in the freezer until thaw, so
    an exec-based probe would hang every reconcile tick — therefore
    all freeze operations run at node level: `GET
    /nodes/{n}/lxc/{vmid}/status/current` yields the container's pid,
    `/proc/<pid>/cgroup`'s `0::` line names the CT's cgroup v2 path,
    writing 1/0 to that directory's `cgroup.freeze` freezes/thaws, and
    `cgroup.events`' `frozen` line verifies (all PVE 9 nodes ship
    cgroup v2 unified — verified on the lab cluster). Three new phases
    make intent declarative, matching the delete model: `POST
    /v1/tasks/{name}/suspend` / `/resume` handlers only persist the
    phase (single-statement json_set, the deletionTimestamp
    discipline) and touch no SSH — the reconcile loop runs the state
    machine: Suspending → freeze → verify → Suspended; Resuming →
    thaw → verify → Running; Suspended re-verifies every tick and
    re-freezes if thawed externally (declarative maintenance); a
    Running task that is in fact frozen (someone froze it by hand) is
    adopted into Suspended at its next poll — which must precede any
    pct exec, since that would hang. Suspend/resume on a deleting or
    terminal task is a 409; suspend while suspended and resume while
    running are idempotent 200s. exec stays Running-only (409 on
    Suspended — that is exactly the case that would hang). Destroy
    thaws first: stop+destroy of a frozen CT is undefined territory.
    TTL cleanup and watch are untouched (suspended is non-terminal, so
    TTL never fires and watch keeps streaming). `px suspend task NAME`
    / `px resume task NAME` join the kubectl-style CLI. Threat-model
    note: a suspended CT keeps its whole memory resident on the node —
    suspend is pause, not save-to-disk.

## Explicitly deferred

- Kubernetes backend, gRPC API, multi-cloud — see architecture doc non-goals.

## Backlog

Small items deferred from reviews; not scheduled. (The former backlog's
four items moved to M5, 2026-09-26.)
