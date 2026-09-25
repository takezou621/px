package proxmox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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
		if r.Form.Get("newid") != "142" || r.Form.Get("hostname") != "px-t1" {
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
	c := New(srv.URL, "n1", "root@pam!px=fake", false)
	return c, m
}

func TestSkipTLSVerify(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"data": "104"}`))
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	trusting := New(srv.URL, "n1", "root@pam!px=fake", true)
	if id, err := trusting.NextID(ctx); err != nil {
		t.Fatalf("NextID with skipTLSVerify: %v", err)
	} else if id != 104 {
		t.Fatalf("want id 104, got %d", id)
	}

	verifying := New(srv.URL, "n1", "root@pam!px=fake", false)
	if _, err := verifying.NextID(ctx); err == nil {
		t.Fatal("expected certificate verification to fail when skipTLSVerify is off")
	}
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

// PVE answers probes of a missing container with 500 on both endpoints px
// checks; IsNotFound must read that as "absent" while never mistaking a
// transport failure for it — treating a network blip as "already destroyed"
// would orphan the real container.
func TestIsNotFound(t *testing.T) {
	if IsNotFound(nil) {
		t.Fatal("nil must not read as not-found")
	}

	c, m := newMockPVE(t)
	notFound := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message":"Configuration file 'nodes/n1/lxc/999.conf' does not exist\n","data":null}`))
	}
	m.mux.HandleFunc("/api2/json/nodes/n1/lxc/999/config", notFound)
	m.mux.HandleFunc("/api2/json/nodes/n1/lxc/999/status/current", notFound)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := c.ContainerHostname(ctx, 999); !IsNotFound(err) {
		t.Fatalf("hostname probe of a missing container must read as not-found, got %v", err)
	}
	if _, err := c.ContainerRunning(ctx, 999); !IsNotFound(err) {
		t.Fatalf("status probe of a missing container must read as not-found, got %v", err)
	}

	// A transport failure (server gone) must not read as "absent".
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := dead.URL
	dead.Close()
	down := New(url, "n1", "root@pam!px=fake", false)
	_, err := down.ContainerRunning(context.Background(), 142)
	if err == nil || IsNotFound(err) {
		t.Fatalf("a transport failure must not read as not-found, got %v", err)
	}
}

// PVE's parameter-verification errors report reasons as plain strings,
// not arrays — the reason must survive into the error message.
func TestParamVerifyErrorReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/clone") {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"data":null,"errors":{"name":"property is not defined in schema"},"message":"Parameter verification failed."}`))
			return
		}
		w.Write([]byte(`{"data": "UPID:n1:0001:0002:updateresourcemanager:clone"}`))
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "n1", "root@pam!px=fake", false)
	err := c.CloneContainer(context.Background(), 9000, 142, "px-t1")
	if err == nil {
		t.Fatal("want error for 400 clone")
	}
	if !strings.Contains(err.Error(), "property is not defined in schema") {
		t.Fatalf("error lost the reason: %v", err)
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
	c := New(srv.URL, "n1", "root@pam!px=SECRET", false)
	_, _ = c.NextID(context.Background())
	if got != "PVEAPIToken=root@pam!px=SECRET" {
		t.Fatalf("auth header = %q", got)
	}
}
