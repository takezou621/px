package v1alpha1

import (
	"fmt"
	"strings"
	"testing"
)

const sample = `
apiVersion: px.io/v1alpha1
kind: Workspace
metadata:
  name: myapp
spec:
  git:
    repo: https://github.com/example/myapp.git
    branch: main
---
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: fix-bug-123
spec:
  image: px-runner-debian12
  workspaces:
    - name: myapp
      goal: "Fix bug #123"
  runner:
    command: ["claude", "-p", "$GOAL"]
  resources:
    cores: 4
    memoryMB: 8192
  ttlSecondsAfterFinished: 3600
`

func TestParseManifests(t *testing.T) {
	objs, err := ParseManifests(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("want 2 objects, got %d", len(objs))
	}
	if objs[0].Kind != KindWorkspace || objs[0].Workspace.Git.Repo == "" {
		t.Errorf("workspace parse failed: %+v", objs[0])
	}
	task := objs[1]
	if task.Kind != KindTask {
		t.Fatalf("want Task, got %s", task.Kind)
	}
	if task.Task.Image != "px-runner-debian12" {
		t.Errorf("image = %q", task.Task.Image)
	}
	if task.Task.Resources.MemoryMB != 8192 || task.Task.Resources.Cores != 4 {
		t.Errorf("resources = %+v", task.Task.Resources)
	}
	if task.Task.Workspaces[0].Goal != "Fix bug #123" {
		t.Errorf("goal = %q", task.Task.Workspaces[0].Goal)
	}
}

func TestParseManifestsEmptyDocs(t *testing.T) {
	in := "---\n---\n" + strings.TrimSpace(sample) + "\n---\n"
	objs, err := ParseManifests(strings.NewReader(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("want 2 objects, got %d", len(objs))
	}
}

func TestParseRejectsDuplicateWorkspaceRef(t *testing.T) {
	in := `apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: t1
spec:
  image: tmpl
  workspaces:
    - name: demo
      goal: one
    - name: demo
      goal: two
  runner:
    command: ["true"]
`
	if _, err := ParseManifests(strings.NewReader(in)); err == nil {
		t.Fatal("want error for duplicated workspace name")
	}
}

func TestParseAcceptsWorkspaceRefWithoutGoal(t *testing.T) {
	in := `apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: t1
spec:
  image: tmpl
  workspaces:
    - name: demo
  runner:
    command: ["true"]
`
	objs, err := ParseManifests(strings.NewReader(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if objs[0].Task.Workspaces[0].Goal != "" {
		t.Errorf("goal should be optional, got %q", objs[0].Task.Workspaces[0].Goal)
	}
}

func TestParseRejectsUnknownKind(t *testing.T) {
	in := "apiVersion: px.io/v1alpha1\nkind: Pod\nmetadata:\n  name: x\n"
	if _, err := ParseManifests(strings.NewReader(in)); err == nil {
		t.Fatal("want error for unknown kind")
	}
}

func TestParseRejectsWrongAPIVersion(t *testing.T) {
	in := "apiVersion: v1\nkind: Task\nmetadata:\n  name: x\nspec:\n  image: t\n"
	if _, err := ParseManifests(strings.NewReader(in)); err == nil {
		t.Fatal("want error for wrong apiVersion")
	}
}

func TestParseRejectsMissingSpec(t *testing.T) {
	in := "apiVersion: px.io/v1alpha1\nkind: Task\nmetadata:\n  name: x\n"
	if _, err := ParseManifests(strings.NewReader(in)); err == nil {
		t.Fatal("want error for missing spec")
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	in := "apiVersion: px.io/v1alpha1\nkind: Task\nmetadata:\n  name: x\nspec:\n  image: t\n  runner:\n    command: [\"true\"]\n  typoField: 1\n"
	if _, err := ParseManifests(strings.NewReader(in)); err == nil {
		t.Fatal("want error for unknown field")
	}
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"a", "fix-bug-123", "task0"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("%q should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-x", "UPPER", "a_b", strings.Repeat("a", 64)} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestValidateUser(t *testing.T) {
	for _, ok := range []string{"agent", "a", "_svc", "dev-2", "1000", "0"} {
		if err := ValidateUser(ok); err != nil {
			t.Errorf("%q should be valid: %v", ok, err)
		}
	}
	// Every one of these could break out of the shellQuote'd pct exec argument.
	for _, bad := range []string{"", "root;reboot", "bad name", "-rf", "u\x27", "100000", "Agent", "a b"} {
		if err := ValidateUser(bad); err == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func taskManifest(spec string) string {
	return "apiVersion: px.io/v1alpha1\nkind: Task\nmetadata:\n  name: x\nspec:\n" + spec
}

func TestParseAcceptsRunnerUser(t *testing.T) {
	in := taskManifest("  image: t\n  runner:\n    command: [\"true\"]\n    user: agent\n")
	objs, err := ParseManifests(strings.NewReader(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if objs[0].Task.Runner.User != "agent" {
		t.Fatalf("user = %q", objs[0].Task.Runner.User)
	}
}

func TestParseRejectsBadRunnerUser(t *testing.T) {
	in := taskManifest("  image: t\n  runner:\n    command: [\"true\"]\n    user: \"bad;user\"\n")
	if _, err := ParseManifests(strings.NewReader(in)); err == nil {
		t.Fatal("want error for invalid runner.user")
	}
}

// Caps keep the base64-embedded goal and command within MAX_ARG_STRLEN
// (128 KiB per argv element on Linux).
func TestParseRejectsOversizedGoal(t *testing.T) {
	in := fmt.Sprintf(taskManifest("  image: t\n  workspaces:\n    - name: w\n      goal: %q\n  runner:\n    command: [\"true\"]\n"),
		strings.Repeat("a", MaxGoalBytes+1))
	if _, err := ParseManifests(strings.NewReader(in)); err == nil {
		t.Fatal("want error for goal over MaxGoalBytes")
	}
}

func TestParseRejectsOversizedCommand(t *testing.T) {
	big := strings.Repeat("a", MaxCommandBytes)
	in := taskManifest("  image: t\n  runner:\n    command: [\"true\", \"" + big + "\"]\n")
	if _, err := ParseManifests(strings.NewReader(in)); err == nil {
		t.Fatal("want error for command over MaxCommandBytes")
	}
}
