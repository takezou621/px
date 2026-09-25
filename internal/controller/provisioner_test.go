package controller

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/kawai/px/internal/apis/v1alpha1"
)

func testProvTask() *v1alpha1.Task {
	return &v1alpha1.Task{
		APIVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata:   v1alpha1.ObjectMeta{Name: "t1"},
		Spec: v1alpha1.TaskSpec{
			Image:      "tmpl",
			Workspaces: []v1alpha1.TaskWorkspace{{Name: "ws1", Goal: "Fix bug #123"}},
			Runner:     v1alpha1.RunnerSpec{Command: []string{"claude", "-p", "it's fine"}},
		},
	}
}

func TestRunnerScriptLayout(t *testing.T) {
	script := runnerScript(testProvTask(), nil, nil)
	for _, want := range []string{
		"#!/bin/sh",
		"mkdir -p /run/px",
		"> /run/px/goal",
		"> /run/px/cmd.sh",
		"> /run/px/task.log 2>&1",
		"echo $? > /run/px/exit",
		"echo PX_BOOT_OK",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("boot script missing %q:\n%s", want, script)
		}
	}
	// The umask must precede every write, so the credential files never fall
	// back to the container's default (0644 under the usual 022).
	if !strings.HasPrefix(script, "#!/bin/sh\numask 077\n") {
		t.Errorf("boot script must set umask 077 before any writes:\n%s", script)
	}
	// The goal must be embedded base64-encoded, never raw.
	if strings.Contains(script, "Fix bug #123") {
		t.Error("goal leaked raw into boot script")
	}
	// Without mounts there must be no clone machinery, not even the DHCP wait.
	if strings.Contains(script, "ip route") || strings.Contains(script, "clone_ws") {
		t.Errorf("workspace machinery present without mounts:\n%s", script)
	}
	// The booted marker must be touched only after the runner is spawned, so
	// that a present marker proves the runner process exists.
	marker := strings.Index(script, "touch /run/px/booted")
	spawn := strings.Index(script, "nohup sh -c")
	if marker == -1 || spawn == -1 || marker < spawn {
		t.Errorf("booted marker must follow the runner spawn:\n%s", script)
	}
}

func TestRunnerScriptClonesWorkspaces(t *testing.T) {
	mounts := []ResolvedWorkspace{
		{Name: "repo-a", Repo: "https://example.com/a.git", Branch: "main"},
		{Name: "repo-b", Repo: "https://example.com/b.git"},
	}
	script := runnerScript(testProvTask(), mounts, nil)
	if !strings.Contains(script, "mkdir -p /workspace") {
		t.Errorf("missing /workspace mkdir:\n%s", script)
	}
	if !strings.Contains(script, "ip route 2>/dev/null | grep -q default") {
		t.Errorf("missing DHCP wait before the clones:\n%s", script)
	}
	// Repo and branch go in base64, so neither may appear raw in the script.
	for _, secret := range []string{"https://example.com/a.git", "main"} {
		if strings.Contains(script, secret) {
			t.Errorf("workspace input leaked raw into boot script: %q", secret)
		}
	}
	if !strings.Contains(script, `clone_ws0() { git clone --depth 1 --branch "$(cat /run/px/ws0.branch)" "$(cat /run/px/ws0.repo)" -- /workspace/repo-a; }`) {
		t.Errorf("missing branch clone for repo-a:\n%s", script)
	}
	if !strings.Contains(script, `clone_ws1() { git clone --depth 1 "$(cat /run/px/ws1.repo)" -- /workspace/repo-b; }`) {
		t.Errorf("missing default clone for repo-b:\n%s", script)
	}
	// Each clone retries once before the boot fails, clearing any partial
	// clone the first attempt left behind.
	for _, retry := range []string{
		`clone_ws0 || { rm -rf /workspace/repo-a; sleep 2; clone_ws0; } || { echo 'px: git clone repo-a failed' >&2; exit 1; }`,
		`clone_ws1 || { rm -rf /workspace/repo-b; sleep 2; clone_ws1; } || { echo 'px: git clone repo-b failed' >&2; exit 1; }`,
	} {
		if !strings.Contains(script, retry) {
			t.Errorf("missing clone retry line %q:\n%s", retry, script)
		}
	}
	// A failed clone must exit before the runner spawns, so the task lands in
	// the existing provision-failure path instead of a fake Running.
	cloneEnd := strings.Index(script, "exit 1")
	spawn := strings.Index(script, "nohup sh -c")
	if cloneEnd == -1 || spawn == -1 || cloneEnd > spawn {
		t.Errorf("clone failure handling must precede the runner spawn:\n%s", script)
	}
}

func TestQuoteCommandExportsGoalAndQuotesArgv(t *testing.T) {
	out := quoteCommand([]string{"claude", "-p", "it's fine"})
	if !strings.Contains(out, `export GOAL="$(cat /run/px/goal)"`) {
		t.Errorf("missing GOAL export:\n%s", out)
	}
	if !strings.Contains(out, `'claude' '-p' 'it'\''s fine'`) {
		t.Errorf("argv not safely quoted:\n%s", out)
	}
}

func TestBootCommandMkdirBeforeRedirect(t *testing.T) {
	cmd := bootCommand(142, "", "echo hi")
	um := strings.Index(cmd, "umask 077")
	mk := strings.Index(cmd, "mkdir -p /run/px")
	rd := strings.Index(cmd, "> /run/px/boot.sh")
	if um == -1 || mk == -1 || rd == -1 || um > mk || mk > rd {
		t.Fatalf("umask and mkdir must precede the /run/px/boot.sh redirect: %s", cmd)
	}
	if !strings.Contains(cmd, "pct exec 142 -- sh -c '") {
		t.Fatalf("bad pct invocation: %s", cmd)
	}
	// The embedded script must survive a base64 round-trip untouched.
	i := strings.Index(cmd, "echo ") + len("echo ")
	j := strings.Index(cmd[i:], " | base64")
	dec, err := base64.StdEncoding.DecodeString(cmd[i : i+j])
	if err != nil || string(dec) != "echo hi" {
		t.Fatalf("embedded script broken: %q err=%v", dec, err)
	}
}

func TestBootCommandUser(t *testing.T) {
	cmd := bootCommand(142, "agent", "echo hi")
	if !strings.Contains(cmd, "pct exec 142 --user 'agent' --") {
		t.Fatalf("user arg missing: %s", cmd)
	}
	if cmd := bootCommand(142, "", "echo hi"); strings.Contains(cmd, "--user") {
		t.Fatalf("no --user expected for empty runner.user: %s", cmd)
	}
}

// probeVerdict reads the PX_PROBE:<n> trailer a successful boot-probe exec
// prints; anything else is a retryable error, not a verdict — a garbage
// output must never be read as "unbooted" (that destroys live runners).
func TestProbeVerdict(t *testing.T) {
	if v, err := probeVerdict("PX_PROBE:0\n"); err != nil || !v {
		t.Fatalf("PX_PROBE:0 = (%v, %v), want (true, nil)", v, err)
	}
	if v, err := probeVerdict("PX_PROBE:1\n"); err != nil || v {
		t.Fatalf("PX_PROBE:1 = (%v, %v), want (false, nil)", v, err)
	}
	// Boot noise can precede the marker, so the trailer is what counts.
	if v, err := probeVerdict("some pct noise\nPX_PROBE:0\n"); err != nil || !v {
		t.Fatalf("trailer after noise = (%v, %v)", v, err)
	}
	for _, bad := range []string{"", "\n", "garbage"} {
		if _, err := probeVerdict(bad); err == nil {
			t.Errorf("output %q must be an error, not a verdict", bad)
		}
	}
}

// embeddedPayload decodes the base64 embedding the boot script carries for
// target (e.g. "/run/px/model.env"). base64 contains no single quotes, so the
// quoted token before the redirect marker is unambiguous.
func embeddedPayload(t *testing.T, script, target string) string {
	t.Helper()
	marker := "| base64 -d > " + target
	i := strings.Index(script, marker)
	if i < 0 {
		t.Fatalf("no base64 embedding for %s:\n%s", target, script)
	}
	j := strings.LastIndex(script[:i], "'")
	k := strings.LastIndex(script[:j], "'")
	if j < 0 || k < 0 {
		t.Fatalf("malformed embedding for %s:\n%s", target, script)
	}
	dec, err := base64.StdEncoding.DecodeString(script[k+1 : j])
	if err != nil {
		t.Fatalf("bad base64 payload for %s: %v", target, err)
	}
	return string(dec)
}

// A resolved model must land in the boot script base64-encoded: the key file
// holds the raw key, the env file references it by cat — neither the key nor
// the base URL may appear raw anywhere in the script, and the runner spawn
// must source the env file so the vars reach the runner and its children.
func TestRunnerScriptInjectsModelEnv(t *testing.T) {
	model := &ResolvedModel{
		Provider: v1alpha1.ProviderAnthropic,
		APIKey:   "sk-test-123",
		BaseURL:  "https://proxy.example.com/v1",
	}
	script := runnerScript(testProvTask(), nil, model)

	for _, raw := range []string{"sk-test-123", "https://proxy.example.com", "ANTHROPIC_API_KEY=\"sk"} {
		if strings.Contains(script, raw) {
			t.Errorf("credential leaked raw into boot script: %q", raw)
		}
	}
	if got := embeddedPayload(t, script, "/run/px/model.key"); got != "sk-test-123" {
		t.Errorf("model.key payload = %q, want the raw key", got)
	}
	env := embeddedPayload(t, script, "/run/px/model.env")
	for _, want := range []string{
		`export ANTHROPIC_API_KEY="$(cat /run/px/model.key)"`,
		`export ANTHROPIC_BASE_URL="$(cat /run/px/model.baseurl)"`,
	} {
		if !strings.Contains(env, want) {
			t.Errorf("model.env missing %q, got:\n%s", want, env)
		}
	}
	if !strings.Contains(script, "> /run/px/model.baseurl") {
		t.Errorf("missing model.baseurl write:\n%s", script)
	}
	spawn := strings.Index(script, "nohup sh -c")
	if spawn == -1 || !strings.Contains(script[spawn:], ". /run/px/model.env 2>/dev/null;") {
		t.Errorf("runner spawn must source model.env:\n%s", script)
	}
}

// An OpenAI model maps to the OPENAI_* names, per the provider env map.
func TestRunnerScriptOpenAIEnvPrefix(t *testing.T) {
	model := &ResolvedModel{Provider: v1alpha1.ProviderOpenAI, APIKey: "sk-oai"}
	script := runnerScript(testProvTask(), nil, model)
	env := embeddedPayload(t, script, "/run/px/model.env")
	if !strings.Contains(env, `export OPENAI_API_KEY="$(cat /run/px/model.key)"`) {
		t.Errorf("model.env missing OPENAI_API_KEY export, got:\n%s", env)
	}
	if strings.Contains(env, "BASE_URL") {
		t.Errorf("empty baseUrl must not emit a BASE_URL export, got:\n%s", env)
	}
	if strings.Contains(script, "> /run/px/model.baseurl") {
		t.Errorf("empty baseUrl must not write model.baseurl:\n%s", script)
	}
}

// A task without a model must not write any credential files. The spawn line
// still references model.env (one boot script serves both shapes), so the
// reference must be the guarded no-op source, never an unguarded one.
func TestRunnerScriptWithoutModel(t *testing.T) {
	script := runnerScript(testProvTask(), nil, nil)
	for _, absent := range []string{"> /run/px/model.key", "> /run/px/model.env", "> /run/px/model.baseurl"} {
		if strings.Contains(script, absent) {
			t.Errorf("model-less script writes %s:\n%s", absent, script)
		}
	}
	if !strings.Contains(script, ". /run/px/model.env 2>/dev/null;") {
		t.Errorf("spawn must tolerate a missing model.env:\n%s", script)
	}
}
