package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/kawai/px/internal/apis/v1alpha1"
	"gopkg.in/yaml.v3"
)

// captureTask serves /v1/apply, records the YAML body cmdRun posts, and
// returns a getter for the decoded manifest (and the raw body, for
// casing-sensitive checks the decode would hide).
func captureTask(t *testing.T) (get func() *v1alpha1.Task, body func() string) {
	t.Helper()
	var m *v1alpha1.Task
	var raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		raw = string(data)
		var task v1alpha1.Task
		if err := yaml.Unmarshal(data, &task); err != nil {
			http.Error(w, fmt.Sprintf("bad yaml: %v", err), http.StatusBadRequest)
			return
		}
		m = &task
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string][]string{"results": {"task.px.io/x applied"}})
	}))
	t.Cleanup(srv.Close)
	serverURL = srv.URL
	return func() *v1alpha1.Task { return m }, func() string { return raw }
}

func TestCmdRunDefaultCommandWithoutSeparator(t *testing.T) {
	get, body := captureTask(t)
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	if err := cmdRun(fs, []string{"-no-wait", "write the tests"}); err != nil {
		t.Fatalf("cmdRun: %v", err)
	}
	m := get()
	if m == nil {
		t.Fatal("no manifest posted")
	}
	if m.Spec.Goal != "write the tests" {
		t.Errorf("goal = %q, want the positional goal", m.Spec.Goal)
	}
	if !slices.Equal(m.Spec.Runner.Command, defaultAgentCommand) {
		t.Errorf("runner.command = %q, want default %q", m.Spec.Runner.Command, defaultAgentCommand)
	}
	if m.Spec.Image != "px-agent-debian12" {
		t.Errorf("image = %q, want the agent template default", m.Spec.Image)
	}
	// The real apply endpoint parses with unknown-field rejection and yaml
	// tags in their exact case: a lower-cased "apiversion" (yaml.v3's
	// untagged-field default) or a marshaled "status" block must not slip
	// through a lenient decode.
	if !strings.Contains(body(), "apiVersion: "+v1alpha1.APIVersion) {
		t.Errorf("posted yaml lacks the correctly cased apiVersion line: %q", body())
	}
	if strings.Contains(body(), "\nstatus:") {
		t.Errorf("posted yaml marshals a status block: %q", body())
	}
}

func TestCmdRunExplicitCommandAfterSeparator(t *testing.T) {
	get, _ := captureTask(t)
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	if err := cmdRun(fs, []string{"-no-wait", "sanity check", "--", "claude", "--version"}); err != nil {
		t.Fatalf("cmdRun: %v", err)
	}
	m := get()
	want := []string{"claude", "--version"}
	if !slices.Equal(m.Spec.Runner.Command, want) {
		t.Errorf("runner.command = %q, want %q", m.Spec.Runner.Command, want)
	}
}

func TestCmdRunFlagsBeforeGoal(t *testing.T) {
	get, _ := captureTask(t)
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	if err := cmdRun(fs, []string{"-no-wait", "-model", "m1", "-ttl", "60", "goal text"}); err != nil {
		t.Fatalf("cmdRun: %v", err)
	}
	m := get()
	if m.Spec.Model != "m1" || m.Spec.TTLSecondsAfterFinished != 60 {
		t.Errorf("flags not applied: model=%q ttl=%d", m.Spec.Model, m.Spec.TTLSecondsAfterFinished)
	}
	if !slices.Equal(m.Spec.Runner.Command, defaultAgentCommand) {
		t.Errorf("runner.command = %q, want default", m.Spec.Runner.Command)
	}
}

func TestCmdRunRejectsExtraPositional(t *testing.T) {
	captureTask(t)
	if err := cmdRun(flag.NewFlagSet("run", flag.ContinueOnError), []string{"-no-wait", "goal", "extra"}); err == nil {
		t.Fatal("extra positional arguments accepted silently")
	}
}

func TestSplitRunArgs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		in        []string
		wantFlags []string
		wantCmd   []string
	}{
		{"no separator", []string{"-model", "m", "goal"}, []string{"-model", "m", "goal"}, nil},
		{"with separator", []string{"goal", "--", "sh", "-c", "x"}, []string{"goal"}, []string{"sh", "-c", "x"}},
		{"separator first", []string{"--", "echo"}, nil, []string{"echo"}},
		{"empty", nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flags, cmd := splitRunArgs(tc.in)
			if !slices.Equal(flags, tc.wantFlags) || !slices.Equal(cmd, tc.wantCmd) {
				t.Errorf("splitRunArgs(%q) = %q, %q; want %q, %q", tc.in, flags, cmd, tc.wantFlags, tc.wantCmd)
			}
		})
	}
}
