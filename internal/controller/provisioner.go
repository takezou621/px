package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kawai/px/internal/apis/v1alpha1"
	"github.com/kawai/px/internal/proxmox"
	"github.com/kawai/px/internal/sshexec"
)

// ErrNotOwned reports that a VMID names a container that is not the task's
// sandbox — most plausibly because PVE's unreserved nextid let an external
// party reuse the VMID after px lost track of it. The controller stops
// retrying the destroy and gives up on the container rather than touching a
// foreign one.
var ErrNotOwned = errors.New("container is not owned by the task")

// ErrGuestGone reports that no guest with a given VMID exists anywhere in
// the cluster — the container a record still names was removed out of band
// (manual pct destroy, node reinstall). The controller fails such a task
// instead of wedging on a node it can never determine.
var ErrGuestGone = errors.New("no guest with that VMID exists in the cluster")

// Provisioner drives one Task's sandbox through its lifecycle. node is the
// PVE cluster node the container lives on: single-node deployments always
// pass their configured node, cluster mode the node Schedule picked. px
// reaches every node through one API endpoint (PVE proxies cross-node
// requests) and one SSH pool (one connection per node, dialed lazily).
type Provisioner interface {
	// Allocate returns a free VMID before any node-side work, so the task
	// record names the container from the first moment: a crash mid-provision
	// still leaves a record that delete/TTL can destroy by VMID. PVE's nextid
	// is a suggestion, not a reservation — see the controller's handling of a
	// failed Create.
	Allocate(ctx context.Context) (int, error)
	// Schedule picks the node a new task's container clones onto. In cluster
	// mode (no fixed node) candidates are the nodes that are online AND hold
	// the task's image as an LXC template — restricting to template-holding
	// nodes sidesteps cross-node cloning, whose endpoint cannot name a target
	// storage; among them the one with the most free memory wins, tie-broken
	// by lower CPU load, then by name, so the choice is deterministic.
	// Single-node mode returns its fixed node without cluster calls.
	Schedule(ctx context.Context, image string) (string, error)
	// NodeOf reports which cluster node currently hosts the guest with the
	// given VMID — the repair path for records persisted before Status.Node
	// existed. A VMID that names no guest returns an error wrapping
	// ErrGuestGone.
	NodeOf(ctx context.Context, vmid int) (string, error)
	// Create clones the template into vmid on node, starts the container and
	// boots the runner. mounts are workspace repos cloned into the container
	// before the runner starts; model, when non-nil, carries provider
	// credentials the runner gets as environment variables; gw, when non-nil,
	// puts the container behind an egress allowlist (LXC firewall,
	// default-deny out). On failure it destroys any partial work, so the vmid
	// no longer names a container of ours.
	Create(ctx context.Context, t *v1alpha1.Task, node string, vmid int, mounts []ResolvedWorkspace, model *ResolvedModel, gw *ResolvedGateway) error
	// Booted reports whether the container's boot script reached the runner
	// spawn step and touched its marker (/run/px/booted) — see runnerScript
	// for why the marker sits after the spawn. A provision interrupted by a
	// restart leaves either a live runner (marker present — adopt the task)
	// or a partial clone without a runner (marker absent — clean up).
	Booted(ctx context.Context, node string, vmid int) (bool, error)
	// Exit polls the runner's exit code; nil means still running.
	// A non-nil error means the probe itself failed (e.g. container gone).
	Exit(ctx context.Context, node string, vmid int) (*int, error)
	// Running reports whether the task container is still up.
	Running(ctx context.Context, node string, vmid int) (bool, error)
	// Logs returns the runner's combined output so far.
	Logs(ctx context.Context, node string, vmid int) (string, error)
	// Exec runs one command in the task's container and returns its output
	// and exit code. A non-zero exit is a result, not an error.
	Exec(ctx context.Context, node string, vmid int, argv []string) (*ExecResult, error)
	// Destroy stops and deletes the container.
	Destroy(ctx context.Context, node string, vmid int) error
	// DestroyOwned stops and deletes the task container, but only after
	// verifying by hostname that the VMID really names this task's sandbox —
	// PVE's nextid is a suggestion, not a reservation, so a VMID px lost
	// track of (crash between persist and Create) may have been reused by
	// someone else. A mismatch returns an error wrapping ErrNotOwned.
	DestroyOwned(ctx context.Context, taskName, node string, vmid int) error
	// Owned reports whether the VMID names the task's own sandbox, by
	// hostname — same check as DestroyOwned, without destroying. A container
	// that does not exist is reported as not owned (false, nil).
	Owned(ctx context.Context, taskName, node string, vmid int) (bool, error)
	// Frozen reports whether the container's cgroup is currently frozen.
	// It goes through the node host (API pid → /proc/<pid>/cgroup), never
	// pct exec: a frozen cgroup blocks every process spawned into it,
	// including the one pct exec would use to inspect the freeze.
	Frozen(ctx context.Context, node string, vmid int) (bool, error)
	// Freeze freezes the container's cgroup (echo 1 into cgroup.freeze),
	// the memory-resident pause suspend/resume is built on. The write is
	// synchronous; the caller verifies with Frozen on a later tick.
	Freeze(ctx context.Context, node string, vmid int) error
	// Thaw lifts a Freeze (echo 0).
	Thaw(ctx context.Context, node string, vmid int) error
}

type provisioner struct {
	// pve is the cluster-wide client (NextID, ClusterResources) — calls whose
	// path is /cluster/* and node-independent.
	pve *proxmox.Client
	// nodePVE yields the API client for one node. Every client shares the
	// endpoint and token (PVE proxies cross-node requests); the factory
	// exists so node-scoped paths (/nodes/<node>/...) always name the node
	// the container actually lives on. Single-node mode and tests wrap one
	// fixed client — see fixedNodePVE.
	nodePVE func(node string) *proxmox.Client
	// ssh is the per-node command channel, dialed lazily: in cluster mode a
	// node no task ever lands on is never SSH'd.
	ssh *sshexec.Pool
	// fixedNode is set in single-node mode (-pve-node): Schedule returns it
	// untouched and no cluster discovery runs.
	fixedNode string
	// egressGate runs between start and boot for gateway tasks; nil falls
	// back to waitForEgressEnforcement. Tests stub it to keep the flow
	// deterministic without a live node.
	egressGate func(ctx context.Context, node string, vmid int) error
}

func NewProvisioner(pve *proxmox.Client, nodePVE func(string) *proxmox.Client, ssh *sshexec.Pool, fixedNode string) Provisioner {
	p := &provisioner{pve: pve, nodePVE: nodePVE, ssh: ssh, fixedNode: fixedNode}
	p.egressGate = p.waitForEgressEnforcement
	return p
}

// fixedNodePVE wraps one client as the per-node factory — what single-node
// deployments and tests use.
func fixedNodePVE(c *proxmox.Client) func(string) *proxmox.Client {
	return func(string) *proxmox.Client { return c }
}

// nodeSSH runs one command on the given node, dialing that node's executor
// on first use.
func (p *provisioner) nodeSSH(node, cmd string, timeout time.Duration) (string, int, error) {
	e, err := p.ssh.Executor(node)
	if err != nil {
		return "", -1, err
	}
	return e.Run(cmd, timeout)
}

// cgroupOpTimeout bounds one node-level cgroup probe or write: a single SSH
// round trip, no container work involved.
const cgroupOpTimeout = 15 * time.Second

// Schedule picks the node a new task's container clones onto — see the
// interface comment for the scoring.
func (p *provisioner) Schedule(ctx context.Context, image string) (string, error) {
	if p.fixedNode != "" {
		return p.fixedNode, nil
	}
	res, err := p.pve.ClusterResources(ctx)
	if err != nil {
		return "", fmt.Errorf("cluster resources: %w", err)
	}
	type candidate struct {
		node string
		free float64 // MaxMem-Mem on the node row
		cpu  float64
	}
	online := map[string]candidate{}
	var tmplNodes []string
	for _, r := range res {
		switch {
		case r.Type == "node" && r.Status == "online":
			online[r.Node] = candidate{node: r.Node, free: r.MaxMem - r.Mem, cpu: r.CPU}
		case r.Type == "lxc" && r.Template == 1 && r.Name == image:
			tmplNodes = append(tmplNodes, r.Node)
		}
	}
	var scored []candidate
	for _, node := range tmplNodes {
		c, ok := online[node]
		if !ok {
			continue
		}
		scored = append(scored, c)
	}
	if len(scored) == 0 {
		return "", fmt.Errorf("no online node holds template %q (check spec.image and the template's storage)", image)
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].free != scored[j].free {
			return scored[i].free > scored[j].free
		}
		if scored[i].cpu != scored[j].cpu {
			return scored[i].cpu < scored[j].cpu
		}
		return scored[i].node < scored[j].node
	})
	return scored[0].node, nil
}

// NodeOf reports which node hosts a guest — the repair path for records
// persisted before Status.Node existed.
func (p *provisioner) NodeOf(ctx context.Context, vmid int) (string, error) {
	res, err := p.pve.ClusterResources(ctx)
	if err != nil {
		return "", fmt.Errorf("cluster resources: %w", err)
	}
	for _, r := range res {
		if r.VMID == vmid && (r.Type == "lxc" || r.Type == "qemu") {
			return r.Node, nil
		}
	}
	return "", fmt.Errorf("vmid %d: %w", vmid, ErrGuestGone)
}

// setFrozen writes 1/0 into the container's cgroup.freeze, via the node
// host: API pid → /proc/<pid>/cgroup → the init's unified cgroup →
// cgroup.freeze. Freezing the init's cgroup stops every process in the
// container, they are all its descendants. The write is synchronous; the
// controller verifies with Frozen on a later tick rather than here, so a
// crashed write is just retried state, not a lost one.
func (p *provisioner) setFrozen(ctx context.Context, node string, vmid int, freeze bool) error {
	pid, err := p.nodePVE(node).ContainerPID(ctx, vmid)
	if err != nil {
		return err
	}
	want := "0"
	if freeze {
		want = "1"
	}
	out, code, err := p.nodeSSH(node, fmt.Sprintf(`p=$(sed -n 's/^0:://p' /proc/%d/cgroup)
[ -n "$p" ] || { echo PX_NO_CGROUP; exit 1; }
echo %s > "/sys/fs/cgroup$p/cgroup.freeze"`, pid, want), cgroupOpTimeout)
	if err != nil {
		return err
	}
	if strings.Contains(out, "PX_NO_CGROUP") {
		return fmt.Errorf("ct %d: init pid %d has no unified cgroup", vmid, pid)
	}
	if code != 0 {
		return fmt.Errorf("write cgroup.freeze=%s on node %s: exit=%d out=%q", want, node, code, strings.TrimSpace(out))
	}
	return nil
}

func (p *provisioner) Freeze(ctx context.Context, node string, vmid int) error {
	return p.setFrozen(ctx, node, vmid, true)
}

func (p *provisioner) Thaw(ctx context.Context, node string, vmid int) error {
	return p.setFrozen(ctx, node, vmid, false)
}

func (p *provisioner) Frozen(ctx context.Context, node string, vmid int) (bool, error) {
	pid, err := p.nodePVE(node).ContainerPID(ctx, vmid)
	if err != nil {
		return false, err
	}
	out, code, err := p.nodeSSH(node, fmt.Sprintf(`p=$(sed -n 's/^0:://p' /proc/%d/cgroup)
[ -n "$p" ] || { echo PX_NO_CGROUP; exit 1; }
[ -r "/sys/fs/cgroup$p/cgroup.events" ] || { echo PX_NO_EVENTS; exit 1; }
grep -qx 'frozen 1' "/sys/fs/cgroup$p/cgroup.events" && echo PX_FROZEN=1 || echo PX_FROZEN=0`, pid), cgroupOpTimeout)
	if err != nil {
		return false, err
	}
	if strings.Contains(out, "PX_NO_CGROUP") {
		return false, fmt.Errorf("ct %d: init pid %d has no unified cgroup", vmid, pid)
	}
	if strings.Contains(out, "PX_NO_EVENTS") {
		// An unreadable cgroup.events must not read as "not frozen": that
		// would confirm a Resuming task while the cgroup may still be frozen,
		// and the next pct exec would hang inside the freezer.
		return false, fmt.Errorf("ct %d: cgroup.events unreadable (cgroup path %q)", vmid, strings.TrimSpace(out))
	}
	if code != 0 {
		return false, fmt.Errorf("freeze probe on node %s: exit=%d out=%q", node, code, strings.TrimSpace(out))
	}
	switch {
	case strings.Contains(out, "PX_FROZEN=1"):
		return true, nil
	case strings.Contains(out, "PX_FROZEN=0"):
		return false, nil
	default:
		return false, fmt.Errorf("freeze probe: unexpected output %q", strings.TrimSpace(out))
	}
}

// ResolvedWorkspace is a task workspace reference resolved against the
// store: the repo the boot script clones into /workspace/<name> before the
// runner starts.
type ResolvedWorkspace struct {
	Name   string
	Repo   string
	Branch string
}

// ResolvedModel is a task model reference resolved against the store: the
// provider credentials the boot script hands the runner as env vars. The
// APIKey rides the same base64 embedding as every other user-controlled
// boot input — it never appears raw in a command line or a log.
type ResolvedModel struct {
	Provider v1alpha1.ModelProvider
	APIKey   string
	BaseURL  string
}

// ResolvedGateway is a task gateway reference resolved against the store:
// the egress allowlist the container runs behind — default-deny outbound
// with one ACCEPT rule per entry, via the LXC firewall.
type ResolvedGateway struct {
	Name   string
	Egress []v1alpha1.EgressRule
}

// envPrefix maps a provider to its credential env names. Apply-time
// validation keeps Provider within the known set, so the default is safe.
func envPrefix(p v1alpha1.ModelProvider) string {
	if p == v1alpha1.ProviderOpenAI {
		return "OPENAI"
	}
	return "ANTHROPIC"
}

// runnerScript builds the container-side boot script.
// Everything user-controlled is embedded base64-encoded, so no quoting
// pitfalls; the runner's output goes to /run/px/task.log, its exit code
// to /run/px/exit.
func runnerScript(t *v1alpha1.Task, mounts []ResolvedWorkspace, model *ResolvedModel) string {
	goal := ""
	for i, ws := range t.Spec.Workspaces {
		if i > 0 {
			goal += "\n\n"
		}
		goal += fmt.Sprintf("## %s\n%s", ws.Name, ws.Goal)
	}
	cmd := quoteCommand(t.Spec.Runner.Command)
	var s strings.Builder
	// umask 077 before any writes: everything under /run/px is this task's
	// private state, and the credential files (model.key, model.env, and
	// boot.sh itself with its base64 embeddings) must not fall back to the
	// container's default umask (0644 with the usual 022).
	s.WriteString("#!/bin/sh\numask 077\nmkdir -p /run/px")
	if len(mounts) > 0 {
		s.WriteString("\nmkdir -p /workspace")
	}
	fmt.Fprintf(&s, `
printf '%%s' '%s' | base64 -d > /run/px/goal
printf '%%s' '%s' | base64 -d > /run/px/cmd.sh
`,
		base64.StdEncoding.EncodeToString([]byte(goal)),
		base64.StdEncoding.EncodeToString([]byte(cmd)))
	if model != nil {
		prefix := envPrefix(model.Provider)
		fmt.Fprintf(&s, "printf '%%s' '%s' | base64 -d > /run/px/model.key\n",
			base64.StdEncoding.EncodeToString([]byte(model.APIKey)))
		var env strings.Builder
		fmt.Fprintf(&env, "export %s_API_KEY=\"$(cat /run/px/model.key)\"\n", prefix)
		if model.BaseURL != "" {
			fmt.Fprintf(&s, "printf '%%s' '%s' | base64 -d > /run/px/model.baseurl\n",
				base64.StdEncoding.EncodeToString([]byte(model.BaseURL)))
			fmt.Fprintf(&env, "export %s_BASE_URL=\"$(cat /run/px/model.baseurl)\"\n", prefix)
		}
		fmt.Fprintf(&s, "printf '%%s' '%s' | base64 -d > /run/px/model.env\n",
			base64.StdEncoding.EncodeToString([]byte(env.String())))
	}
	// The container was started seconds ago and DHCP may not have handed out
	// a lease yet, so the first clone would die on DNS. Wait (max 20s) for a
	// default route before any clone.
	if len(mounts) > 0 {
		s.WriteString(`i=0
while [ $i -lt 20 ]; do
  ip route 2>/dev/null | grep -q default && grep -q nameserver /etc/resolv.conf 2>/dev/null && break
  i=$((i+1))
  sleep 1
done
`)
	}
	for i, ws := range mounts {
		fmt.Fprintf(&s, "printf '%%s' '%s' | base64 -d > /run/px/ws%d.repo\n",
			base64.StdEncoding.EncodeToString([]byte(ws.Repo)), i)
		branchFlag := ""
		if ws.Branch != "" {
			fmt.Fprintf(&s, "printf '%%s' '%s' | base64 -d > /run/px/ws%d.branch\n",
				base64.StdEncoding.EncodeToString([]byte(ws.Branch)), i)
			branchFlag = fmt.Sprintf("--branch \"$(cat /run/px/ws%d.branch)\" ", i)
		}
		// ws.Name is DNS-1123-validated, safe as a path segment and inside a
		// single-quoted echo. The `--` keeps a repo string starting with '-'
		// from being parsed as a git option. The boot runs right after
		// container start, when DHCP may not have handed out a lease yet, so
		// wait for the default route (max 20s) and retry the clone once before
		// failing — the retry clears any partial clone a first attempt may
		// have left behind, since git refuses to clone into a non-empty dir.
		// A failed clone exits before the runner spawns, so PX_BOOT_OK is
		// never printed and Create cleans up the container.
		fmt.Fprintf(&s, `clone_ws%d() { git clone --depth 1 %s"$(cat /run/px/ws%d.repo)" -- /workspace/%s; }
clone_ws%d || { rm -rf /workspace/%s; sleep 2; clone_ws%d; } || { echo 'px: git clone %s failed' >&2; exit 1; }
`, i, branchFlag, i, ws.Name, i, ws.Name, i, ws.Name)
	}
	// The model.env source must be an [ -f ] guard, not ". file 2>/dev/null":
	// under dash (the template's /bin/sh) a dot-builtin that cannot open its
	// file is a fatal error for the shell, so the runner would die before
	// cmd.sh ever starts — a model-less task then hangs in Running forever.
	s.WriteString(`nohup sh -c 'if [ -f /run/px/model.env ]; then . /run/px/model.env; fi; sh /run/px/cmd.sh; echo $? > /run/px/exit' > /run/px/task.log 2>&1 &
# marker for Booted(), touched only after the runner is spawned so that a
# present marker proves the runner process exists — a restart mid-boot can
# then tell a live runner from a partial clone whose boot died with the SSH
# session.
touch /run/px/booted
echo PX_BOOT_OK
`)
	return s.String()
}

// bootCommand wraps the boot script so it lands inside the container via
// base64. mkdir runs before the redirect on purpose: /run is tmpfs and the
// template does not carry /run/px, so the redirect would fail before
// boot.sh ever executes. The umask precedes the redirect too, so boot.sh —
// which embeds the model credential base64-encoded — is not left
// world-readable by the outer shell's default umask.
func bootCommand(vmid int, user, script string) string {
	userArg := ""
	if user != "" {
		userArg = " --user " + shellQuote(user)
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(script))
	return fmt.Sprintf("pct exec %d%s -- sh -c %s", vmid, userArg,
		shellQuote("umask 077; mkdir -p /run/px && echo "+b64+" | base64 -d > /run/px/boot.sh && sh /run/px/boot.sh"))
}

func quoteCommand(argv []string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\nexport GOAL=\"$(cat /run/px/goal)\"\n")
	for i, a := range argv {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString("'" + strings.ReplaceAll(a, "'", `'\''`) + "'")
	}
	b.WriteString("\n")
	return b.String()
}

func (p *provisioner) Allocate(ctx context.Context) (int, error) {
	return p.pve.NextID(ctx)
}

func (p *provisioner) Create(ctx context.Context, t *v1alpha1.Task, node string, vmid int, mounts []ResolvedWorkspace, model *ResolvedModel, gw *ResolvedGateway) error {
	pve := p.nodePVE(node)
	templateVMID, err := pve.FindTemplateVMID(ctx, t.Spec.Image)
	if err != nil {
		return fmt.Errorf("find template: %w", err)
	}
	if err := pve.CloneContainer(ctx, templateVMID, vmid, "px-"+t.Metadata.Name); err != nil {
		return fmt.Errorf("clone: %w", err)
	}
	// The sandbox contract is an unprivileged uid mapping, but the clone
	// endpoint cannot request one (PVE rejects unknown params) — it only
	// inherits the flag from the template. Verify the inheritance so a
	// privileged template fails the provision instead of shipping a
	// sandbox the threat model does not cover.
	unpriv, err := pve.ContainerUnprivileged(ctx, vmid)
	if err != nil {
		_ = p.Destroy(context.WithoutCancel(ctx), node, vmid)
		return fmt.Errorf("read container config: %w", err)
	}
	if !unpriv {
		_ = p.Destroy(context.WithoutCancel(ctx), node, vmid)
		return fmt.Errorf("template %q produced a privileged container; rebuild it with --unprivileged 1 (see docs/threat-model.md)", t.Spec.Image)
	}
	// Apply resource limits post-clone (clone inherits template resources).
	if err := p.applyResources(ctx, node, vmid, t); err != nil {
		_ = p.Destroy(context.WithoutCancel(ctx), node, vmid)
		return err
	}
	// The egress policy must be in place before the container starts — a
	// sandbox that boots wide open even for a second is not deny-by-default.
	if gw != nil {
		if err := p.applyEgressPolicy(ctx, node, vmid, gw); err != nil {
			_ = p.Destroy(context.WithoutCancel(ctx), node, vmid)
			return fmt.Errorf("apply egress policy %q: %w", gw.Name, err)
		}
	}
	if err := pve.StartContainer(ctx, vmid); err != nil {
		_ = p.Destroy(context.WithoutCancel(ctx), node, vmid)
		return fmt.Errorf("start: %w", err)
	}
	// The .fw config is in pmxcfs before start, but pve-firewall programs
	// the dataplane only a few seconds after the veth appears — measured ~3s
	// of unrestricted egress right after boot. Hold the runner until the
	// container's OUT chain is live; failing loud beats silently open.
	if gw != nil {
		gate := p.egressGate
		if gate == nil {
			gate = p.waitForEgressEnforcement
		}
		if err := gate(ctx, node, vmid); err != nil {
			_ = p.Destroy(context.WithoutCancel(ctx), node, vmid)
			return fmt.Errorf("wait for egress enforcement: %w", err)
		}
	}
	// The DHCP wait (max 20s) plus one retried clone per workspace ride on
	// top of the plain boot, so give each workspace its own 30s budget.
	bootTimeout := 60*time.Second + time.Duration(len(mounts))*30*time.Second
	out, code, err := p.nodeSSH(node, bootCommand(vmid, t.Spec.Runner.User, runnerScript(t, mounts, model)), bootTimeout)
	if err != nil || code != 0 || !strings.Contains(out, "PX_BOOT_OK") {
		_ = p.Destroy(context.WithoutCancel(ctx), node, vmid)
		return fmt.Errorf("boot runner: exit=%d out=%q err=%v", code, strings.TrimSpace(out), err)
	}
	return nil
}

// applyEgressPolicy installs the gateway's allowlist on the not-yet-started
// clone: verify the datacenter firewall is on (without it PVE ignores every
// guest rule), enable the guest firewall on net0 + the firewall options
// (enable + policy_out=DROP), then the implicit DNS/DHCP allows and one
// ACCEPT rule per spec rule. The container is still stopped, so no
// hot-apply ordering concerns.
func (p *provisioner) applyEgressPolicy(ctx context.Context, node string, vmid int, gw *ResolvedGateway) error {
	pve := p.nodePVE(node)
	enabled, err := pve.ClusterFirewallEnabled(ctx)
	if err != nil {
		return fmt.Errorf("check cluster firewall: %w", err)
	}
	if !enabled {
		return fmt.Errorf("cluster firewall is disabled — guest egress rules are inert without it; enable it first (Datacenter > Firewall > Options, or: pvesh set /cluster/firewall/options -enable 1)")
	}
	net0, err := pve.ContainerNet0(ctx, vmid)
	if err != nil {
		return fmt.Errorf("read net0: %w", err)
	}
	net0, err = net0WithFirewall(net0)
	if err != nil {
		return err
	}
	if err := pve.EnableFirewall(ctx, vmid, net0); err != nil {
		return fmt.Errorf("enable firewall: %w", err)
	}
	if err := pve.SetEgressDropPolicy(ctx, vmid); err != nil {
		return fmt.Errorf("set egress drop policy: %w", err)
	}
	// Name resolution and the DHCP lease must survive the firewall: nearly
	// every real egress is name-based, and the template boots with ip=dhcp.
	implicit := []proxmox.FirewallRule{
		{Proto: "udp", Dport: "53", Comment: "px: dns"},
		{Proto: "tcp", Dport: "53", Comment: "px: dns"},
		{Proto: "udp", Dport: "67", Comment: "px: dhcp"},
	}
	for _, r := range implicit {
		if err := pve.AddFirewallRule(ctx, vmid, r); err != nil {
			return fmt.Errorf("add %s/%s rule: %w", r.Proto, r.Dport, err)
		}
	}
	for _, r := range gw.Egress {
		fr := proxmox.FirewallRule{
			Proto:   r.Proto,
			Dest:    r.CIDR,
			Dport:   r.Ports,
			Comment: "px: gateway " + gw.Name,
		}
		if err := pve.AddFirewallRule(ctx, vmid, fr); err != nil {
			return fmt.Errorf("add egress rule for %s: %w", r.CIDR, err)
		}
	}
	return nil
}

// waitForEgressEnforcement polls the node's iptables until pve-firewall has
// installed the container's OUT chain (veth<vmid>i0-OUT), the moment its
// egress policy actually starts dropping packets. grep exit 1 (chain not yet
// there) is the ordinary retry case, not a failure. Both address families
// are required: pve-firewall compiles the v4 and v6 rulesets independently
// and loads them in separate iptables-restore passes, so a runner released
// on the v4 chain alone could still talk over IPv6 in the gap. The v6 chain
// is generated unconditionally (v6 routing is not needed for it to exist),
// so this never wedges on a v4-only node.
func (p *provisioner) waitForEgressEnforcement(ctx context.Context, node string, vmid int) error {
	chain := fmt.Sprintf(":veth%di0-OUT", vmid)
	probe := fmt.Sprintf("iptables-save | grep -qF %s && ip6tables-save | grep -qF %s",
		shellQuote(chain), shellQuote(chain))
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, code, err := p.nodeSSH(node, probe, 10*time.Second)
		if err == nil && code == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("egress chain for CT %d not installed within 30s", vmid)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// net0WithFirewall rewrites an LXC net0 config string with firewall=1:
// a firewall key already present (from the template) is flipped in place,
// otherwise the flag is appended. The rest of the value (hwaddr, type, ...)
// must survive untouched or the clone loses its NIC identity.
func net0WithFirewall(net0 string) (string, error) {
	if net0 == "" {
		return "", fmt.Errorf("container has no net0")
	}
	parts := strings.Split(net0, ",")
	found := false
	for i, kv := range parts {
		if strings.HasPrefix(kv, "firewall=") {
			parts[i] = "firewall=1"
			found = true
		}
	}
	if !found {
		parts = append(parts, "firewall=1")
	}
	return strings.Join(parts, ","), nil
}

func (p *provisioner) applyResources(ctx context.Context, node string, vmid int, t *v1alpha1.Task) error {
	var parts []string
	if t.Spec.Resources.Cores > 0 {
		parts = append(parts, fmt.Sprintf("--cores %d", t.Spec.Resources.Cores))
	}
	if t.Spec.Resources.MemoryMB > 0 {
		parts = append(parts, fmt.Sprintf("--memory %d", t.Spec.Resources.MemoryMB))
	}
	if len(parts) == 0 {
		return nil
	}
	out, code, err := p.nodeSSH(node, fmt.Sprintf("pct set %d %s", vmid, strings.Join(parts, " ")), 30*time.Second)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("pct set: exit=%d out=%q", code, strings.TrimSpace(out))
	}
	return nil
}

func (p *provisioner) Exit(ctx context.Context, node string, vmid int) (*int, error) {
	out, code, err := p.nodeSSH(node,
		fmt.Sprintf("pct exec %d -- sh -c %s", vmid, shellQuote("cat /run/px/exit 2>/dev/null || echo __RUNNING__")),
		30*time.Second)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		// pct exec itself failed; the controller treats this as a dead
		// container after double-checking with Running.
		return nil, fmt.Errorf("pct exec: exit=%d out=%q", code, strings.TrimSpace(out))
	}
	out = strings.TrimSpace(out)
	if out == "" || out == "__RUNNING__" {
		return nil, nil
	}
	var n int
	if _, err := fmt.Sscanf(out, "%d", &n); err != nil {
		return nil, fmt.Errorf("bad exit file %q", out)
	}
	return &n, nil
}

// Running reports whether the task container is still up.
func (p *provisioner) Running(ctx context.Context, node string, vmid int) (bool, error) {
	return p.nodePVE(node).ContainerRunning(ctx, vmid)
}

// Booted checks for the marker the boot script touches right after it spawns
// the runner (see runnerScript).
func (p *provisioner) Booted(ctx context.Context, node string, vmid int) (bool, error) {
	// The probe prints test's exit status, so a successful exec is
	// distinguishable from pct exec itself failing — a live container whose
	// clone step died prints "no" and is cleaned up, while a transient pct
	// failure on a live container must not be read as "unbooted" (that would
	// destroy a runner that may be mid-boot).
	out, code, err := p.nodeSSH(node,
		fmt.Sprintf("pct exec %d -- sh -c %s", vmid, shellQuote("test -f /run/px/booted; echo PX_PROBE:$?")),
		30*time.Second)
	if err != nil {
		return false, err
	}
	if code == 0 {
		return probeVerdict(out)
	}
	// pct exec itself failed: either the container is stopped (a runner
	// cannot exist there — genuinely unbooted) or the exec died transiently
	// on a live container (retry next tick rather than destroy). A container
	// that is gone entirely counts as unbooted too, so recovery of a record
	// whose clone was already destroyed does not wedge.
	running, rerr := p.nodePVE(node).ContainerRunning(ctx, vmid)
	if rerr != nil {
		if proxmox.IsNotFound(rerr) {
			return false, nil
		}
		return false, fmt.Errorf("boot probe: pct exec failed (%d) and status check: %w", code, rerr)
	}
	if running {
		return false, fmt.Errorf("boot probe: pct exec failed (%d) while container is running", code)
	}
	return false, nil
}

// probeVerdict interprets a boot-probe exec that itself succeeded: the probe
// script prints test's exit status as PX_PROBE:<n>. Anything else is a
// retryable error, not a verdict.
func probeVerdict(out string) (bool, error) {
	i := strings.LastIndex(out, "PX_PROBE:")
	if i < 0 {
		return false, fmt.Errorf("boot probe: unexpected pct exec output %q", strings.TrimSpace(out))
	}
	return strings.TrimSpace(out[i+len("PX_PROBE:"):]) == "0", nil
}

func (p *provisioner) Logs(ctx context.Context, node string, vmid int) (string, error) {
	out, _, err := p.nodeSSH(node,
		fmt.Sprintf("pct exec %d -- sh -c %s", vmid, shellQuote("cat /run/px/task.log 2>/dev/null || echo '(no log yet)'")),
		30*time.Second)
	return out, err
}

// execTimeout bounds one `px exec` call: a command that hangs (a wrong
// foreground process, a wait on input that cannot arrive without a TTY) must
// release the SSH session and the HTTP request.
const execTimeout = 2 * time.Minute

// maxExecStreamBytes caps each exec stream; overflow sets ExecResult.Truncated
// rather than failing — partial output beats none for a debug tool.
const maxExecStreamBytes = 1 << 20

// ExecResult is one `px exec` invocation's outcome. Truncated marks a stream
// that overflowed maxExecStreamBytes (the excess is discarded, the command
// keeps running to completion).
type ExecResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Truncated bool
}

func (p *provisioner) Exec(ctx context.Context, node string, vmid int, argv []string) (*ExecResult, error) {
	// max must be set explicitly: the zero value (0) would drop every byte.
	stdout := &cappedWriter{max: maxExecStreamBytes}
	stderr := &cappedWriter{max: maxExecStreamBytes}
	e, err := p.ssh.Executor(node)
	if err != nil {
		return nil, err
	}
	// RunStreamsOnce, never a retrying path: a retry would re-run a command
	// that may have already started (repeating its side effects) and splice
	// the first attempt's partial output into the result. A failed exec is
	// final — the user re-runs it.
	code, err := e.RunStreamsOnce(pctExecCommand(vmid, argv), execTimeout, stdout, stderr)
	if err != nil {
		return nil, err
	}
	return &ExecResult{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		ExitCode:  code,
		Truncated: stdout.truncated || stderr.truncated,
	}, nil
}

// newCappedWriter is the only way to build a cappedWriter outside tests: a
// zero-value cappedWriter has max 0 and would silently discard all output.
func newCappedWriter(max int) *cappedWriter { return &cappedWriter{max: max} }

// pctExecCommand builds the node-side command line. Each argument crosses two
// shells (SSH's remote shell, then pct's argv) as a POSIX single-quoted word,
// so what the caller wrote reaches the container byte-exact — same discipline
// the boot script enforces by embedding everything base64.
func pctExecCommand(vmid int, argv []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pct exec %d --", vmid)
	for _, a := range argv {
		b.WriteByte(' ')
		b.WriteString(shellQuote(a))
	}
	return b.String()
}

// cappedWriter keeps the first max bytes of a stream and silently drops the
// rest: the ssh library copies from a pipe, so a short write or an error would
// stall or kill the command mid-run.
type cappedWriter struct {
	max       int
	buf       bytes.Buffer
	truncated bool
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room < len(p) {
		if room > 0 {
			w.buf.Write(p[:room])
		}
		w.truncated = true
		return len(p), nil
	}
	return w.buf.Write(p)
}

func (w *cappedWriter) String() string { return w.buf.String() }

func (p *provisioner) Destroy(ctx context.Context, node string, vmid int) error {
	pve := p.nodePVE(node)
	_ = pve.StopContainer(ctx, vmid)
	return pve.DestroyContainer(ctx, vmid)
}

// DestroyOwned verifies the container's hostname before destroying — see the
// interface comment for why. A container that does not exist counts as
// already destroyed, so deletes stay idempotent.
func (p *provisioner) DestroyOwned(ctx context.Context, taskName, node string, vmid int) error {
	host, err := p.nodePVE(node).ContainerHostname(ctx, vmid)
	if err != nil {
		if proxmox.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("check ownership of ct %d: %w", vmid, err)
	}
	if host != "px-"+taskName {
		return fmt.Errorf("%w: ct %d hostname %q is not %q", ErrNotOwned, vmid, host, "px-"+taskName)
	}
	return p.Destroy(ctx, node, vmid)
}

// Owned reports whether the VMID names the task's own sandbox, by hostname —
// the adoption check for interrupted provisioning, which must not adopt a
// reused VMID just because it happens to carry a boot marker.
func (p *provisioner) Owned(ctx context.Context, taskName, node string, vmid int) (bool, error) {
	host, err := p.nodePVE(node).ContainerHostname(ctx, vmid)
	if err != nil {
		if proxmox.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("check ownership of ct %d: %w", vmid, err)
	}
	return host == "px-"+taskName, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
