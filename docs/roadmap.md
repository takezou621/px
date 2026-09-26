# px roadmap

Status: 2026-09-26. Owner: Claude (acting PO).

## M7 — Port exposure (done 2026-09-26)

Chosen 2026-09-26 from four candidates (port exposure, session
continuation, observability, user-defined templates): M6 closed the
loop from goal to a running agent, but an agent that builds anything
web-shaped has no way to show it to a human — the missing piece is
`curl http://<node>:<port>` reaching into the sandbox. Scope:

- [x] `spec.ports`: a list of `{name, port, hostPort?}` — `port` is the
  TCP port the task listens on inside the container, `hostPort` the
  port the node listens on for it (omitted: auto-assigned from a px
  range, store-tracked so two tasks never collide; a colliding explicit
  `hostPort` is an apply-time rejection). Validation: MaxPorts, name
  (DNS label), port 1-65535, hostPort in a px-reserved range.
- [x] Forwarding runs on the node as `socat TCP-LISTEN:<hostPort>,fork
  TCP:<ctIP>:<port>` — no agent inside the container, nothing in the
  CT image, no iptables: the lab's LAN is one flat L2 segment
  (192.168.2.0/24 on vmbr0), so a DNAT would need SNAT too for the
  return path (asymmetric routing drops the reply), while a TCP proxy
  has no return-path property to reason about and its lifecycle is a
  process px can kill. ctIP comes from the existing node SSH + `pct
  exec ip` path (the container's eth0 address, resolved after boot and
  re-checked on reconcile so a DHCP change re-programs the forward).
- [x] Declarative reconciliation, like suspend: Status.Ports is the
  desired state, the controller re-programs forwards that vanished or
  drifted every tick (px-server restart included — the record carries
  everything needed to rebuild), and delete/TTL/provision-failure
  removes them with the container (kill by the listen-port signature,
  verified gone). `px get tasks` gains a PORTS column, describe shows
  the mapping, and a port publish requires a Running container like
  exec does.
- [x] Threat model: publishing a port is an opt-in widening of the
  sandbox's attack surface — the node now accepts connections the
  Gateway allowlist was built to deny, on the operator's LAN; the
  section names what a published task can be reached for and that
  bind-local (loopback-only) exposure is out of scope for M7.
- [x] E2E on the real node: a task serving an HTTP port is reachable
  through `node:hostPort` from outside the node, a colliding hostPort
  is rejected at apply, delete removes both container and forward (no
  listener left on the node), and a px-server restart rebuilds the
  forwards from the store. Verified 2026-09-26: `scripts/e2e-ports.sh`
  17/17 plus a manual restart-rebuild pass; found on the way: the ctIP
  awk misread `ip -o` field positions, and a pve-firewall-enabled node
  needs an explicit allow for 30000–32767 in cluster.fw (see
  docs/e2e.md Prerequisites).

## M8 — Session continuation (done 2026-09-26)

Chosen 2026-09-26 after M7 (candidates: session continuation,
observability, user-defined templates). M6 deferred this believing it
needed snapshots or a shared volume; it turns out neither is true — the
Claude Code conversation lives as JSONL files under the runner user's
`~/.claude/projects`, a directory of text that tars to a few MB, so a
capture/restore pair over the existing node-SSH + `pct exec` channel
covers it. The gap today: a task's agent work evaporates when its
container is destroyed at TTL cleanup or delete, so "run the follow-up
against what the agent learned" is impossible. Scope: carry that
conversation from a finished task into the next one.

Design decisions:

- What transfers: the runner user's `~/.claude/projects` only. Code
  inheritance is already solved by git Workspaces — a continuing task
  re-clones. Full-container snapshots (vzdump) are rejected as
  minutes-heavy and modeling state px does not own; a shared LXC mount
  (mpX) is rejected because `local`/`local-lvm` rootfs storage cannot
  cross nodes, which would break M4 multi-node.
- Capture window: a terminal task's container stays alive until TTL
  cleanup or delete destroys it, so capture hooks the three paths that
  destroy: the tick that settles Succeeded/Failed, the delete path, and
  the TTL-destroy gate. Each saves once per task (a status flag marks
  it), retries on the next tick when the SSH/capture step fails (the
  RemovePorts pattern — destroy does not proceed past an uncaptured
  task), and proceeds when the container is already gone (nothing to
  capture, ProvisionFailed tasks included).
- Storage: a `sessions` SQLite table (task name → gzip+base64 tar of
  the archive, capped at 32 MiB — session JSONL is text and compresses
  well), not the Task status JSON: status is re-persisted every tick and
  must not carry megabytes. Status carries only a saved flag and byte
  count. Deleting the source task drops its session row — `continueFrom`
  reads the source task's own record, so the row's lifetime is the
  source task's lifetime. (A source task with the default TTL never
  auto-cleans, so the common case keeps working.)
- Restore path: the continuing task's Create unpacks the archive inside
  the container before the runner boots, through the same `pct exec`
  channel in chunked base64 writes — the kernel's MAX_ARG_STRLEN caps a
  single argv element at 128 KiB, so the boot-command embedding pattern
  does not scale to megabytes. Everything runs as the runner user
  (`pct exec --user`), staging under the user's own HOME (resolved
  in-container via getent, never assumed), umask 077 like the rest of
  the /run/px staging.
- API: `spec.session.continueFrom: <taskname>`. Reference validation
  mirrors the Gateway pattern: unknown source, source not yet finished,
  or source without a captured session resolves to ProvisionFailed
  before any container is created; a self-reference is an apply-time
  rejection (knowable immediately). A continuing task copies nothing
  from the source — its own spec is authoritative.
- CLI: `px run --continue TASK "goal"` — sugar that copies the source
  task's spec (image, runner command/user, resources, model, gateway,
  workspaces), swaps the goal, sets continueFrom, and swaps the default
  agent command for a `claude --continue` variant when the source used
  the default. Flags that restate copied fields (-image, -model,
  -workspace, -gateway, -cores, -memory) are rejected — the copy is the
  point; -name, -ttl, -no-wait stay allowed. TTL and spec.ports are
  deliberately not copied: a short TTL inherited by every link of a
  chain would end sessions by accident, and an explicit hostPort still
  claimed by the living source task would fail the apply.
- Explicitly out: named Session resources (one conversation, many
  tasks — revisit if divergence matters), `~/.claude.json` (settings,
  not conversation), capture of a Running task's live state.

- [x] `spec.session.continueFrom` + validation (self-ref rejected at
  apply; unknown/unfinished/uncaptured source resolves to
  ProvisionFailed)
- [x] Capture on the three destroy paths (terminal tick, delete, TTL
  gate), one save per task, retried until the container is gone
- [x] `sessions` table in the store; source-task deletion drops the
  row; status carries the saved flag and byte count
- [x] Restore in Create before boot (chunked base64 through pct exec as
  the runner user, HOME resolved in-container)
- [x] `px run --continue`
- [x] Unit tests: store round-trip, capture on each destroy path,
  restore round-trip through the fake provisioner, validation matrix
- [x] E2E on the real node (`scripts/e2e-session.sh`): a fake session
  JSONL (no real API key, runner template suffices), capture on
  success, restore into a continuing task whose runner reads the
  restored file, ProvisionFailed for unknown/unfinished/uncaptured
  sources, the TTL path, and cleanup

Shipped after the double-agent self-review (Codex + a fresh Claude
reviewer, independent passes over the same diff). Both flagged the
capture path's missing ownership gate and a size-unbounded capture;
the Claude pass additionally caught that `Owned()` checks the config
hostname and returns true for a stopped container, so capture also
probes `Running` before exec and settles when the container is gone
(an uncapturable capture must not block delete/TTL forever). The
capture settles rather than retries on a remote size refusal
(`ErrSessionTooLarge`), the terminal-tick transition captures in the
same tick to close a delete race, restore-stage writes run un-retried
(a redialed chunk append would corrupt the archive), and a fresh task
record clears any session row a same-named predecessor left behind.

## M9 — User-defined templates

Chosen 2026-09-26 after M8 (candidates: user-defined templates,
observability, named sessions, session capture streaming). M6 made any
container a first-class agent runtime and M8 lets its work outlive it;
the next ceiling is generality — a user who builds their own LXC
template (their CLI, their toolchain) must reverse-engineer px's
implicit contract, and nothing in px says what templates exist or
whether one would work: a task naming a missing or incompatible image
applies cleanly and only dies at provision with "no online node holds
template". px treats the PVE node as the registry and its template
requirements as a published contract — scope:

- [x] Template contract, documented (template/README.md): what px
  requires of any LXC template it clones — unprivileged, net0 with
  `ip=dhcp` (the provisioner resolves the address via `ip` in-CT and
  cannot wait for a static config), a writable `/run/px` for the boot
  marker, `/bin/sh` for the boot script; agent-template extras (the
  Claude CLI) are optional and only needed by the default agent
  command. Plus how to author one from template/agent/build.sh.
- [x] `GET /v1/templates`: cluster-wide discovery through the existing
  ClusterResources view (lxc rows with the template flag), then one
  API config read per template — unprivileged flag, net0 — so every
  check is API-only; a template is never exec'd into (it is not ours
  and may be stopped). Single-node mode lists the configured node
  only; cluster mode lists every online node, matching what Schedule
  can actually clone from.
- [x] Compatibility verdict, computed server-side and carried on the
  record: `pxOk` (all requirements met) plus the failing requirement
  names, so a user sees "why would my template fail" without
  provisioning anything. Facts (vmid, node, unprivileged, net0) ship
  alongside; the CLI renders them.
- [x] CLI: `px get templates` (NAME, VMID, NODE, PX-OK columns) and
  `px describe template NAME` (facts + the failed requirements).
- [x] Apply-time: deliberately no PVE check. Apply stays
  control-plane-only (the kubectl precedent: apply does not verify the
  image exists, the pull error surfaces later), and Schedule's
  ProvisionFailed reason already names the template — a PVE round-trip
  inside apply would make the API unusable whenever the node is down.
- [x] Explicitly out: px orchestrating template builds (build.sh stays
  a node-side script — px would need node SSH in the CLI, a second
  control path), a px-side template registry or storage (PVE storage
  already is the registry), auth/privilege requirements beyond
  unprivileged.
- [x] Unit tests: the verdict logic (all-requirement matrix, missing
  config keys), the handler against a fake cluster view, CLI table
  parsing.
- [x] E2E on the real node (`scripts/e2e-templates.sh`, 12/12 pass
  2026-09-26): the shipped
  templates list with PX-OK true, describe shows their facts, a task
  naming an unknown image reaches ProvisionFailed with the template
  named in the reason, and a user-authored minimal template built
  per the contract doc lists as PX-OK.

## M10 — Observability: task events & metrics

Chosen 2026-09-26 after M9 (candidates: observability, named sessions,
scheduled tasks, session capture streaming) — the fourth time
observability reached the shortlist. M3–M9's live debugging kept hitting
the same wall: a task's path to ProvisionFailed exists only in
px-server's stderr log. The record says what phase a task is in now, not
how it got there — how many retries, which tick scheduled it onto which
node, when the session was captured. Scope:

- [x] `events` table in the store: (task, ts, reason, message) rows
  written by the controller at phase transitions and the operational
  moments around them (node scheduling, session capture, destroy-guard
  refusal, TTL cleanup). Capped per task (newest N kept) and dropped
  when the source task is deleted; never carried in Task Status (it is
  re-persisted every tick — the M8 lesson).
- [x] `GET /v1/tasks/{name}/events` and `GET /v1/events`; CLI `px
  events` (cluster-wide; both endpoints return newest first, the CLI
  prints oldest first so a history reads forward) and an Events section
  in `px describe task`.
- [x] `GET /v1/metrics`: hand-written Prometheus text (no client
  library — single-binary axis) covering task counts by phase,
  reconcile tick duration, store size, and event totals. Protected
  like every other route when -token-file is set; task names never
  appear.
- [x] CLI `px metrics` (prints the server's metrics text).
- [x] Unit tests: event round-trip + cap + delete cascade, events on
  the transition paths, metrics text shape, handlers.
- [x] E2E on the real node: a task's transitions appear as events in
  order, a ProvisionFailed carries its reason, delete drops the
  task's events, /metrics has the right shape.

Explicitly out: distributed tracing, log aggregation, events in `px
watch`, a Prometheus client dependency.

## M11 — Named sessions

Chosen 2026-09-26 after M10 (candidates: named sessions, scheduled
tasks, quota/limits, capture streaming). M8 explicitly out'd Session
resources with "revisit if divergence matters" — divergence does matter
in one real place: a `--continue` chain dies with its newest task's
record. The capture lives in the sessions table under the source task's
name and delete drops the row, so no old task can be cleaned while its
conversation still matters, and a weeks-long workflow leaves behind a
trail of chain-link tasks that must all stay. The fix decided here is
NOT a Session resource: the four primitives stay (ax compatibility is a
design axis), and the sessions table grows into a namespace of named
captures instead.

Design decisions:

- `spec.session.name` (new, optional): the capture destination.
  Default is the task's own name — exactly today's behavior. A default
  row keeps its current lifetime (dies with the task record); an
  explicitly named row survives task deletion, because its lifetime is
  the user's. Rows carry an `explicit` flag, and the user-owned rows
  are shielded from an unlucky task name: the task-lifetime cleanups
  only ever touch explicit=0 rows, and a default capture colliding with
  an explicitly named one is refused (ErrSessionOwned) — the controller
  drops the write loudly (a SessionSaved event names the loss) instead
  of clobbering the user's row, and the task still finishes. Many tasks
  may write the same explicit name; the upsert converges
  last-writer-wins, which for sessions means "the newest full state of
  the conversation". No merge is attempted.
- `continueFrom` gains the `session:NAME` form: restore directly from
  a named capture, with no task record consulted. A capture is a
  settled state, unlike a task, so there is no phase to check; a
  missing row is a ProvisionFailed, like today's uncaptured source.
  The `session:` prefix cannot collide with task names (DNS-1123), and
  a bare `continueFrom` stays task-scoped (phase-checked, resolved
  under the source's capture name — spec.session.name when it names
  one), so M8 manifests keep meaning exactly what they meant.
- The sessions table grows `last_task` (who wrote the capture last —
  feeds `describe` and the CLI sugar below; nothing resolves through it
  at provision time) and `explicit` (the user-owned flag above). Both
  are ALTER TABLE'd in with a backfill that keeps pre-existing rows
  exactly where they were (`last_task = task`, `explicit = 0`), and the
  backfill runs idempotently on every open, so a crash between ALTER
  and backfill heals instead of skipping. The pre-existing `created_at`
  (SQLite `datetime('now')`, UTC) is the written-at shown to users, so
  a resave moves it — the last writer's time, not the first capture's.
- API: `GET /v1/sessions` (metadata only — name, bytes, last task,
  written-at), `GET /v1/sessions/{name}`, `DELETE /v1/sessions/{name}`.
  The archive itself is never served: it is a tar of runner-internal
  JSONL, not a user-facing artifact, and it would be the largest
  payload the API can return for no caller. Deleting a session drops
  only the row.
- CLI: `px get sessions`, `px describe session NAME` (plus the tasks
  that reference it, resolved against the live task list), `px delete
  session NAME`, and `px run --continue-session NAME "goal"` — the
  counterpart of `--continue` for a conversation outliving its tasks:
  it copies the last writer's spec (the copy is the point, same flag
  rules), sets `continueFrom: session:NAME`, and sets
  `spec.session.name: NAME` so the capture returns to the same name —
  one conversation, one name. If the last writer's record is gone the
  sugar fails loudly; a hand-written manifest with `spec.session` is
  the fallback.
- Validation: `spec.session.name` is a validated name; it may equal the
  task's own name (same row either way).
- Metrics/events: a `px_sessions` gauge (row count) joins
  `px_sessions_bytes`; SessionSaved event messages name the
  destination session.

Explicitly out: merge or diff between captures, versioned session
history beyond the newest state, a Session kind in the manifest API
(rejected — a capture is produced by a task and never applied by a
user, so it has no spec and a resource shape would be a lie), serving
the archive over the API.

- [x] `spec.session.name` + validation; capture writes there (default
  = task name); explicit rows survive task delete
- [x] `continueFrom: session:NAME` resolution (no phase check, missing
  row is ProvisionFailed)
- [x] store: `last_task` migration + backfill, ListSessions
- [x] sessions API endpoints; `px get/describe/delete session`
- [x] `px run --continue-session`
- [x] `px_sessions` gauge; SessionSaved message names the session
- [x] Unit tests: migration/backfill, named-capture lifetime,
  session:NAME resolution matrix, endpoint surface, CLI sugar rules
- [x] E2E on the real node (scripts/e2e-sessions.sh): a named capture
  surviving its task's delete, `--continue-session` twice against the
  same name, describe showing the references, delete session

## M6 — First-class agent runtime (done)

Chosen 2026-09-26 from four candidates (agent runtime, port exposure,
observability, user-defined templates): the primitives M3 assembled
(Model credentials, Gateway egress, Workspace code) only pay off once an
actual agent runs through them end to end — the missing piece is the
experience of launching one. Scope:

- [x] Task-level goal: `spec.goal` joins the per-workspace goals in the
  runner's GOAL env (task goal first, then workspace blocks), so a task
  without workspaces can carry an instruction — today goal only exists
  per workspace, and an agent task is goal-first by nature. Verified
  live: a task-level goal with no workspaces reaches the runner as GOAL
  and drives it to Succeeded.
- [x] Agent template `px-agent-debian12` (`template/agent/build.sh`):
  the runner template plus the Claude Code CLI baked in as the native
  release binary (a self-contained executable — no Node runtime in the
  image), fetched directly from the release CDN and verified against the
  published manifest's SHA256. `px run` defaults `spec.image` to it;
  `--image` overrides. Live finding: the installer's own `claude install`
  step re-downloads the binary through an internal client that delivered
  a truncated staging file on the lab network ("staged binary no longer
  matches the verified checksum") — the template build fetches the raw
  binary and does the same manifest check itself instead. Second live
  finding: `pct exec`'s PATH is the fixed `/sbin:/bin:/usr/sbin:/usr/bin`
  (no `/usr/local/bin`), so the CLI is symlinked into `/usr/bin` and the
  build verifies `claude --version` under that exact PATH before the
  template is frozen.
- [x] `px run`: client-side sugar that builds and applies a Task from
  flags (`--model`, `--workspace NAME[=goal]`, `--gateway`, `--ttl`,
  `--name`, `--image`, `--cores`/`--memory`) with the positional goal,
  then polls the task and follows its log to a terminal phase (2s
  polling; new log output echoed as it lands), and exits with the
  task's exit code; `--no-wait` returns after apply, Ctrl-C detaches
  with the task left running. The default runner command is
  `sh -c 'IS_SANDBOX=1 claude --dangerously-skip-permissions -p "$GOAL"'`:
  inside an LXC sandbox behind a Gateway allowlist there is no human to
  answer permission prompts, so the isolation boundary is the container,
  not the CLI's permission system (documented in the threat model);
  IS_SANDBOX=1 is the CLI's declared escape hatch for that arrangement,
  without which it refuses the flag under root — found live (first E2E
  run: the CLI exited before reaching the provider with exactly that
  refusal), fixed, and re-verified. A related live find: `px run` posts
  `yaml.Marshal` output, and without yaml tags the API types marshaled
  `apiversion`/`status` (lower-cased Go names) — the server's
  unknown-field parser rejected every apply with a 400 that unit tests
  (which decode leniently) could not see. All four kinds now carry yaml
  tags mirroring the json ones, Task.Status marshals omitempty, and a
  regression test asserts the posted yaml's `apiVersion:` casing and
  absent status block.
- [x] E2E on the real node: template builds, an agent task boots, the
  CLI is present, and a task whose Model holds a non-working key fails
  cleanly with the provider error in the log (no credential needed);
  a live agent run with a real key is a user-supervised step. Landed
  2026-09-26: `scripts/e2e-agent.sh` 15/15 (docs/e2e.md §3g) — px run
  explicit command → Succeeded; default command + spec.goal shape; CLI
  runnable in the template; task goal → GOAL; dummy-key Model fails
  cleanly with the provider error surfaced in the task log; cleanup
  leaves no containers. No credential needed, nothing left on the node.
- Explicitly out: session continuation across tasks (deferred —
  supersedes the snapshot/shared-volume assumption, see M8, which does
  it with plain capture/restore), interactive TTY exec (M4 deferral
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

## M1 — Single task on one node (MVP, done)

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

## M3 — Sandbox hardening (done)

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

## M4 — Beyond one node (done)

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
