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

// The workspace reference name becomes a path segment in the boot script, so
// a task must not sneak in a name the Workspace kind would have rejected.
func TestParseRejectsBadWorkspaceRefName(t *testing.T) {
	in := taskManifest("  image: t\n  workspaces:\n    - name: \"../etc\"\n      goal: x\n  runner:\n    command: [\"true\"]\n")
	if _, err := ParseManifests(strings.NewReader(in)); err == nil {
		t.Fatal("want error for invalid workspace reference name")
	}
}

func TestParseRejectsTooManyWorkspaces(t *testing.T) {
	wss := ""
	for i := 0; i <= MaxWorkspaces; i++ {
		wss += fmt.Sprintf("    - name: ws%d\n      goal: g\n", i)
	}
	in := taskManifest("  image: t\n  workspaces:\n" + wss + "  runner:\n    command: [\"true\"]\n")
	if _, err := ParseManifests(strings.NewReader(in)); err == nil {
		t.Fatal("want error for more than MaxWorkspaces entries")
	}
}

func workspaceManifestWith(spec string) string {
	return "apiVersion: px.io/v1alpha1\nkind: Workspace\nmetadata:\n  name: w\nspec:\n  git:\n" + spec
}

func TestParseRejectsOversizedRepo(t *testing.T) {
	in := workspaceManifestWith(fmt.Sprintf("    repo: %q\n", strings.Repeat("a", MaxRepoBytes+1)))
	if _, err := ParseManifests(strings.NewReader(in)); err == nil {
		t.Fatal("want error for repo over MaxRepoBytes")
	}
}

func TestParseRejectsOversizedBranch(t *testing.T) {
	in := workspaceManifestWith(fmt.Sprintf("    repo: https://example.com/a.git\n    branch: %q\n", strings.Repeat("b", MaxBranchBytes+1)))
	if _, err := ParseManifests(strings.NewReader(in)); err == nil {
		t.Fatal("want error for branch over MaxBranchBytes")
	}
}

func modelManifestWith(spec string) string {
	return "apiVersion: px.io/v1alpha1\nkind: Model\nmetadata:\n  name: m\nspec:\n" + spec
}

func TestParseModel(t *testing.T) {
	in := modelManifestWith("  provider: anthropic\n  apiKey: sk-test\n  baseUrl: https://proxy.example.com/v1\n")
	objs, err := ParseManifests(strings.NewReader(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := objs[0]
	if m.Kind != KindModel || m.Model == nil {
		t.Fatalf("want Model, got kind=%s manifest=%+v", m.Kind, m)
	}
	if m.Model.Provider != ProviderAnthropic || m.Model.APIKey != "sk-test" || m.Model.BaseURL != "https://proxy.example.com/v1" {
		t.Errorf("model spec = %+v", m.Model)
	}
}

func TestParseModelBaseUrlOptional(t *testing.T) {
	in := modelManifestWith("  provider: openai\n  apiKey: sk-oai\n")
	objs, err := ParseManifests(strings.NewReader(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if objs[0].Model.Provider != ProviderOpenAI || objs[0].Model.BaseURL != "" {
		t.Errorf("model spec = %+v", objs[0].Model)
	}
}

func TestParseRejectsUnknownProvider(t *testing.T) {
	in := modelManifestWith("  provider: mistral\n  apiKey: sk-x\n")
	if _, err := ParseManifests(strings.NewReader(in)); err == nil {
		t.Fatal("want error for provider outside the closed set")
	}
}

func TestParseRejectsMissingAPIKey(t *testing.T) {
	in := modelManifestWith("  provider: anthropic\n")
	if _, err := ParseManifests(strings.NewReader(in)); err == nil {
		t.Fatal("want error for missing spec.apiKey")
	}
}

func TestParseRejectsOversizedAPIKey(t *testing.T) {
	in := modelManifestWith(fmt.Sprintf("  provider: anthropic\n  apiKey: %q\n", strings.Repeat("k", MaxAPIKeyBytes+1)))
	if _, err := ParseManifests(strings.NewReader(in)); err == nil {
		t.Fatal("want error for apiKey over MaxAPIKeyBytes")
	}
}

func TestParseRejectsOversizedBaseURL(t *testing.T) {
	in := modelManifestWith(fmt.Sprintf("  provider: anthropic\n  apiKey: sk\n  baseUrl: %q\n", strings.Repeat("u", MaxBaseURLBytes+1)))
	if _, err := ParseManifests(strings.NewReader(in)); err == nil {
		t.Fatal("want error for baseUrl over MaxBaseURLBytes")
	}
}

// GET output carries the placeholder, and Model apply upserts — so a
// round-tripped dump must be rejected, or it would silently replace the
// real key with the placeholder and the next provision would fail obscurely.
func TestParseRejectsRedactedPlaceholder(t *testing.T) {
	in := modelManifestWith(fmt.Sprintf("  provider: anthropic\n  apiKey: %q\n", RedactedAPIKey))
	_, err := ParseManifests(strings.NewReader(in))
	if err == nil {
		t.Fatal("want error for the redaction placeholder as apiKey")
	}
	if !strings.Contains(err.Error(), "placeholder") {
		t.Errorf("error must name the placeholder problem, got: %v", err)
	}
}

// Values are injected into the runner via shell command substitution
// ("$(cat ...)"), which strips trailing newlines — a stored value with
// surrounding whitespace or control bytes would reach the runner as a
// different value than the one apply accepted, so apply rejects it.
func TestParseRejectsUnstableSecretValues(t *testing.T) {
	for _, tt := range []struct{ field, value string }{
		{"apiKey", "sk-x\n"},
		{"apiKey", " sk-x"},
		{"apiKey", "sk-x "},
		{"apiKey", "\t"},
		{"apiKey", "sk-\tx"},
		{"baseUrl", "https://proxy.example.com/v1\n"},
		{"baseUrl", " https://proxy.example.com/v1"},
	} {
		in := modelManifestWith(fmt.Sprintf("  provider: anthropic\n  %s: %q\n", tt.field, tt.value))
		if _, err := ParseManifests(strings.NewReader(in)); err == nil {
			t.Errorf("%s = %q must be rejected", tt.field, tt.value)
		}
	}
}

// The model reference resolves into boot-script env names via the stored
// Model, but the name itself rides a Go map key and a log line, so a task
// must not sneak in a name the Model kind would have rejected.
func TestParseTaskModelRefName(t *testing.T) {
	ok := taskManifest("  image: t\n  model: claude-proxy\n  runner:\n    command: [\"true\"]\n")
	objs, err := ParseManifests(strings.NewReader(ok))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if objs[0].Task.Model != "claude-proxy" {
		t.Errorf("model = %q", objs[0].Task.Model)
	}
	bad := taskManifest("  image: t\n  model: \"../etc\"\n  runner:\n    command: [\"true\"]\n")
	if _, err := ParseManifests(strings.NewReader(bad)); err == nil {
		t.Fatal("want error for invalid model reference name")
	}
}

func gatewayManifest(spec string) string {
	return "apiVersion: px.io/v1alpha1\nkind: Gateway\nmetadata:\n  name: g\nspec:\n" + spec
}

func TestParseGateway(t *testing.T) {
	in := gatewayManifest(`  egress:
    - cidr: 10.0.0.0/8
      ports: "443"
    - cidr: 192.168.2.100
      ports: "8000:9000"
      proto: udp
    - cidr: 172.16.0.1
`)
	objs, err := ParseManifests(strings.NewReader(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	g := objs[0].Gateway
	if g == nil {
		t.Fatal("Gateway spec not parsed")
	}
	if len(g.Egress) != 3 {
		t.Fatalf("want 3 rules, got %d", len(g.Egress))
	}
	// Ports without an explicit proto defaults to tcp, the overwhelmingly
	// common case for API egress.
	if g.Egress[0].Proto != "tcp" {
		t.Errorf("rule 0 proto = %q, want tcp", g.Egress[0].Proto)
	}
	if g.Egress[1].Proto != "udp" || g.Egress[1].Ports != "8000:9000" {
		t.Errorf("rule 1 = %+v", g.Egress[1])
	}
	// No ports + no proto means "any protocol, any port".
	if g.Egress[2].Ports != "" || g.Egress[2].Proto != "" {
		t.Errorf("rule 2 = %+v", g.Egress[2])
	}
}

func TestParseGatewayEmptyEgress(t *testing.T) {
	in := gatewayManifest("  egress: []\n")
	objs, err := ParseManifests(strings.NewReader(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if objs[0].Gateway == nil || len(objs[0].Gateway.Egress) != 0 {
		t.Fatalf("empty egress must parse, got %+v", objs[0].Gateway)
	}
}

func TestParseRejectsBadEgress(t *testing.T) {
	for _, spec := range []string{
		"  egress:\n    - ports: \"443\"\n",                       // cidr required
		"  egress:\n    - cidr: not-an-ip\n",                      // invalid ip/cidr
		"  egress:\n    - cidr: 10.0.0.1/99\n",                    // invalid prefix
		"  egress:\n    - cidr: 10.0.0.0/8\n      ports: \"0\"\n", // port < 1
		"  egress:\n    - cidr: 10.0.0.0/8\n      ports: \"65536\"\n",
		"  egress:\n    - cidr: 10.0.0.0/8\n      ports: \"9000:1000\"\n", // range start > end
		"  egress:\n    - cidr: 10.0.0.0/8\n      proto: icmp\n",
		"  egress:\n    - cidr: 10.0.0.0/8\n      ports: \"443\"\n      proto: sctp\n",
	} {
		if _, err := ParseManifests(strings.NewReader(gatewayManifest(spec))); err == nil {
			t.Errorf("spec %q must be rejected", spec)
		}
	}
}

func TestParseRejectsTooManyEgressRules(t *testing.T) {
	var b strings.Builder
	b.WriteString("  egress:\n")
	for i := 0; i <= MaxEgressRules; i++ {
		fmt.Fprintf(&b, "    - cidr: 10.%d.0.0/16\n", i)
	}
	if _, err := ParseManifests(strings.NewReader(gatewayManifest(b.String()))); err == nil {
		t.Fatal("want error for exceeding MaxEgressRules")
	}
}

// The gateway reference resolves into firewall rules, but the name rides a Go
// map key, so a task must not sneak in a name the Gateway kind would have
// rejected.
func TestParseTaskGatewayRefName(t *testing.T) {
	ok := taskManifest("  image: t\n  gateway: locked-down\n  runner:\n    command: [\"true\"]\n")
	objs, err := ParseManifests(strings.NewReader(ok))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if objs[0].Task.Gateway != "locked-down" {
		t.Errorf("gateway = %q", objs[0].Task.Gateway)
	}
	bad := taskManifest("  image: t\n  gateway: \"../etc\"\n  runner:\n    command: [\"true\"]\n")
	if _, err := ParseManifests(strings.NewReader(bad)); err == nil {
		t.Fatal("want error for invalid gateway reference name")
	}
}

func TestValidateEgress(t *testing.T) {
	// A bare IP (no prefix) is accepted and left as-is: PVE's firewall
	// accepts both forms.
	r := &EgressRule{CIDR: "192.168.2.100"}
	if err := ValidateEgress(r); err != nil {
		t.Fatalf("bare IP rejected: %v", err)
	}
	// An oversized cidr is rejected before parsing, so a huge string cannot
	// reach netlink or the API layer.
	if err := ValidateEgress(&EgressRule{CIDR: strings.Repeat("a", MaxCIDRBytes+1)}); err == nil {
		t.Fatal("oversized cidr must be rejected")
	}
	// Port edges: 1 and 65535 are valid, 0 and 65536 are not; an empty range
	// endpoint fails as a non-port.
	for _, ports := range []string{"1", "65535", "1,65535", "1000:2000", "443,80,8000:9000"} {
		if err := ValidateEgress(&EgressRule{CIDR: "10.0.0.0/8", Ports: ports}); err != nil {
			t.Errorf("ports %q rejected: %v", ports, err)
		}
	}
	for _, ports := range []string{"0", "65536", "1000:0", ":443", "443:", "443,,80"} {
		if err := ValidateEgress(&EgressRule{CIDR: "10.0.0.0/8", Ports: ports}); err == nil {
			t.Errorf("ports %q must be rejected", ports)
		}
	}
}
