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
- [ ] Gateway kind: egress allowlist via LXC firewall
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
- [ ] unprivileged CT default; docs on threat model

## M4 — Beyond one node

- [ ] Multi-node scheduling (PVE cluster, pick node by free resources)
- [ ] `px suspend` / `px resume` (PVE snapshot / CT freeze)
- [ ] `px exec` (debug shell into a running task)

## Explicitly deferred

- Kubernetes backend, gRPC API, multi-cloud — see architecture doc non-goals.
