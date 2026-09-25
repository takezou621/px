// Package v1alpha1 defines the px manifest API (px.io/v1alpha1).
package v1alpha1

import (
	"fmt"
	"regexp"
	"time"
)

const (
	APIVersion = "px.io/v1alpha1"

	KindTask      = "Task"
	KindWorkspace = "Workspace"
)

// ObjectMeta identifies a manifest object.
type ObjectMeta struct {
	Name string `json:"name" yaml:"name"`
}

// TaskSpec declares an agent task run in an LXC sandbox.
type TaskSpec struct {
	// Image is the name of the LXC template to clone (must exist on the PVE node).
	Image string `json:"image" yaml:"image"`
	// Workspaces to wire into the task; each carries its own goal.
	Workspaces []TaskWorkspace `json:"workspaces,omitempty" yaml:"workspaces,omitempty"`
	// Runner is the command executed inside the container.
	Runner RunnerSpec `json:"runner" yaml:"runner"`
	// Resources map to LXC cores/memory limits.
	Resources Resources `json:"resources,omitempty" yaml:"resources,omitempty"`
	// TTLSecondsAfterFinished deletes the container this long after the task ends.
	TTLSecondsAfterFinished int `json:"ttlSecondsAfterFinished,omitempty" yaml:"ttlSecondsAfterFinished,omitempty"`
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

// TaskPhase is the lifecycle phase of a Task.
type TaskPhase string

const (
	TaskPending       TaskPhase = "Pending"
	TaskProvisioning  TaskPhase = "Provisioning"
	TaskRunning       TaskPhase = "Running"
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
	Phase     TaskPhase  `json:"phase" yaml:"phase"`
	Reason    string     `json:"reason,omitempty" yaml:"reason,omitempty"`
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
	MaxGoalBytes    = 32 << 10 // combined across workspaces
	MaxCommandBytes = 16 << 10 // total of spec.runner.command argv
	MaxRepoBytes    = 2 << 10  // per workspace spec.git.repo
	MaxBranchBytes  = 256      // per workspace spec.git.branch
	MaxWorkspaces   = 8        // per task spec.workspaces
)

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
