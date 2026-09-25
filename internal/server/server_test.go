package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kawai/px/internal/apis/v1alpha1"
	"github.com/kawai/px/internal/controller"
	"github.com/kawai/px/internal/store"
)

type nopProv struct{}

func (nopProv) Allocate(_ context.Context) (int, error) { return 0, nil }
func (nopProv) Create(_ context.Context, _ *v1alpha1.Task, _ int, _ []controller.ResolvedWorkspace, _ *controller.ResolvedModel, _ *controller.ResolvedGateway) error {
	return nil
}
func (nopProv) Booted(_ context.Context, _ int) (bool, error)          { return false, nil }
func (nopProv) Exit(_ context.Context, _ int) (*int, error)            { return nil, nil }
func (nopProv) Running(_ context.Context, _ int) (bool, error)         { return true, nil }
func (nopProv) Logs(_ context.Context, _ int) (string, error)          { return "", nil }
func (nopProv) Destroy(_ context.Context, _ int) error                 { return nil }
func (nopProv) DestroyOwned(_ context.Context, _ string, _ int) error  { return nil }
func (nopProv) Owned(_ context.Context, _ string, _ int) (bool, error) { return true, nil }

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
