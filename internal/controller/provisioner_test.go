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
	script := runnerScript(testProvTask())
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
