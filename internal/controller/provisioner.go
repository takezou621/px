package controller

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/kawai/px/internal/apis/v1alpha1"
	"github.com/kawai/px/internal/proxmox"
	"github.com/kawai/px/internal/sshexec"
)

// Provisioner drives one Task's sandbox through its lifecycle.
type Provisioner interface {
	// Create clones the template, starts the container and boots the runner.
	Create(ctx context.Context, t *v1alpha1.Task) (vmid int, err error)
	// Exit polls the runner's exit code; nil means still running.
	// A non-nil error means the probe itself failed (e.g. container gone).
	Exit(ctx context.Context, vmid int) (*int, error)
	// Running reports whether the task container is still up.
	Running(ctx context.Context, vmid int) (bool, error)
	// Logs returns the runner's combined output so far.
	Logs(ctx context.Context, vmid int) (string, error)
	// Destroy stops and deletes the container.
	Destroy(ctx context.Context, vmid int) error
}

type provisioner struct {
	pve *proxmox.Client
	ssh *sshexec.Executor
}

func NewProvisioner(pve *proxmox.Client, ssh *sshexec.Executor) Provisioner {
	return &provisioner{pve: pve, ssh: ssh}
}

// runnerScript builds the container-side boot script.
// Everything user-controlled is embedded base64-encoded, so no quoting
// pitfalls; the runner's output goes to /run/px/task.log, its exit code
// to /run/px/exit.
func runnerScript(t *v1alpha1.Task) string {
	goal := ""
	for i, ws := range t.Spec.Workspaces {
		if i > 0 {
			goal += "\n\n"
		}
		goal += fmt.Sprintf("## %s\n%s", ws.Name, ws.Goal)
	}
	cmd := quoteCommand(t.Spec.Runner.Command)
	return fmt.Sprintf(`#!/bin/sh
mkdir -p /run/px
printf '%%s' '%s' | base64 -d > /run/px/goal
printf '%%s' '%s' | base64 -d > /run/px/cmd.sh
nohup sh -c 'sh /run/px/cmd.sh; echo $? > /run/px/exit' > /run/px/task.log 2>&1 &
echo PX_BOOT_OK
`,
		base64.StdEncoding.EncodeToString([]byte(goal)),
		base64.StdEncoding.EncodeToString([]byte(cmd)))
}

// bootCommand wraps the boot script so it lands inside the container via
// base64. mkdir runs before the redirect on purpose: /run is tmpfs and the
// template does not carry /run/px, so the redirect would fail before
// boot.sh ever executes.
func bootCommand(vmid int, user, script string) string {
	userArg := ""
	if user != "" {
		userArg = " --user " + shellQuote(user)
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(script))
	return fmt.Sprintf("pct exec %d%s -- sh -c %s", vmid, userArg,
		shellQuote("mkdir -p /run/px && echo "+b64+" | base64 -d > /run/px/boot.sh && sh /run/px/boot.sh"))
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

func (p *provisioner) Create(ctx context.Context, t *v1alpha1.Task) (int, error) {
	templateVMID, err := p.pve.FindTemplateVMID(ctx, t.Spec.Image)
	if err != nil {
		return 0, fmt.Errorf("find template: %w", err)
	}
	vmid, err := p.pve.NextID(ctx)
	if err != nil {
		return 0, fmt.Errorf("next id: %w", err)
	}
	if err := p.pve.CloneContainer(ctx, templateVMID, vmid, "px-"+t.Metadata.Name); err != nil {
		return 0, fmt.Errorf("clone: %w", err)
	}
	// Apply resource limits post-clone (clone inherits template resources).
	if err := p.applyResources(ctx, vmid, t); err != nil {
		_ = p.Destroy(context.WithoutCancel(ctx), vmid)
		return 0, err
	}
	if err := p.pve.StartContainer(ctx, vmid); err != nil {
		_ = p.Destroy(context.WithoutCancel(ctx), vmid)
		return 0, fmt.Errorf("start: %w", err)
	}
	out, code, err := p.ssh.Run(bootCommand(vmid, t.Spec.Runner.User, runnerScript(t)), 60*time.Second)
	if err != nil || code != 0 || !strings.Contains(out, "PX_BOOT_OK") {
		_ = p.Destroy(context.WithoutCancel(ctx), vmid)
		return 0, fmt.Errorf("boot runner: exit=%d out=%q err=%v", code, strings.TrimSpace(out), err)
	}
	return vmid, nil
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

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
