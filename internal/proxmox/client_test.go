package proxmox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// mockPVE simulates the handful of PVE API endpoints px uses.
type mockPVE struct {
	t          *testing.T
	mux        *http.ServeMux
	cloneCount int
	nextID     int
	taskDone   bool
	taskExit   string // exitstatus reported once the task is done
}

func newMockPVE(t *testing.T) (*Client, *mockPVE) {
	t.Helper()
	m := &mockPVE{t: t, mux: http.NewServeMux(), nextID: 142, taskDone: true, taskExit: "OK"}

	m.mux.HandleFunc("/api2/json/cluster/nextid", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"data": "142"}`))
	})
	m.mux.HandleFunc("/api2/json/nodes/n1/lxc", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"data": [{"vmid": 9000, "name": "px-runner-debian12", "template": 1}]}`))
			return
		}
		http.NotFound(w, r)
	})
	m.mux.HandleFunc("/api2/json/nodes/n1/lxc/9000/clone", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("newid") != "142" || r.Form.Get("name") != "px-t1" {
			t.Errorf("clone form: %v", r.Form)
		}
		m.cloneCount++
		w.Write([]byte(`{"data": "UPID:n1:0001:0002:updateresourcemanager:clone"}`))
	})
	m.mux.HandleFunc("/api2/json/nodes/n1/lxc/142/status/start", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"data": "UPID:n1:0001:0002:updateresourcemanager:start"}`))
	})
	m.mux.HandleFunc("/api2/json/nodes/n1/lxc/142/status/stop", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"data": "UPID:n1:0001:0002:updateresourcemanager:stop"}`))
	})
	m.mux.HandleFunc("/api2/json/nodes/n1/lxc/142/status/current", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"data": {"status": "running"}}`))
	})
	m.mux.HandleFunc("/api2/json/nodes/n1/lxc/142", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"data": "UPID:n1:0001:0002:updateresourcemanager:destroy"}`))
	})
	m.mux.HandleFunc("/api2/json/nodes/n1/tasks/", func(w http.ResponseWriter, _ *http.Request) {
		status := "stopped"
		if !m.taskDone {
			status = "running"
		}
		w.Write([]byte(`{"data": {"status": "` + status + `", "exitstatus": "` + m.taskExit + `"}}`))
	})

	srv := httptest.NewServer(m.mux)
	t.Cleanup(srv.Close)
	c := New(srv.URL, "n1", "root@pam!px=fake")
	return c, m
}

func TestCloneStartFlow(t *testing.T) {
	c, m := newMockPVE(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.CloneContainer(ctx, 9000, 142, "px-t1"); err != nil {
		t.Fatalf("clone: %v", err)
	}
	if m.cloneCount != 1 {
		t.Fatalf("want 1 clone, got %d", m.cloneCount)
	}
	if err := c.StartContainer(ctx, 142); err != nil {
		t.Fatalf("start: %v", err)
	}
	running, err := c.ContainerRunning(ctx, 142)
	if err != nil || !running {
		t.Fatalf("running=%v err=%v", running, err)
	}
}

func TestNextID(t *testing.T) {
	c, _ := newMockPVE(t)
	id, err := c.NextID(context.Background())
	if err != nil || id != 142 {
		t.Fatalf("id=%d err=%v", id, err)
	}
}

func TestFindTemplate(t *testing.T) {
	c, _ := newMockPVE(t)
	vmid, err := c.FindTemplateVMID(context.Background(), "px-runner-debian12")
	if err != nil || vmid != 9000 {
		t.Fatalf("vmid=%d err=%v", vmid, err)
	}
	if _, err := c.FindTemplateVMID(context.Background(), "nope"); err == nil {
		t.Fatal("want error for missing template")
	}
}

func TestDestroyMissing(t *testing.T) {
	c, m := newMockPVE(t)
	// Simulate PVE's 500 "does not exist" response.
	m.mux.HandleFunc("/api2/json/nodes/n1/lxc/999", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"errors": {"vmid": ["does not exist"]}}`))
	})
	if err := c.DestroyContainer(context.Background(), 999); err != nil {
		t.Fatalf("destroy missing should be tolerated: %v", err)
	}
}

// A finished PVE task whose exitstatus is not "OK" must surface as an error,
// not silent success — otherwise failed stops/clones look green.
func TestTaskFailureDetected(t *testing.T) {
	c, m := newMockPVE(t)
	m.taskExit = "TASK ERROR: volume detach failed"
	if err := c.StopContainer(context.Background(), 142); err == nil {
		t.Fatal("want error for failed pve task")
	}
}

func TestAuthHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Write([]byte(`{"data": "142"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "n1", "root@pam!px=SECRET")
	_, _ = c.NextID(context.Background())
	if got != "PVEAPIToken=root@pam!px=SECRET" {
		t.Fatalf("auth header = %q", got)
	}
}
