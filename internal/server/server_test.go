package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kawai/px/internal/apis/v1alpha1"
	"github.com/kawai/px/internal/controller"
	"github.com/kawai/px/internal/store"
)

type nopProv struct{}

func (nopProv) Allocate(_ context.Context) (int, error) { return 0, nil }
func (nopProv) Schedule(_ context.Context, _ string) (string, error) {
	return "n1", nil
}
func (nopProv) NodeOf(_ context.Context, _ int) (string, error) {
	return "n1", nil
}
func (nopProv) Create(_ context.Context, _ *v1alpha1.Task, _ string, _ int, _ []controller.ResolvedWorkspace, _ *controller.ResolvedModel, _ *controller.ResolvedGateway, _ []byte) error {
	return nil
}
func (nopProv) CaptureSession(_ context.Context, _ string, _ int, _ string) ([]byte, error) {
	return nil, nil
}
func (nopProv) RestoreSession(_ context.Context, _ string, _ int, _ string, _ []byte) error {
	return nil
}
func (nopProv) Booted(_ context.Context, _ string, _ int) (bool, error)  { return false, nil }
func (nopProv) Exit(_ context.Context, _ string, _ int) (*int, error)    { return nil, nil }
func (nopProv) Running(_ context.Context, _ string, _ int) (bool, error) { return true, nil }
func (nopProv) Logs(_ context.Context, _ string, _ int) (string, error)  { return "", nil }
func (nopProv) Exec(_ context.Context, _ string, _ int, _ []string) (*controller.ExecResult, error) {
	return &controller.ExecResult{}, nil
}
func (nopProv) Destroy(_ context.Context, _ string, _ int) error          { return nil }
func (nopProv) DestroyOwned(_ context.Context, _, _ string, _ int) error  { return nil }
func (nopProv) Owned(_ context.Context, _, _ string, _ int) (bool, error) { return true, nil }
func (nopProv) Frozen(_ context.Context, _ string, _ int) (bool, error)   { return false, nil }
func (nopProv) Freeze(_ context.Context, _ string, _ int) error           { return nil }
func (nopProv) Thaw(_ context.Context, _ string, _ int) error             { return nil }
func (nopProv) EnsurePorts(_ context.Context, _ string, _ int, _ []controller.PortForward) (controller.PortForwardResult, error) {
	return controller.PortForwardResult{}, nil
}
func (nopProv) RemovePorts(_ context.Context, _ string, _ []int) error { return nil }

func (nopProv) Templates(_ context.Context) ([]*v1alpha1.Template, error) {
	return []*v1alpha1.Template{}, nil
}

const manifest = `
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: t1
spec:
  image: tmpl
  runner:
    command: ["true"]
`

func newTestServer(t *testing.T) (*httptest.Server, *controller.Controller) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/px.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctl := controller.New(st, nopProv{}, slog.New(slog.DiscardHandler))
	srv := httptest.NewServer(New(st, ctl, nopProv{}, slog.New(slog.DiscardHandler)).Handler())
	t.Cleanup(srv.Close)
	return srv, ctl
}

func TestApplyAndList(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, err := http.Post(srv.URL+"/v1/apply", "application/yaml", strings.NewReader(manifest))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", resp.StatusCode, body)
	}

	resp2, err := http.Get(srv.URL + "/v1/tasks")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("list: want 200, got %d", resp2.StatusCode)
	}
	data, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(data), `"name": "t1"`) {
		t.Fatalf("task t1 not listed: %s", data)
	}
}

func TestApplyInvalid(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, err := http.Post(srv.URL+"/v1/apply", "application/yaml",
		strings.NewReader("apiVersion: px.io/v1alpha1\nkind: Task\nmetadata:\n  name: BAD_NAME\nspec:\n  image: x\n  runner:\n    command: [\"true\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

// An empty manifest stream (empty stdin, truncated file) must be a loud 400,
// not a 201 no-op — the caller would otherwise read exit 0 as "applied".
func TestApplyEmpty(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, body := range []string{"", "\n", "---\n---\n"} {
		resp, err := http.Post(srv.URL+"/v1/apply", "application/yaml", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("apply of %q: want 400, got %d", body, resp.StatusCode)
		}
	}
}

func TestApplyDuplicate(t *testing.T) {
	srv, _ := newTestServer(t)
	for i := 0; i < 2; i++ {
		resp, err := http.Post(srv.URL+"/v1/apply", "application/yaml", strings.NewReader(manifest))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		want := http.StatusCreated
		if i == 1 {
			want = http.StatusConflict
		}
		if resp.StatusCode != want {
			t.Fatalf("apply %d: want %d, got %d: %s", i, want, resp.StatusCode, body)
		}
	}
}

// Explicit spec.ports hostPorts are claimed across tasks: a second apply
// pointing at a claimed port is refused at the API (409), not left for the
// controller to thrash over. hostPort 0 (auto) never collides here.
func TestApplyHostPortConflict(t *testing.T) {
	srv, _ := newTestServer(t)
	mk := func(name string, hostPort int) string {
		port := fmt.Sprintf("  ports:\n    - name: http\n      port: 8080\n")
		if hostPort != 0 {
			port += fmt.Sprintf("      hostPort: %d\n", hostPort)
		}
		return fmt.Sprintf(`
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: %s
spec:
  image: px-runner-debian12
  runner:
    command: ["true"]
%s
`, name, port)
	}

	resp, err := http.Post(srv.URL+"/v1/apply", "application/yaml", strings.NewReader(mk("a", 31234)))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first apply: want 201, got %d: %s", resp.StatusCode, body)
	}

	resp, err = http.Post(srv.URL+"/v1/apply", "application/yaml", strings.NewReader(mk("b", 31234)))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("conflicting apply: want 409, got %d: %s", resp.StatusCode, body)
	}

	// A session continuing from itself can never resolve — the source is
	// this very task, unfinished by definition. The parser cannot see the
	// name pair, so the API refuses it with 400 before anything is stored.
	// A different task's name passes the parse (400) stage by construction.
	resp, err = http.Post(srv.URL+"/v1/apply", "application/yaml", strings.NewReader(`
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: selfy
spec:
  image: px-runner-debian12
  runner:
    command: ["true"]
  session:
    continueFrom: selfy
`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("self-referencing session: want 400, got %d: %s", resp.StatusCode, body)
	}

	// Auto-assign (hostPort 0) passes the API check; the controller picks
	// a free port when the task goes Running.
	resp, err = http.Post(srv.URL+"/v1/apply", "application/yaml", strings.NewReader(mk("b", 0)))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("auto-assign apply: want 201, got %d: %s", resp.StatusCode, body)
	}
}

func TestGetUnknown(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, err := http.Get(srv.URL + "/v1/tasks/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

const workspaceManifest = `
apiVersion: px.io/v1alpha1
kind: Workspace
metadata:
  name: demo
spec:
  git:
    repo: https://example.com/demo.git
    branch: main
`

func TestWorkspaces(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, err := http.Post(srv.URL+"/v1/apply", "application/yaml", strings.NewReader(workspaceManifest))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("apply: want 201, got %d: %s", resp.StatusCode, body)
	}

	// List shows the workspace.
	listResp, err := http.Get(srv.URL + "/v1/workspaces")
	if err != nil {
		t.Fatal(err)
	}
	listData, _ := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	if listResp.StatusCode != 200 || !strings.Contains(string(listData), `"name": "demo"`) {
		t.Fatalf("list: want 200 with demo, got %d: %s", listResp.StatusCode, listData)
	}

	// Get returns the spec.
	getResp, err := http.Get(srv.URL + "/v1/workspaces/demo")
	if err != nil {
		t.Fatal(err)
	}
	getData, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if getResp.StatusCode != 200 || !strings.Contains(string(getData), "https://example.com/demo.git") {
		t.Fatalf("get: want repo in body, got %d: %s", getResp.StatusCode, getData)
	}

	// Re-apply upserts (no conflict, unlike Task).
	resp2, err := http.Post(srv.URL+"/v1/apply", "application/yaml", strings.NewReader(workspaceManifest))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("re-apply: want 201, got %d", resp2.StatusCode)
	}

	// Unknown workspace is a 404.
	resp3, err := http.Get(srv.URL + "/v1/workspaces/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp3.StatusCode)
	}

	// Delete removes it; a second delete is 404 (no idempotence for
	// declarative config), and the name is gone from get and list.
	del := func() (*http.Response, []byte, error) {
		req, err := http.NewRequest(http.MethodDelete, srv.URL+"/v1/workspaces/demo", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, body, nil
	}
	delResp, delBody, err := del()
	if err != nil {
		t.Fatal(err)
	}
	if delResp.StatusCode != 200 || !strings.Contains(string(delBody), "deleted") {
		t.Fatalf("delete: want 200 deleted, got %d: %s", delResp.StatusCode, delBody)
	}
	resp4, err := http.Get(srv.URL + "/v1/workspaces/demo")
	if err != nil {
		t.Fatal(err)
	}
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusNotFound {
		t.Fatalf("workspace must be gone after delete, got %d", resp4.StatusCode)
	}
	resp5, _, err := del()
	if err != nil {
		t.Fatal(err)
	}
	if resp5.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete: want 404, got %d", resp5.StatusCode)
	}
}

// The whole apply is one store transaction, so document order cannot matter:
// the reconcile loop never observes a Task whose referenced Workspace is
// still on its way into the store (a Task-first batch used to race the 2s
// tick into a permanent ProvisionFailed).
func TestApplyTaskBeforeWorkspace(t *testing.T) {
	srv, ctl := newTestServer(t)
	in := `
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: t-ws
spec:
  image: tmpl
  workspaces:
    - name: demo
      goal: go
  runner:
    command: ["true"]
---
apiVersion: px.io/v1alpha1
kind: Workspace
metadata:
  name: demo
spec:
  git:
    repo: https://example.com/demo.git
`
	resp, err := http.Post(srv.URL+"/v1/apply", "application/yaml", strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", resp.StatusCode, body)
	}

	// One reconcile tick must resolve the workspace reference and provision.
	ctl.ReconcileOnce(context.Background())
	getResp, err := http.Get(srv.URL + "/v1/tasks/t-ws")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if !strings.Contains(string(data), `"phase": "Running"`) {
		t.Fatalf("task must provision past workspace resolution, got: %s", data)
	}
}

func TestDelete(t *testing.T) {
	srv, ctl := newTestServer(t)
	if _, err := http.Post(srv.URL+"/v1/apply", "application/yaml", strings.NewReader(manifest)); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/tasks/t1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	// Controller reconcile removes the record (the mark is already persisted
	// by the API call above).
	ctl.ReconcileOnce(context.Background())
	resp2, _ := http.Get(srv.URL + "/v1/tasks/t1")
	resp2.Body.Close()
	if resp2.StatusCode != 404 {
		t.Fatalf("want 404 after delete, got %d", resp2.StatusCode)
	}
}

const modelManifest = `
apiVersion: px.io/v1alpha1
kind: Model
metadata:
  name: claude
spec:
  provider: anthropic
  apiKey: sk-super-secret
  baseUrl: https://proxy.example.com/v1
`

// The API key is write-only: apply accepts it, every GET returns the
// placeholder, and delete removes the record.
func TestModels(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, err := http.Post(srv.URL+"/v1/apply", "application/yaml", strings.NewReader(modelManifest))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("apply: want 201, got %d: %s", resp.StatusCode, body)
	}
	// The key is write-only: even the apply acknowledgement must not echo it.
	if strings.Contains(string(body), "sk-super-secret") {
		t.Fatalf("raw key leaked via apply response: %s", body)
	}

	getResp, err := http.Get(srv.URL + "/v1/models/claude")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if getResp.StatusCode != 200 {
		t.Fatalf("get: want 200, got %d", getResp.StatusCode)
	}
	if !strings.Contains(string(data), "<redacted>") {
		t.Fatalf("get must redact the key, got: %s", data)
	}
	if strings.Contains(string(data), "sk-super-secret") {
		t.Fatalf("raw key leaked via GET: %s", data)
	}

	listResp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	listData, _ := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	if listResp.StatusCode != 200 || !strings.Contains(string(listData), `"name": "claude"`) {
		t.Fatalf("list: want claude, got %d: %s", listResp.StatusCode, listData)
	}
	if strings.Contains(string(listData), "sk-super-secret") {
		t.Fatalf("raw key leaked via list: %s", listData)
	}

	// Re-applying a fetched Model must be rejected loudly: its apiKey is the
	// placeholder, and upserting that would silently break the next
	// provision instead of surfacing the mistake here.
	roundTrip := strings.Replace(modelManifest, "sk-super-secret", v1alpha1.RedactedAPIKey, 1)
	rtResp, err := http.Post(srv.URL+"/v1/apply", "application/yaml", strings.NewReader(roundTrip))
	if err != nil {
		t.Fatal(err)
	}
	rtBody, _ := io.ReadAll(rtResp.Body)
	rtResp.Body.Close()
	if rtResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("re-applying the redacted placeholder: want 400, got %d: %s", rtResp.StatusCode, rtBody)
	}
	if !strings.Contains(string(rtBody), "placeholder") {
		t.Fatalf("rejection must name the placeholder problem: %s", rtBody)
	}

	// Upsert replaces the spec (no conflict, unlike Task).
	resp2, err := http.Post(srv.URL+"/v1/apply", "application/yaml", strings.NewReader(modelManifest))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("re-apply: want 201, got %d", resp2.StatusCode)
	}

	delReq, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/models/claude", nil)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != 200 {
		t.Fatalf("delete: want 200, got %d", delResp.StatusCode)
	}
	goneResp, _ := http.Get(srv.URL + "/v1/models/claude")
	goneResp.Body.Close()
	if goneResp.StatusCode != 404 {
		t.Fatalf("want 404 after delete, got %d", goneResp.StatusCode)
	}
}

// execProv records the argv an exec handed it and returns canned results.
type execProv struct {
	nopProv
	mu        sync.Mutex
	argv      []string
	stdout    string
	stderr    string
	exitCode  int
	err       error
	pctFailed bool
	pctReason string
	// noOwn/ownErr shape the Owned verdict (true, nil by default).
	noOwn  bool
	ownErr error
}

func (p *execProv) Exec(_ context.Context, _ string, _ int, argv []string) (*controller.ExecResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.argv = argv
	if p.err != nil {
		return nil, p.err
	}
	return &controller.ExecResult{
		Stdout:    p.stdout,
		Stderr:    p.stderr,
		ExitCode:  p.exitCode,
		PctFailed: p.pctFailed,
		PctReason: p.pctReason,
	}, nil
}

func (p *execProv) Owned(_ context.Context, _, _ string, _ int) (bool, error) {
	return !p.noOwn, p.ownErr
}

func newExecServer(t *testing.T, prov controller.Provisioner) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/px.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctl := controller.New(st, prov, slog.New(slog.DiscardHandler))
	srv := httptest.NewServer(New(st, ctl, prov, slog.New(slog.DiscardHandler)).Handler())
	t.Cleanup(srv.Close)
	return srv, st
}

func seedExecTask(t *testing.T, st *store.Store, phase v1alpha1.TaskPhase, container int, node string) {
	t.Helper()
	tk := &v1alpha1.Task{
		APIVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata:   v1alpha1.ObjectMeta{Name: "t1"},
		Spec:       v1alpha1.TaskSpec{Image: "tmpl", Runner: v1alpha1.RunnerSpec{Command: []string{"true"}}},
		Status:     v1alpha1.TaskStatus{Phase: phase, Container: container, Node: node},
	}
	if err := st.UpsertTask(tk); err != nil {
		t.Fatal(err)
	}
}

func postExec(t *testing.T, srv *httptest.Server, body string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/tasks/t1/exec", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, data
}

// Exec needs a live sandbox: anything else is a loud 409, never a silent
// success or a hit on a container the record no longer owns.
func TestTaskExecRequiresRunningSandbox(t *testing.T) {
	prov := &execProv{}
	srv, st := newExecServer(t, prov)

	cases := []struct {
		name      string
		phase     v1alpha1.TaskPhase
		container int
		node      string
		want      int
	}{
		{"running", v1alpha1.TaskRunning, 42, "n1", http.StatusOK},
		{"running but no node", v1alpha1.TaskRunning, 42, "", http.StatusConflict},
		{"pending", v1alpha1.TaskPending, 0, "n1", http.StatusConflict},
		{"running but no container", v1alpha1.TaskRunning, 0, "n1", http.StatusConflict},
		{"failed", v1alpha1.TaskFailed, 42, "n1", http.StatusConflict},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seedExecTask(t, st, c.phase, c.container, c.node)
			resp, body := postExec(t, srv, `{"command": ["true"]}`)
			if resp.StatusCode != c.want {
				t.Fatalf("want %d, got %d: %s", c.want, resp.StatusCode, body)
			}
		})
	}

	// A task marked for deletion is going away — exec must refuse even while
	// the phase still reads Running.
	seedExecTask(t, st, v1alpha1.TaskRunning, 42, "n1")
	if err := st.MarkTaskDeleted("t1", time.Now()); err != nil {
		t.Fatal(err)
	}
	resp, body := postExec(t, srv, `{"command": ["true"]}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("deleting task: want 409, got %d: %s", resp.StatusCode, body)
	}

	// A task that does not exist is 404, not a confusion with 409.
	resp404, _ := http.Post(srv.URL+"/v1/tasks/nope/exec", "application/json", strings.NewReader(`{"command": ["true"]}`))
	resp404.Body.Close()
	if resp404.StatusCode != http.StatusNotFound {
		t.Fatalf("missing task: want 404, got %d", resp404.StatusCode)
	}
}

// The result carries the streams and the command's exit code; a non-zero exit
// is a result (200), not an error.
func TestTaskExecReturnsResult(t *testing.T) {
	prov := &execProv{stdout: "out\n", stderr: "err\n", exitCode: 3}
	srv, st := newExecServer(t, prov)
	seedExecTask(t, st, v1alpha1.TaskRunning, 42, "n1")

	resp, body := postExec(t, srv, `{"command": ["sh", "-c", "echo out; echo err >&2; exit 3"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}
	prov.mu.Lock()
	gotArgv := prov.argv
	prov.mu.Unlock()
	if diff := cmpArgs(gotArgv, []string{"sh", "-c", "echo out; echo err >&2; exit 3"}); diff != "" {
		t.Fatalf("argv mangled: %s", diff)
	}
	var out struct {
		Stdout    string `json:"stdout"`
		Stderr    string `json:"stderr"`
		ExitCode  int    `json:"exitCode"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if out.Stdout != "out\n" || out.Stderr != "err\n" || out.ExitCode != 3 || out.Truncated {
		t.Fatalf("result = %+v", out)
	}
}

// A pct-level refusal (ct not running, no verdict) still returns 200 with the
// result, but flags the refusal so the CLI can report which layer failed.
func TestTaskExecPctFailure(t *testing.T) {
	prov := &execProv{pctFailed: true, pctReason: "ct 42 is not running (pct status failed)"}
	srv, st := newExecServer(t, prov)
	seedExecTask(t, st, v1alpha1.TaskRunning, 42, "n1")

	resp, body := postExec(t, srv, `{"command": ["true"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}
	var out struct {
		PctFailed bool   `json:"pctFailed"`
		PctReason string `json:"pctReason"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if !out.PctFailed || out.PctReason != prov.pctReason {
		t.Fatalf("pct failure not surfaced: %+v", out)
	}
}

// A provisioner failure (node unreachable) is a 502 — distinct from the
// command's own non-zero exit.
func TestTaskExecProvError(t *testing.T) {
	prov := &execProv{err: errors.New("ssh down")}
	srv, st := newExecServer(t, prov)
	seedExecTask(t, st, v1alpha1.TaskRunning, 42, "n1")
	resp, body := postExec(t, srv, `{"command": ["true"]}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("want 502, got %d: %s", resp.StatusCode, body)
	}
}

// Request caps, in the apply style: an empty command, too many arguments, an
// oversized argument, a malformed body.
func TestTaskExecRejectsBadRequests(t *testing.T) {
	prov := &execProv{}
	srv, st := newExecServer(t, prov)
	seedExecTask(t, st, v1alpha1.TaskRunning, 42, "n1")

	cases := []struct {
		name string
		body string
	}{
		{"no command", `{}`},
		{"empty command", `{"command": []}`},
		{"NUL byte in argument", `{"command": ["a` + `\u0000` + `b"]}`},
		{"too many arguments", `{"command": [` + strings.TrimSuffix(strings.Repeat(`"a",`, 17), ",") + `]}`},
		{"oversize argument", `{"command": ["` + strings.Repeat("x", 4<<10+1) + `"]}`},
		{"malformed body", `{"command":`},
		{"not a list", `{"command": "true"}`},
		{"trailing garbage", `{"command": ["true"]} extra`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, body := postExec(t, srv, c.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", resp.StatusCode, body)
			}
		})
	}
	// Nothing rejected above may have reached the provisioner.
	prov.mu.Lock()
	reached := prov.argv != nil
	prov.mu.Unlock()
	if reached {
		t.Fatal("a rejected request must not reach the provisioner")
	}
}

// Empty strings are legal argv ("printf '%s\n' ”") and must pass through
// byte-exact — the quoting layer renders them as ”.
func TestTaskExecAcceptsEmptyArguments(t *testing.T) {
	prov := &execProv{}
	srv, st := newExecServer(t, prov)
	seedExecTask(t, st, v1alpha1.TaskRunning, 42, "n1")
	resp, body := postExec(t, srv, `{"command": ["printf", "%s", ""]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}
	prov.mu.Lock()
	got := prov.argv
	prov.mu.Unlock()
	if diff := cmpArgs(got, []string{"printf", "%s", ""}); diff != "" {
		t.Fatalf("argv mangled: %s", diff)
	}
}

// Exec must not reach a container that no longer belongs to the task: a
// racing delete (and PVE's CTID reuse) is caught by the hostname check.
func TestTaskExecRefusesForeignContainer(t *testing.T) {
	prov := &execProv{noOwn: true}
	srv, st := newExecServer(t, prov)
	seedExecTask(t, st, v1alpha1.TaskRunning, 42, "n1")
	resp, body := postExec(t, srv, `{"command": ["true"]}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", resp.StatusCode, body)
	}
	prov.mu.Lock()
	reached := prov.argv != nil
	prov.mu.Unlock()
	if reached {
		t.Fatal("an unowned container must not be exec'd into")
	}

	// An ownership probe failure (node unreachable) is a 502, not a guess.
	prov2 := &execProv{ownErr: errors.New("pve down")}
	srv2, st2 := newExecServer(t, prov2)
	seedExecTask(t, st2, v1alpha1.TaskRunning, 42, "n1")
	resp2, body := postExec(t, srv2, `{"command": ["true"]}`)
	if resp2.StatusCode != http.StatusBadGateway {
		t.Fatalf("want 502, got %d: %s", resp2.StatusCode, body)
	}
}

// Logs ride pct exec into the container, and a pct exec into a frozen cgroup
// blocks in the freezer until the probe timeout — so a suspended task must be
// refused at the API (409) instead of stalling the request.
func TestTaskLogsRefusedWhileSuspended(t *testing.T) {
	srv, st := newExecServer(t, &execProv{})

	phases := []v1alpha1.TaskPhase{
		v1alpha1.TaskSuspending,
		v1alpha1.TaskSuspended,
		v1alpha1.TaskResuming,
	}
	for _, phase := range phases {
		seedExecTask(t, st, phase, 42, "n1")
		resp, err := http.Get(srv.URL + "/v1/tasks/t1/logs")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("%s: want 409, got %d: %s", phase, resp.StatusCode, body)
		}
	}

	// A running task still gets its logs (nop-style prov returns empty logs
	// with a 200 here via execProv).
	seedExecTask(t, st, v1alpha1.TaskRunning, 42, "n1")
	resp, err := http.Get(srv.URL + "/v1/tasks/t1/logs")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("running task: want 200, got %d", resp.StatusCode)
	}
}

func cmpArgs(got, want []string) string {
	if len(got) != len(want) {
		return fmt.Sprintf("len %d != %d (%q vs %q)", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			return fmt.Sprintf("arg %d: %q != %q", i, got[i], want[i])
		}
	}
	return ""
}

// tmplProv serves a fixed template listing (or an error) for the discovery
// endpoint; everything else falls through to nopProv.
type tmplProv struct {
	nopProv
	tmpls []*v1alpha1.Template
	err   error
}

func (p tmplProv) Templates(_ context.Context) ([]*v1alpha1.Template, error) {
	return p.tmpls, p.err
}

func TestListTemplates(t *testing.T) {
	facts := []*v1alpha1.Template{
		{Name: "px-agent-debian12", VMID: 998, Node: "third", Unprivileged: true, DHCP: true, PxOK: true},
		{Name: "px-privileged", VMID: 997, Node: "third", Missing: []string{"unprivileged"}},
	}
	srv, _ := newExecServer(t, tmplProv{tmpls: facts})
	resp, err := http.Get(srv.URL + "/v1/templates")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var got []*v1alpha1.Template
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"pxOk":true`) {
		t.Errorf("verdict must serialize as pxOk: %s", b)
	}
	clear, _ := json.Marshal(got[0])
	if strings.Contains(string(clear), `"missing"`) {
		t.Errorf("an all-clear template must omit missing: %s", clear)
	}
	if len(got) != 2 || got[1].Missing[0] != "unprivileged" {
		t.Errorf("listing lost its verdicts: %+v", got)
	}
}

func TestListTemplatesEmptyIsArrayNot(t *testing.T) {
	srv, _ := newExecServer(t, tmplProv{})
	resp, err := http.Get(srv.URL + "/v1/templates")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(body)) != "[]" {
		t.Errorf("a template-less node must serialize as [], got: %s", body)
	}
}

func TestListTemplatesPVEUnreachable(t *testing.T) {
	srv, _ := newExecServer(t, tmplProv{err: fmt.Errorf("cluster resources: connection refused")})
	resp, err := http.Get(srv.URL + "/v1/templates")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502 (the PVE side is down, not px)", resp.StatusCode)
	}
}
