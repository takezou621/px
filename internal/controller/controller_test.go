package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/kawai/px/internal/apis/v1alpha1"
)

type fakeProv struct {
	mu         sync.Mutex
	createErr  error
	exitErr    error   // returned by Exit as a probe failure
	destroyErr error   // returned by Destroy until cleared
	exits      map[int]int  // vmid -> exit code; missing = still running
	dead       map[int]bool // vmids whose container is not running
	created    []int
	destroyed  []int
}

func (f *fakeProv) Create(_ context.Context, _ *v1alpha1.Task) (int, error) {
	if f.createErr != nil {
		return 0, f.createErr
	}
	vmid := 100 + len(f.created)
	f.mu.Lock()
	f.created = append(f.created, vmid)
	f.mu.Unlock()
	return vmid, nil
}

func (f *fakeProv) Exit(_ context.Context, vmid int) (*int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exitErr != nil {
		return nil, f.exitErr
	}
	if code, ok := f.exits[vmid]; ok {
		c := code
		return &c, nil
	}
	return nil, nil
}

func (f *fakeProv) Running(_ context.Context, vmid int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.dead[vmid], nil
}

func (f *fakeProv) Logs(_ context.Context, _ int) (string, error) { return "log\n", nil }

func (f *fakeProv) Destroy(_ context.Context, vmid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.destroyErr != nil {
		return f.destroyErr
	}
	f.destroyed = append(f.destroyed, vmid)
	return nil
}

type memStore struct {
	mu    sync.Mutex
	tasks map[string]*v1alpha1.Task
}

func newMemStore() *memStore { return &memStore{tasks: map[string]*v1alpha1.Task{}} }

func (m *memStore) ListTasks() ([]*v1alpha1.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*v1alpha1.Task
	for _, t := range m.tasks {
		out = append(out, copyTask(t))
	}
	return out, nil
}

func (m *memStore) UpsertTask(t *v1alpha1.Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks[t.Metadata.Name] = copyTask(t)
	return nil
}

// copyTask deep-copies what the controller mutates through returned pointers
// (spec slices, status timestamps), so tests see the same aliasing rules as
// the real store.
func copyTask(t *v1alpha1.Task) *v1alpha1.Task {
	cp := *t
	cp.Spec.Workspaces = append([]v1alpha1.TaskWorkspace(nil), t.Spec.Workspaces...)
	cp.Spec.Runner.Command = append([]string(nil), t.Spec.Runner.Command...)
	if t.Status.StartedAt != nil {
		v := *t.Status.StartedAt
		cp.Status.StartedAt = &v
	}
	if t.Status.EndedAt != nil {
		v := *t.Status.EndedAt
		cp.Status.EndedAt = &v
	}
	return &cp
}

func (m *memStore) DeleteTask(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tasks, name)
	return nil
}

func testTask(ttl int) *v1alpha1.Task {
	return &v1alpha1.Task{
		APIVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata:   v1alpha1.ObjectMeta{Name: "t1"},
		Spec: v1alpha1.TaskSpec{
			Image:                   "tmpl",
			Runner:                  v1alpha1.RunnerSpec{Command: []string{"true"}},
			TTLSecondsAfterFinished: ttl,
		},
		Status: v1alpha1.TaskStatus{Phase: v1alpha1.TaskPending},
	}
}

func runOnce(c *Controller) { c.reconcileAll(context.Background()) }

func get(t *testing.T, m *memStore, name string) *v1alpha1.Task {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.tasks[name]
	if !ok {
		t.Fatalf("task %s gone", name)
	}
	return task
}

func TestHappyPath(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	_ = st.UpsertTask(testTask(0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl) // Pending -> Running
	task := get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskRunning {
		t.Fatalf("want Running, got %s", task.Status.Phase)
	}
	if task.Status.Container != 100 {
		t.Fatalf("want vmid 100, got %d", task.Status.Container)
	}

	prov.mu.Lock()
	prov.exits[100] = 0
	prov.mu.Unlock()
	runOnce(ctl) // Running -> Succeeded
	task = get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskSucceeded {
		t.Fatalf("want Succeeded, got %s", task.Status.Phase)
	}
}

func TestFailurePath(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	_ = st.UpsertTask(testTask(0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	prov.mu.Lock()
	prov.exits[100] = 3
	prov.mu.Unlock()
	runOnce(ctl)

	task := get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskFailed {
		t.Fatalf("want Failed, got %s", task.Status.Phase)
	}
	if task.Status.ExitCode != 3 {
		t.Fatalf("want exit 3, got %d", task.Status.ExitCode)
	}
}

func TestProvisionError(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{createErr: context.DeadlineExceeded}
	_ = st.UpsertTask(testTask(0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	task := get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskProvisionFail {
		t.Fatalf("want ProvisionFailed, got %s", task.Status.Phase)
	}
}

func TestTTLCleanup(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	_ = st.UpsertTask(testTask(60))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	prov.mu.Lock()
	prov.exits[100] = 0
	prov.mu.Unlock()
	runOnce(ctl)

	task := get(t, st, "t1")
	if task.Status.Container == 0 {
		t.Fatal("container should still exist within TTL")
	}

	// Jump past the TTL.
	base := time.Now()
	ctl.now = func() time.Time { return base.Add(2 * time.Hour) }
	runOnce(ctl)

	task = get(t, st, "t1")
	if task.Status.Container != 0 {
		t.Fatalf("want container cleaned, got %d", task.Status.Container)
	}
	if len(prov.destroyed) != 1 || prov.destroyed[0] != 100 {
		t.Fatalf("want destroy [100], got %v", prov.destroyed)
	}
}

func TestDeleteRunningTask(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	_ = st.UpsertTask(testTask(0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	ctl.RequestDestroy("t1")
	runOnce(ctl)

	if _, ok := st.tasks["t1"]; ok {
		t.Fatal("task record should be deleted")
	}
	if len(prov.destroyed) != 1 || prov.destroyed[0] != 100 {
		t.Fatalf("want destroy [100], got %v", prov.destroyed)
	}
}

// A Provisioning task with no container must be failed, not retried: the
// reconciler is serial, so a tick crossing with Container==0 means the
// provision was interrupted (e.g. px-server restarted) and the container is
// possibly orphaned on the node.
func TestInterruptedProvisioning(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	task := testTask(0)
	task.Status.Phase = v1alpha1.TaskProvisioning
	_ = st.UpsertTask(task)
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	task = get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskProvisionFail {
		t.Fatalf("want ProvisionFailed, got %s", task.Status.Phase)
	}
	if len(prov.created) != 0 {
		t.Fatalf("must not re-provision, created %v", prov.created)
	}
}

func TestDeleteRetriesOnDestroyFailure(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}, destroyErr: errors.New("pve down")}
	_ = st.UpsertTask(testTask(0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl) // -> Running, vmid 100
	ctl.RequestDestroy("t1")
	runOnce(ctl)
	if _, ok := st.tasks["t1"]; !ok {
		t.Fatal("record should survive a failed destroy")
	}

	prov.mu.Lock()
	prov.destroyErr = nil
	prov.mu.Unlock()
	runOnce(ctl)
	if _, ok := st.tasks["t1"]; ok {
		t.Fatal("record should be deleted after successful destroy")
	}
	if len(prov.destroyed) != 1 || prov.destroyed[0] != 100 {
		t.Fatalf("want exactly one destroy of 100, got %v", prov.destroyed)
	}
}

// Exit probe failing while the container is gone means the runner never wrote
// an exit file and never will: the task must be failed, not spin in Running.
func TestDeadContainerMarkedFailed(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{
		exits:   map[int]int{},
		dead:    map[int]bool{},
		exitErr: fmt.Errorf("pct exec: exit=255 out=\"unable to parse runtime config\""),
	}
	_ = st.UpsertTask(testTask(0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl) // -> Running
	prov.mu.Lock()
	prov.dead[100] = true
	prov.mu.Unlock()
	runOnce(ctl)

	task := get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskFailed {
		t.Fatalf("want Failed, got %s", task.Status.Phase)
	}
	if task.Status.EndedAt == nil {
		t.Fatal("EndedAt should be set")
	}
	if task.Status.Reason == "" {
		t.Fatal("Reason should explain the dead container")
	}
}

// Conversely, an Exit probe failure while the container is still running must
// not fail the task — it is a transient probe error.
func TestExitProbeErrorWithLiveContainerKeepsRunning(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}, exitErr: errors.New("ssh connection reset")}
	_ = st.UpsertTask(testTask(0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl) // -> Running
	runOnce(ctl) // probe fails, but container is up

	task := get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskRunning {
		t.Fatalf("want still Running, got %s", task.Status.Phase)
	}
}
