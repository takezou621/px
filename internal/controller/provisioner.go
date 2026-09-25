package controller

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
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

// Provisioner drives one Task's sandbox through its lifecycle.
type Provisioner interface {
	// Allocate returns a free VMID before any node-side work, so the task
	// record names the container from the first moment: a crash mid-provision
	// still leaves a record that delete/TTL can destroy by VMID. PVE's nextid
	// is a suggestion, not a reservation — see the controller's handling of a
	// failed Create.
	Allocate(ctx context.Context) (int, error)
	// Create clones the template into vmid, starts the container and boots
	// the runner. mounts are workspace repos cloned into the container before
	// the runner starts; model, when non-nil, carries provider credentials
	// the runner gets as environment variables. On failure it destroys any
	// partial work, so the vmid no longer names a container of ours.
	Create(ctx context.Context, t *v1alpha1.Task, vmid int, mounts []ResolvedWorkspace, model *ResolvedModel) error
	// Booted reports whether the container's boot script reached the runner
	// spawn step and touched its marker (/run/px/booted) — see runnerScript
	// for why the marker sits after the spawn. A provision interrupted by a
	// restart leaves either a live runner (marker present — adopt the task)
	// or a partial clone without a runner (marker absent — clean up).
	Booted(ctx context.Context, vmid int) (bool, error)
	// Exit polls the runner's exit code; nil means still running.
	// A non-nil error means the probe itself failed (e.g. container gone).
	Exit(ctx context.Context, vmid int) (*int, error)
	// Running reports whether the task container is still up.
	Running(ctx context.Context, vmid int) (bool, error)
	// Logs returns the runner's combined output so far.
	Logs(ctx context.Context, vmid int) (string, error)
	// Destroy stops and deletes the container.
	Destroy(ctx context.Context, vmid int) error
	// DestroyOwned stops and deletes the task container, but only after
	// verifying by hostname that the VMID really names this task's sandbox —
	// PVE's nextid is a suggestion, not a reservation, so a VMID px lost
	// track of (crash between persist and Create) may have been reused by
	// someone else. A mismatch returns an error wrapping ErrNotOwned.
	DestroyOwned(ctx context.Context, taskName string, vmid int) error
	// Owned reports whether the VMID names the task's own sandbox, by
	// hostname — same check as DestroyOwned, without destroying. A container
	// that does not exist is reported as not owned (false, nil).
	Owned(ctx context.Context, taskName string, vmid int) (bool, error)
}

type provisioner struct {
	pve *proxmox.Client
	ssh *sshexec.Executor
}

func NewProvisioner(pve *proxmox.Client, ssh *sshexec.Executor) Provisioner {
	return &provisioner{pve: pve, ssh: ssh}
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
	s.WriteString(`nohup sh -c '. /run/px/model.env 2>/dev/null; sh /run/px/cmd.sh; echo $? > /run/px/exit' > /run/px/task.log 2>&1 &
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

func (p *provisioner) Create(ctx context.Context, t *v1alpha1.Task, vmid int, mounts []ResolvedWorkspace, model *ResolvedModel) error {
	templateVMID, err := p.pve.FindTemplateVMID(ctx, t.Spec.Image)
	if err != nil {
		return fmt.Errorf("find template: %w", err)
	}
	if err := p.pve.CloneContainer(ctx, templateVMID, vmid, "px-"+t.Metadata.Name); err != nil {
		return fmt.Errorf("clone: %w", err)
	}
	// Apply resource limits post-clone (clone inherits template resources).
	if err := p.applyResources(ctx, vmid, t); err != nil {
		_ = p.Destroy(context.WithoutCancel(ctx), vmid)
		return err
	}
	if err := p.pve.StartContainer(ctx, vmid); err != nil {
		_ = p.Destroy(context.WithoutCancel(ctx), vmid)
		return fmt.Errorf("start: %w", err)
	}
	// The DHCP wait (max 20s) plus one retried clone per workspace ride on
	// top of the plain boot, so give each workspace its own 30s budget.
	bootTimeout := 60*time.Second + time.Duration(len(mounts))*30*time.Second
	out, code, err := p.ssh.Run(bootCommand(vmid, t.Spec.Runner.User, runnerScript(t, mounts, model)), bootTimeout)
	if err != nil || code != 0 || !strings.Contains(out, "PX_BOOT_OK") {
		_ = p.Destroy(context.WithoutCancel(ctx), vmid)
		return fmt.Errorf("boot runner: exit=%d out=%q err=%v", code, strings.TrimSpace(out), err)
	}
	return nil
}

func (p *provisioner) applyResources(ctx context.Context, vmid int, t *v1alpha1.Task) error {
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
	out, code, err := p.ssh.Run(fmt.Sprintf("pct set %d %s", vmid, strings.Join(parts, " ")), 30*time.Second)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("pct set: exit=%d out=%q", code, strings.TrimSpace(out))
	}
	return nil
}

func (p *provisioner) Exit(ctx context.Context, vmid int) (*int, error) {
	out, code, err := p.ssh.Run(
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
func (p *provisioner) Running(ctx context.Context, vmid int) (bool, error) {
	return p.pve.ContainerRunning(ctx, vmid)
}

// Booted checks for the marker the boot script touches right after it spawns
// the runner (see runnerScript).
func (p *provisioner) Booted(ctx context.Context, vmid int) (bool, error) {
	// The probe prints test's exit status, so a successful exec is
	// distinguishable from pct exec itself failing — a live container whose
	// clone step died prints "no" and is cleaned up, while a transient pct
	// failure on a live container must not be read as "unbooted" (that would
	// destroy a runner that may be mid-boot).
	out, code, err := p.ssh.Run(
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
	running, rerr := p.pve.ContainerRunning(ctx, vmid)
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

func (p *provisioner) Logs(ctx context.Context, vmid int) (string, error) {
	out, _, err := p.ssh.Run(
		fmt.Sprintf("pct exec %d -- sh -c %s", vmid, shellQuote("cat /run/px/task.log 2>/dev/null || echo '(no log yet)'")),
		30*time.Second)
	return out, err
}

func (p *provisioner) Destroy(ctx context.Context, vmid int) error {
	_ = p.pve.StopContainer(ctx, vmid)
	return p.pve.DestroyContainer(ctx, vmid)
}

// DestroyOwned verifies the container's hostname before destroying — see the
// interface comment for why. A container that does not exist counts as
// already destroyed, so deletes stay idempotent.
func (p *provisioner) DestroyOwned(ctx context.Context, taskName string, vmid int) error {
	host, err := p.pve.ContainerHostname(ctx, vmid)
	if err != nil {
		if proxmox.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("check ownership of ct %d: %w", vmid, err)
	}
	if host != "px-"+taskName {
		return fmt.Errorf("%w: ct %d hostname %q is not %q", ErrNotOwned, vmid, host, "px-"+taskName)
	}
	return p.Destroy(ctx, vmid)
}

// Owned reports whether the VMID names the task's own sandbox, by hostname —
// the adoption check for interrupted provisioning, which must not adopt a
// reused VMID just because it happens to carry a boot marker.
func (p *provisioner) Owned(ctx context.Context, taskName string, vmid int) (bool, error) {
	host, err := p.pve.ContainerHostname(ctx, vmid)
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
