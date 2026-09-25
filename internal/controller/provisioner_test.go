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
	script := runnerScript(testProvTask(), nil)
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
	script := runnerScript(testProvTask(), mounts)
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
	if !strings.Contains(script, `clone_ws0() { git clone --depth 1 --branch "$(cat /run/px/ws0.branch)" "$(cat /run/px/ws0.repo)" /workspace/repo-a; }`) {
		t.Errorf("missing branch clone for repo-a:\n%s", script)
	}
	if !strings.Contains(script, `clone_ws1() { git clone --depth 1 "$(cat /run/px/ws1.repo)" /workspace/repo-b; }`) {
		t.Errorf("missing default clone for repo-b:\n%s", script)
	}
	// Each clone retries once before the boot fails.
	for _, retry := range []string{
		`clone_ws0 || { sleep 2; clone_ws0; } || { echo 'px: git clone repo-a failed' >&2; exit 1; }`,
		`clone_ws1 || { sleep 2; clone_ws1; } || { echo 'px: git clone repo-b failed' >&2; exit 1; }`,
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
	mk := strings.Index(cmd, "mkdir -p /run/px")
	rd := strings.Index(cmd, "> /run/px/boot.sh")
	if mk == -1 || rd == -1 || mk > rd {
		t.Fatalf("mkdir must precede the /run/px/boot.sh redirect: %s", cmd)
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
