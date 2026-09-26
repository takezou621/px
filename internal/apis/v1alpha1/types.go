// Package v1alpha1 defines the px manifest API (px.io/v1alpha1).
package v1alpha1

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	APIVersion = "px.io/v1alpha1"

	KindTask      = "Task"
	KindWorkspace = "Workspace"
	KindModel     = "Model"
	KindGateway   = "Gateway"
)

// ObjectMeta identifies a manifest object.
type ObjectMeta struct {
	Name string `json:"name" yaml:"name"`
}

// TaskSpec declares an agent task run in an LXC sandbox.
type TaskSpec struct {
	// Image is the name of the LXC template to clone (must exist on the PVE node).
	Image string `json:"image" yaml:"image"`
	// Goal is the task-level instruction the runner receives (GOAL env).
	// It joins any per-workspace goals — the task goal first, then the
	// workspace blocks — so a task without workspaces can still carry an
	// instruction (agent tasks are goal-first by nature).
	Goal string `json:"goal,omitempty" yaml:"goal,omitempty"`
	// Workspaces to wire into the task; each carries its own goal.
	Workspaces []TaskWorkspace `json:"workspaces,omitempty" yaml:"workspaces,omitempty"`
	// Runner is the command executed inside the container.
	Runner RunnerSpec `json:"runner" yaml:"runner"`
	// Resources map to LXC cores/memory limits.
	Resources Resources `json:"resources,omitempty" yaml:"resources,omitempty"`
	// TTLSecondsAfterFinished deletes the container this long after the task ends.
	TTLSecondsAfterFinished int `json:"ttlSecondsAfterFinished,omitempty" yaml:"ttlSecondsAfterFinished,omitempty"`
	// Model optionally references a Model resource by name; the runner then
	// gets the provider's credentials as environment variables.
	Model string `json:"model,omitempty" yaml:"model,omitempty"`
	// Gateway optionally references a Gateway resource by name; the
	// container then runs behind its egress allowlist (default-deny out).
	Gateway string `json:"gateway,omitempty" yaml:"gateway,omitempty"`
}

type TaskWorkspace struct {
	// Name references a Workspace resource: the controller clones its git
	// repo into /workspace/<name> before the runner starts.
	Name string `json:"name" yaml:"name"`
	// Goal is the optional instruction passed to the runner for this
	// workspace.
	Goal string `json:"goal,omitempty" yaml:"goal,omitempty"`
}

type RunnerSpec struct {
	Command []string `json:"command" yaml:"command"`
	// User to exec as inside the container (default: root).
	User string `json:"user,omitempty" yaml:"user,omitempty"`
}

type Resources struct {
	Cores    int `json:"cores,omitempty" yaml:"cores,omitempty"`
	MemoryMB int `json:"memoryMB,omitempty" yaml:"memoryMB,omitempty"`
}

// WorkspaceSpec declares a git repository to clone into task containers.
type WorkspaceSpec struct {
	Git GitSpec `json:"git" yaml:"git"`
}

type GitSpec struct {
	Repo   string `json:"repo" yaml:"repo"`
	Branch string `json:"branch,omitempty" yaml:"branch,omitempty"`
}

// ModelProvider names a supported LLM provider; each maps to the env names
// its SDK reads by default (controller.EnvNamesForProvider).
type ModelProvider string

const (
	ProviderAnthropic ModelProvider = "anthropic"
	ProviderOpenAI    ModelProvider = "openai"
)

// ModelSpec declares LLM credentials task runners receive as env vars.
type ModelSpec struct {
	// Provider selects which provider's env names the runner gets.
	Provider ModelProvider `json:"provider" yaml:"provider"`
	// APIKey is the provider credential. It is write-only: the API returns
	// "<redacted>" and px never logs it.
	APIKey string `json:"apiKey" yaml:"apiKey"`
	// BaseURL overrides the provider's default endpoint (proxy or gateway).
	BaseURL string `json:"baseUrl,omitempty" yaml:"baseUrl,omitempty"`
}

// RedactedAPIKey is the placeholder every Model read returns instead of the
// real key. Apply rejects it, so re-applying a fetched Model fails loudly
// instead of silently replacing the stored credential with the placeholder.
const RedactedAPIKey = "<redacted>"

// Model is the API representation of a Model object.
type Model struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       ModelSpec  `json:"spec"`
}

// EgressRule allows outbound traffic to one destination. A rule with ports
// but no proto is normalized to tcp at parse time; a rule with neither
// allows every protocol to the cidr.
type EgressRule struct {
	// CIDR is the destination address or range ("192.168.2.1",
	// "10.0.0.0/8", v6 allowed).
	CIDR string `json:"cidr" yaml:"cidr"`
	// Ports is the destination port list: "443", "80,443", "8000:9000".
	Ports string `json:"ports,omitempty" yaml:"ports,omitempty"`
	// Proto is "tcp" or "udp" (default tcp when ports is set).
	Proto string `json:"proto,omitempty" yaml:"proto,omitempty"`
}

// GatewaySpec declares an egress allowlist task containers run behind.
// DNS (53) and DHCP (67) are always allowed on top of the rules — name-based
// egress is unusable without DNS and the lease must survive the firewall.
type GatewaySpec struct {
	Egress []EgressRule `json:"egress" yaml:"egress"`
}

// Gateway is the API representation of a Gateway object.
type Gateway struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Metadata   ObjectMeta  `json:"metadata"`
	Spec       GatewaySpec `json:"spec"`
}

// TaskPhase is the lifecycle phase of a Task.
type TaskPhase string

const (
	TaskPending       TaskPhase = "Pending"
	TaskProvisioning  TaskPhase = "Provisioning"
	TaskRunning       TaskPhase = "Running"
	TaskSuspending    TaskPhase = "Suspending"
	TaskSuspended     TaskPhase = "Suspended"
	TaskResuming      TaskPhase = "Resuming"
	TaskSucceeded     TaskPhase = "Succeeded"
	TaskFailed        TaskPhase = "Failed"
	TaskProvisionFail TaskPhase = "ProvisionFailed"
)

// Task is the API representation of a Task object (manifest + status).
type Task struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       TaskSpec   `json:"spec"`
	Status     TaskStatus `json:"status"`
}

// Workspace is the API representation of a Workspace object.
type Workspace struct {
	APIVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	Metadata   ObjectMeta    `json:"metadata"`
	Spec       WorkspaceSpec `json:"spec"`
}

// TaskStatus is the observed state of a Task.
type TaskStatus struct {
	Phase  TaskPhase `json:"phase" yaml:"phase"`
	Reason string    `json:"reason,omitempty" yaml:"reason,omitempty"`
	// Node is the PVE cluster node the container lives on, fixed at
	// provision time (cluster mode schedules it; single-node mode stores
	// the configured node). Empty only on records provisioned before
	// multi-node existed — the controller repairs those in place.
	Node      string     `json:"node,omitempty" yaml:"node,omitempty"`
	Container int        `json:"container,omitempty" yaml:"container,omitempty"` // PVE VMID
	StartedAt *time.Time `json:"startedAt,omitempty" yaml:"startedAt,omitempty"`
	EndedAt   *time.Time `json:"endedAt,omitempty" yaml:"endedAt,omitempty"`
	ExitCode  int        `json:"exitCode,omitempty" yaml:"exitCode,omitempty"`
	// DeletionTimestamp is set once a delete was requested; the controller
	// then destroys the container and removes the record. Persisted, so a
	// px-server restart resumes an in-flight delete.
	DeletionTimestamp *time.Time `json:"deletionTimestamp,omitempty" yaml:"deletionTimestamp,omitempty"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// userRe accepts a container user name or a numeric uid (passed to pct exec).
var userRe = regexp.MustCompile(`^([a-z_][a-z0-9_-]{0,31}|[0-9]{1,5})$`)

// Size caps for values embedded base64 into the boot command line; the real
// constraint is the kernel's MAX_ARG_STRLEN (128 KiB per argument) — the
// whole boot script rides on that single SSH argv element. At the caps below
// a task's boot command stays well under the limit.
const (
	MaxGoalBytes    = 32 << 10 // task goal + all workspace goals combined
	MaxCommandBytes = 16 << 10 // total of spec.runner.command argv
	MaxRepoBytes    = 2 << 10  // per workspace spec.git.repo
	MaxBranchBytes  = 256      // per workspace spec.git.branch
	MaxWorkspaces   = 8        // per task spec.workspaces
	MaxAPIKeyBytes  = 4 << 10  // per model spec.apiKey
	MaxBaseURLBytes = 512      // per model spec.baseUrl
	MaxEgressRules  = 32       // per gateway spec.egress
	MaxCIDRBytes    = 64       // per egress rule cidr
	MaxPortsBytes   = 64       // per egress rule ports
)

// ValidateProvider checks spec.provider is a known provider.
func ValidateProvider(p ModelProvider) error {
	switch p {
	case ProviderAnthropic, ProviderOpenAI:
		return nil
	}
	return fmt.Errorf("invalid provider %q: must be one of anthropic, openai", p)
}

// ValidateName checks DNS-1123-style naming.
func ValidateName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid name %q: must be lowercase alphanumeric or '-', max 63 chars", name)
	}
	return nil
}

// ValidateUser checks spec.runner.user is a plausible container user.
func ValidateUser(user string) error {
	if !userRe.MatchString(user) {
		return fmt.Errorf("invalid runner.user %q: must be a container user name or numeric uid", user)
	}
	return nil
}

// ValidateEgress checks an egress rule survives the round-trip into PVE
// firewall rules: cidr must parse as an IP or CIDR (a hostname is rejected
// — resolving it at apply time would go stale, see the roadmap), ports must
// be a comma-separated list of ports or low:high ranges in 1..65535, and
// proto, when present, must be tcp or udp. It normalizes the proto default
// so the stored spec is always what the firewall sees.
func ValidateEgress(r *EgressRule) error {
	if r.CIDR == "" {
		return fmt.Errorf("cidr is required")
	}
	if len(r.CIDR) > MaxCIDRBytes {
		return fmt.Errorf("cidr exceeds %d bytes", MaxCIDRBytes)
	}
	if net.ParseIP(r.CIDR) == nil {
		if _, _, err := net.ParseCIDR(r.CIDR); err != nil {
			return fmt.Errorf("cidr %q: must be an IP or CIDR", r.CIDR)
		}
	}
	if len(r.Ports) > MaxPortsBytes {
		return fmt.Errorf("ports exceeds %d bytes", MaxPortsBytes)
	}
	if r.Ports != "" {
		for _, part := range strings.Split(r.Ports, ",") {
			lo, hi, ranged := strings.Cut(part, ":")
			ports := []string{lo}
			if ranged {
				ports = append(ports, hi)
			}
			nums := make([]int, 0, 2)
			for _, p := range ports {
				n, err := strconv.Atoi(p)
				// The Itoa round-trip rejects what Atoi silently accepts
				// ("+443", "0443") — PVE would refuse those as dport and
				// fail the whole rule install with an opaque error.
				if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != p {
					return fmt.Errorf("ports %q: %q is not a port (1-65535)", r.Ports, part)
				}
				nums = append(nums, n)
			}
			if ranged && nums[0] > nums[1] {
				return fmt.Errorf("ports %q: range start above end", r.Ports)
			}
		}
	}
	switch r.Proto {
	case "":
		if r.Ports != "" {
			r.Proto = "tcp"
		}
	case "tcp", "udp":
	default:
		return fmt.Errorf("invalid proto %q: must be tcp or udp", r.Proto)
	}
	return nil
}
