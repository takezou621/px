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

// continueSource serves GET /v1/tasks/<name> with the source task and
// captures the apply body, like captureTask.
func continueSource(t *testing.T, src *v1alpha1.Task) (get func() *v1alpha1.Task) {
	t.Helper()
	var posted *v1alpha1.Task
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(src)
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var task v1alpha1.Task
		if err := yaml.Unmarshal(data, &task); err != nil {
			http.Error(w, fmt.Sprintf("bad yaml: %v", err), http.StatusBadRequest)
			return
		}
		posted = &task
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string][]string{"results": {"task.px.io/x applied"}})
	}))
	t.Cleanup(srv.Close)
	serverURL = srv.URL
	return func() *v1alpha1.Task { return posted }
}

func continueTaskSource() *v1alpha1.Task {
	return &v1alpha1.Task{
		APIVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata:   v1alpha1.ObjectMeta{Name: "src"},
		Spec: v1alpha1.TaskSpec{
			Image:                   "px-agent-debian12",
			Goal:                    "old goal",
			Runner:                  v1alpha1.RunnerSpec{Command: defaultAgentCommand},
			Resources:               v1alpha1.Resources{Cores: 4, MemoryMB: 4096},
			TTLSecondsAfterFinished: 60,
			Model:                   "m1",
			Gateway:                 "gw1",
			Workspaces:              []v1alpha1.TaskWorkspace{{Name: "ws1", Goal: "fix"}},
			Ports:                   []v1alpha1.PortSpec{{Name: "http", Port: 8080, HostPort: 31000}},
		},
	}
}

func TestCmdRunContinueCopiesSourceSpec(t *testing.T) {
	get := continueSource(t, continueTaskSource())
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	if err := cmdRun(fs, []string{"-no-wait", "-continue", "src", "next goal"}); err != nil {
		t.Fatalf("cmdRun: %v", err)
	}
	m := get()
	if m == nil {
		t.Fatal("no manifest posted")
	}
	if m.Spec.Goal != "next goal" {
		t.Errorf("goal = %q, want the new positional goal", m.Spec.Goal)
	}
	if m.Spec.Image != "px-agent-debian12" || m.Spec.Model != "m1" || m.Spec.Gateway != "gw1" {
		t.Errorf("copied spec fields: image=%q model=%q gateway=%q", m.Spec.Image, m.Spec.Model, m.Spec.Gateway)
	}
	if m.Spec.Resources.Cores != 4 || m.Spec.Resources.MemoryMB != 4096 {
		t.Errorf("resources not copied: %+v", m.Spec.Resources)
	}
	if len(m.Spec.Workspaces) != 1 || m.Spec.Workspaces[0].Name != "ws1" {
		t.Errorf("workspaces not copied: %+v", m.Spec.Workspaces)
	}
	// TTL and ports stay local to the source: a follow-up is a fresh task,
	// not a re-run of the source's expiry and exposure policy.
	if m.Spec.TTLSecondsAfterFinished != 0 {
		t.Errorf("TTL must not survive the copy, got %d", m.Spec.TTLSecondsAfterFinished)
	}
	if len(m.Spec.Ports) != 0 {
		t.Errorf("ports must not survive the copy, got %+v", m.Spec.Ports)
	}
	if m.Spec.Session == nil || m.Spec.Session.ContinueFrom != "src" {
		t.Errorf("session reference missing: %+v", m.Spec.Session)
	}
	if !slices.Equal(m.Spec.Runner.Command, continueAgentCommand) {
		t.Errorf("runner.command = %q, want the --continue variant %q", m.Spec.Runner.Command, continueAgentCommand)
	}
}

func TestCmdRunContinueRejectsCopiedFields(t *testing.T) {
	continueSource(t, continueTaskSource())
	for _, args := range [][]string{
		{"-model", "m2"},
		{"-image", "tmpl"},
		{"-workspace", "ws2"},
		{"-gateway", "gw2"},
		{"-cores", "2"},
		{"-memory", "512"},
		// Restating a default is a rejection too: the copy always wins, so
		// these would silently no-op. flag.Visit sees the flags regardless
		// of their value.
		{"-image", "px-agent-debian12"},
		{"-cores", "0"},
		{"-memory", "0"},
		{"-model", ""},
	} {
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		full := append([]string{"-no-wait", "-continue", "src"}, append(args, "goal")...)
		if err := cmdRun(fs, full); err == nil {
			t.Errorf("-continue must reject re-specified %q", args)
		}
	}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	if err := cmdRun(fs, []string{"-no-wait", "-continue", "src", "goal", "--", "claude", "--version"}); err == nil {
		t.Error("-continue must reject an explicit runner command")
	}
}

func TestCmdRunContinueKeepsNameAndTTL(t *testing.T) {
	get := continueSource(t, continueTaskSource())
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	if err := cmdRun(fs, []string{"-no-wait", "-continue", "src", "-name", "cont1", "-ttl", "120", "goal"}); err != nil {
		t.Fatalf("cmdRun: %v", err)
	}
	m := get()
	if m.Metadata.Name != "cont1" {
		t.Errorf("name = %q, want cont1", m.Metadata.Name)
	}
	if m.Spec.TTLSecondsAfterFinished != 120 {
		t.Errorf("TTL = %d, want the flag's 120", m.Spec.TTLSecondsAfterFinished)
	}
}

func TestCmdEventsArgParsing(t *testing.T) {
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		_, _ = w.Write([]byte("[]"))
	}))
	defer ts.Close()

	oldURL := serverURL
	serverURL = ts.URL
	defer func() { serverURL = oldURL }()

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no args feeds the whole history", nil, "/v1/events?limit=500"},
		{"-limit takes its value, not a task name", []string{"-limit", "50"}, "/v1/events?limit=50"},
		{"joined form before the name", []string{"-limit=50", "t1"}, "/v1/tasks/t1/events"},
		{"name only", []string{"t1"}, "/v1/tasks/t1/events"},
		{"flags after the name", []string{"t1", "-limit=10"}, "/v1/tasks/t1/events"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := cmdEvents(flag.NewFlagSet("events", flag.ContinueOnError), tc.args); err != nil {
				t.Fatalf("cmdEvents(%v): %v", tc.args, err)
			}
			if gotPath != tc.want {
				t.Errorf("requested %q, want %q", gotPath, tc.want)
			}
		})
	}
}

// continueSessionSource serves GET /v1/sessions/conv with the capture
// metadata, GET /v1/tasks/<name> with src (nil = 404: the last writer's
// record is gone), and /v1/apply like captureTask.
func continueSessionSource(t *testing.T, info v1alpha1.SessionInfo, src *v1alpha1.Task) (get func() *v1alpha1.Task) {
	t.Helper()
	var posted *v1alpha1.Task
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions/conv":
			json.NewEncoder(w).Encode(info)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/tasks/"):
			if src == nil {
				http.Error(w, "task not found", http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(src)
		default:
			data, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			var task v1alpha1.Task
			if err := yaml.Unmarshal(data, &task); err != nil {
				http.Error(w, fmt.Sprintf("bad yaml: %v", err), http.StatusBadRequest)
				return
			}
			posted = &task
			json.NewEncoder(w).Encode(map[string][]string{"results": {"task.px.io/x applied"}})
		}
	}))
	t.Cleanup(srv.Close)
	serverURL = srv.URL
	return func() *v1alpha1.Task { return posted }
}

// --continue-session resolves through the session's last writer: the spec
// is copied from that task, the session reference names the capture
// (continueFrom: session:conv) and redirects the capture back to conv, so
// one conversation keeps one name across any number of tasks.
func TestCmdRunContinueSessionCopiesLastWriter(t *testing.T) {
	info := v1alpha1.SessionInfo{Name: "conv", Bytes: 5, LastTask: "src"}
	get := continueSessionSource(t, info, continueTaskSource())
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	if err := cmdRun(fs, []string{"-no-wait", "-continue-session", "conv", "next goal"}); err != nil {
		t.Fatalf("cmdRun: %v", err)
	}
	m := get()
	if m == nil {
		t.Fatal("no manifest posted")
	}
	if m.Spec.Goal != "next goal" {
		t.Errorf("goal = %q, want the new positional goal", m.Spec.Goal)
	}
	if m.Spec.Image != "px-agent-debian12" || m.Spec.Model != "m1" || m.Spec.Gateway != "gw1" {
		t.Errorf("copied spec fields: image=%q model=%q gateway=%q", m.Spec.Image, m.Spec.Model, m.Spec.Gateway)
	}
	if m.Spec.TTLSecondsAfterFinished != 0 {
		t.Errorf("TTL must not survive the copy, got %d", m.Spec.TTLSecondsAfterFinished)
	}
	if len(m.Spec.Ports) != 0 {
		t.Errorf("ports must not survive the copy, got %+v", m.Spec.Ports)
	}
	if m.Spec.Session == nil ||
		m.Spec.Session.ContinueFrom != v1alpha1.SessionPrefix+"conv" ||
		m.Spec.Session.Name != "conv" {
		t.Errorf("session reference wrong: %+v", m.Spec.Session)
	}
	if !slices.Equal(m.Spec.Runner.Command, continueAgentCommand) {
		t.Errorf("runner.command = %q, want the --continue variant %q", m.Spec.Runner.Command, continueAgentCommand)
	}
}

// Both failure shapes must be loud — the alternatives are a silently fresh
// session (no recorded writer) or an opaque 404 (writer record gone).
func TestCmdRunContinueSessionRejectsMissingWriter(t *testing.T) {
	continueSessionSource(t, v1alpha1.SessionInfo{Name: "conv"}, nil)
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	if err := cmdRun(fs, []string{"-no-wait", "-continue-session", "conv", "goal"}); err == nil {
		t.Error("a session without a recorded writer must fail loudly")
	}
	continueSessionSource(t, v1alpha1.SessionInfo{Name: "conv", LastTask: "src"}, nil)
	fs = flag.NewFlagSet("run", flag.ContinueOnError)
	if err := cmdRun(fs, []string{"-no-wait", "-continue-session", "conv", "goal"}); err == nil {
		t.Error("a session whose last writer record is gone must fail loudly")
	}
}

func TestCmdRunContinueSessionRejectsFlagConflicts(t *testing.T) {
	continueSessionSource(t, v1alpha1.SessionInfo{Name: "conv", LastTask: "src"}, continueTaskSource())

	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	if err := cmdRun(fs, []string{"-no-wait", "-continue", "src", "-continue-session", "conv", "goal"}); err == nil {
		t.Error("-continue and -continue-session must be exclusive")
	}
	fs = flag.NewFlagSet("run", flag.ContinueOnError)
	if err := cmdRun(fs, []string{"-no-wait", "-continue-session", "conv", "-model", "m2", "goal"}); err == nil {
		t.Error("-continue-session must reject re-specified copied fields")
	}
}
