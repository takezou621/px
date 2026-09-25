package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kawai/px/internal/apis/v1alpha1"
)

type fakeProv struct {
	mu          sync.Mutex
	allocateErr error        // returned by Allocate as a provision failure
	createErr   error        // returned by Create as a provision failure
	exitErr     error        // returned by Exit as a probe failure
	bootedErr   error        // returned by Booted as a probe failure
	destroyErr  error        // returned by Destroy until cleared
	exits       map[int]int  // vmid -> exit code; missing = still running
	dead        map[int]bool // vmids whose container is not running
	booted      map[int]bool // vmids whose container reached the runner launch
	created     []int
	mounts      []ResolvedWorkspace // mounts passed to the last Create
	model       *ResolvedModel      // model passed to the last Create
	destroyed   []int
	hostnames   map[int]string // vmid -> hostname; empty or missing = owned
}

func (f *fakeProv) Allocate(_ context.Context) (int, error) {
	if f.allocateErr != nil {
		return 0, f.allocateErr
	}
	return 100 + len(f.created), nil
}

func (f *fakeProv) Create(_ context.Context, _ *v1alpha1.Task, vmid int, mounts []ResolvedWorkspace, model *ResolvedModel) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.mu.Lock()
	f.created = append(f.created, vmid)
	f.mounts = mounts
	f.model = model
	f.mu.Unlock()
	return nil
}

func (f *fakeProv) Booted(_ context.Context, vmid int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bootedErr != nil {
		return false, f.bootedErr
	}
	return f.booted[vmid], nil
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

func (f *fakeProv) DestroyOwned(ctx context.Context, taskName string, vmid int) error {
	f.mu.Lock()
	host := f.hostnames[vmid]
	f.mu.Unlock()
	if host != "" && host != "px-"+taskName {
		return fmt.Errorf("%w: ct %d hostname %q is not %q", ErrNotOwned, vmid, host, "px-"+taskName)
	}
	return f.Destroy(ctx, vmid)
}

// Owned mirrors DestroyOwned's hostname check without destroying: empty or
// missing means owned, since plain tests never set a hostname.
func (f *fakeProv) Owned(_ context.Context, taskName string, vmid int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	host := f.hostnames[vmid]
	return host == "" || host == "px-"+taskName, nil
}

type memStore struct {
	mu          sync.Mutex
	failUpserts int // fail the next N UpsertTask calls, then succeed
	tasks       map[string]*v1alpha1.Task
	workspaces  map[string]*v1alpha1.Workspace
	models      map[string]*v1alpha1.Model
}

func newMemStore() *memStore {
	return &memStore{
		tasks:      map[string]*v1alpha1.Task{},
		workspaces: map[string]*v1alpha1.Workspace{},
		models:     map[string]*v1alpha1.Model{},
	}
}

func (m *memStore) GetModel(name string) (*v1alpha1.Model, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mo, ok := m.models[name]
	if !ok {
		return nil, errors.New("not found")
	}
	return mo, nil
}

func (m *memStore) GetWorkspace(name string) (*v1alpha1.Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws, ok := m.workspaces[name]
	if !ok {
		return nil, errors.New("not found")
	}
	return ws, nil
}

func (m *memStore) ListTasks() ([]*v1alpha1.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*v1alpha1.Task
	for _, t := range m.tasks {
		out = append(out, copyTask(t))
	}
	return out, nil
}

func (m *memStore) GetTask(name string) (*v1alpha1.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[name]
	if !ok {
		return nil, errors.New("not found")
	}
	return copyTask(t), nil
}

// UpsertTask mirrors the real store's semantics: a write based on a snapshot
// that predates a deletion request must not erase the request.
func (m *memStore) UpsertTask(t *v1alpha1.Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failUpserts > 0 {
		m.failUpserts--
		return errors.New("db broken")
	}
	cp := copyTask(t)
	if cur, ok := m.tasks[t.Metadata.Name]; ok &&
		cur.Status.DeletionTimestamp != nil && t.Status.DeletionTimestamp == nil {
		mark := *cur.Status.DeletionTimestamp
		cp.Status.DeletionTimestamp = &mark
	}
	m.tasks[t.Metadata.Name] = cp
	return nil
}

func (m *memStore) MarkTaskDeleted(name string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.tasks[name]
	if !ok {
		return errors.New("not found")
	}
	mark := at
	cur.Status.DeletionTimestamp = &mark
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
	if t.Status.DeletionTimestamp != nil {
		v := *t.Status.DeletionTimestamp
		cp.Status.DeletionTimestamp = &v
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
	if task.Status.Container != 0 {
		t.Fatalf("a failed Create must clear the VMID, got %d", task.Status.Container)
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
	if err := ctl.RequestDestroy("t1"); err != nil {
		t.Fatal(err)
	}
	runOnce(ctl)

	if _, ok := st.tasks["t1"]; ok {
		t.Fatal("task record should be deleted")
	}
	if len(prov.destroyed) != 1 || prov.destroyed[0] != 100 {
		t.Fatalf("want destroy [100], got %v", prov.destroyed)
	}
}

// A VMID that an external party reused (hostname no longer px-<task>) is not
// px's to destroy: the record is dropped instead of retrying forever.
func TestDeleteRefusesForeignContainer(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}, hostnames: map[int]string{100: "someone-elses-ct"}}
	_ = st.UpsertTask(testTask(0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	if err := ctl.RequestDestroy("t1"); err != nil {
		t.Fatal(err)
	}
	runOnce(ctl)

	if _, ok := st.tasks["t1"]; ok {
		t.Fatal("record should be dropped: there is nothing left to retry against a foreign container")
	}
	if len(prov.destroyed) != 0 {
		t.Fatalf("foreign container must not be destroyed, got %v", prov.destroyed)
	}
}

func TestTTLSkipsForeignContainer(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}, hostnames: map[int]string{100: "someone-elses-ct"}}
	_ = st.UpsertTask(testTask(60))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	prov.mu.Lock()
	prov.exits[100] = 0
	prov.mu.Unlock()
	runOnce(ctl)

	base := time.Now()
	ctl.now = func() time.Time { return base.Add(2 * time.Hour) }
	runOnce(ctl)

	task := get(t, st, "t1")
	if task.Status.Container != 0 {
		t.Fatalf("want container cleared, got %d", task.Status.Container)
	}
	if len(prov.destroyed) != 0 {
		t.Fatalf("foreign container must not be destroyed, got %v", prov.destroyed)
	}
	if !strings.Contains(task.Status.Reason, "not owned") {
		t.Fatalf("reason should note the ownership skip, got %q", task.Status.Reason)
	}
}

// An interrupted provision whose recorded VMID now names a foreign container
// fails the task without touching the foreign container.
func TestInterruptedProvisioningForeignContainer(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{
		exits:     map[int]int{},
		booted:    map[int]bool{100: false},
		hostnames: map[int]string{100: "someone-elses-ct"},
	}
	task := testTask(0)
	task.Status.Phase = v1alpha1.TaskProvisioning
	task.Status.Container = 100
	_ = st.UpsertTask(task)
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	if len(prov.destroyed) != 0 {
		t.Fatalf("foreign container must not be destroyed, got %v", prov.destroyed)
	}
	task = get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskProvisionFail {
		t.Fatalf("want ProvisionFailed, got %s", task.Status.Phase)
	}
	if task.Status.Container != 0 {
		t.Fatalf("want container cleared, got %d", task.Status.Container)
	}
	if !strings.Contains(task.Status.Reason, "not owned") {
		t.Fatalf("reason should note the ownership skip, got %q", task.Status.Reason)
	}
	if len(prov.created) != 0 {
		t.Fatalf("must not re-provision, created %v", prov.created)
	}
}

// Deletion requests are persisted on the record, so a brand-new controller
// over the same store (i.e. after a px-server restart) resumes the delete.
func TestDeleteSurvivesRestart(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	_ = st.UpsertTask(testTask(0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl) // -> Running, vmid 100
	if err := ctl.RequestDestroy("t1"); err != nil {
		t.Fatal(err)
	}

	restarted := New(st, prov, slog.New(slog.DiscardHandler))
	runOnce(restarted)

	if _, ok := st.tasks["t1"]; ok {
		t.Fatal("task record should be deleted after restart")
	}
	if len(prov.destroyed) != 1 || prov.destroyed[0] != 100 {
		t.Fatalf("want destroy [100], got %v", prov.destroyed)
	}
}

func TestRequestDestroyUnknownTask(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	ctl := New(st, prov, slog.New(slog.DiscardHandler))
	if err := ctl.RequestDestroy("nope"); err == nil {
		t.Fatal("want error for unknown task")
	}
}

// Task workspace references resolve against stored Workspaces, and the
// resolved repos ride into Create as mounts.
func TestWorkspacesResolvedIntoCreate(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	st.workspaces["repo-a"] = &v1alpha1.Workspace{
		APIVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindWorkspace,
		Metadata:   v1alpha1.ObjectMeta{Name: "repo-a"},
		Spec:       v1alpha1.WorkspaceSpec{Git: v1alpha1.GitSpec{Repo: "https://example.com/a.git", Branch: "main"}},
	}
	st.workspaces["repo-b"] = &v1alpha1.Workspace{
		APIVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindWorkspace,
		Metadata:   v1alpha1.ObjectMeta{Name: "repo-b"},
		Spec:       v1alpha1.WorkspaceSpec{Git: v1alpha1.GitSpec{Repo: "https://example.com/b.git"}},
	}
	task := testTask(0)
	task.Spec.Workspaces = []v1alpha1.TaskWorkspace{
		{Name: "repo-a", Goal: "work on a"},
		{Name: "repo-b"},
	}
	_ = st.UpsertTask(task)
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	prov.mu.Lock()
	mounts := prov.mounts
	prov.mu.Unlock()
	want := []ResolvedWorkspace{
		{Name: "repo-a", Repo: "https://example.com/a.git", Branch: "main"},
		{Name: "repo-b", Repo: "https://example.com/b.git"},
	}
	if len(mounts) != len(want) {
		t.Fatalf("want mounts %v, got %v", want, mounts)
	}
	for i := range want {
		if mounts[i] != want[i] {
			t.Errorf("mounts[%d] = %v, want %v", i, mounts[i], want[i])
		}
	}
}

// A reference to a Workspace that was never applied fails the provision
// before any container work starts.
func TestUnknownWorkspaceFailsProvision(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	task := testTask(0)
	task.Spec.Workspaces = []v1alpha1.TaskWorkspace{{Name: "missing"}}
	_ = st.UpsertTask(task)
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	if len(prov.created) != 0 {
		t.Fatalf("Create must not run for an unresolved workspace, created %v", prov.created)
	}
	task = get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskProvisionFail {
		t.Fatalf("want ProvisionFailed, got %s", task.Status.Phase)
	}
	if task.Status.Container != 0 {
		t.Fatalf("want container 0, got %d", task.Status.Container)
	}
	if task.Status.Reason == "" {
		t.Fatal("Reason should name the missing workspace")
	}
}

// A task model reference resolves against the stored Model and rides into
// Create as credentials for the boot script.
func TestModelResolvedIntoCreate(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	st.models["claude"] = &v1alpha1.Model{
		APIVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindModel,
		Metadata:   v1alpha1.ObjectMeta{Name: "claude"},
		Spec:       v1alpha1.ModelSpec{Provider: v1alpha1.ProviderAnthropic, APIKey: "sk-secret", BaseURL: "https://proxy.example.com"},
	}
	task := testTask(0)
	task.Spec.Model = "claude"
	_ = st.UpsertTask(task)
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	prov.mu.Lock()
	model := prov.model
	prov.mu.Unlock()
	task = get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskRunning {
		t.Fatalf("want Running, got %s", task.Status.Phase)
	}
	if model == nil || model.Provider != v1alpha1.ProviderAnthropic || model.APIKey != "sk-secret" || model.BaseURL != "https://proxy.example.com" {
		t.Fatalf("Create got wrong model: %+v", model)
	}
}

// A task without a model reference passes nil, so the boot script carries no
// credential machinery at all.
func TestNoModelPassesNil(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	_ = st.UpsertTask(testTask(0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	prov.mu.Lock()
	model := prov.model
	prov.mu.Unlock()
	if model != nil {
		t.Fatalf("model-less task must pass nil, got %+v", model)
	}
}

// A reference to a Model that was never applied fails the provision before
// any container work starts.
func TestUnknownModelFailsProvision(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	task := testTask(0)
	task.Spec.Model = "missing"
	_ = st.UpsertTask(task)
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	if len(prov.created) != 0 {
		t.Fatalf("Create must not run for an unresolved model, created %v", prov.created)
	}
	task = get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskProvisionFail {
		t.Fatalf("want ProvisionFailed, got %s", task.Status.Phase)
	}
	if task.Status.Container != 0 {
		t.Fatalf("want container 0, got %d", task.Status.Container)
	}
	if !strings.Contains(task.Status.Reason, "missing") {
		t.Fatalf("Reason should name the missing model, got %q", task.Status.Reason)
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

// Allocate fails before any container exists: the task goes to
// ProvisionFailed with no VMID recorded.
func TestAllocateError(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}, allocateErr: errors.New("no free id")}
	_ = st.UpsertTask(testTask(0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	task := get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskProvisionFail {
		t.Fatalf("want ProvisionFailed, got %s", task.Status.Phase)
	}
	if task.Status.Container != 0 {
		t.Fatalf("want container 0, got %d", task.Status.Container)
	}
	if len(prov.created) != 0 {
		t.Fatalf("Create must not run after a failed Allocate, created %v", prov.created)
	}
}

// If the VMID cannot be persisted, node-side work must not start, and a
// later successful write must not record a VMID for a container that was
// never created — delete/TTL would then target an id we do not own.
func TestPersistVmidFailureAbortsCreate(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	_ = st.UpsertTask(testTask(0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	st.mu.Lock()
	st.failUpserts = 2 // 1st: Provisioning persist, 2nd: VMID persist; failProvision's retry succeeds
	st.mu.Unlock()

	runOnce(ctl)
	if len(prov.created) != 0 {
		t.Fatalf("Create must not run when the VMID cannot be persisted, created %v", prov.created)
	}
	task := get(t, st, "t1")
	if task.Status.Container != 0 {
		t.Fatalf("an uncreated VMID must not be recorded, got %d", task.Status.Container)
	}
	if task.Status.Phase != v1alpha1.TaskProvisionFail {
		t.Fatalf("want ProvisionFailed, got %s", task.Status.Phase)
	}
}

// Restart mid-Create with a container that never reached the runner-launch
// step: the partial clone is destroyed and the task fails — it must not spin
// in a fake Running.
func TestInterruptedProvisioningUnbooted(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	task := testTask(0)
	task.Status.Phase = v1alpha1.TaskProvisioning
	task.Status.Container = 100
	_ = st.UpsertTask(task)
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	if len(prov.destroyed) != 1 || prov.destroyed[0] != 100 {
		t.Fatalf("want destroy [100], got %v", prov.destroyed)
	}
	task = get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskProvisionFail {
		t.Fatalf("want ProvisionFailed, got %s", task.Status.Phase)
	}
	if task.Status.Container != 0 {
		t.Fatalf("want container cleared, got %d", task.Status.Container)
	}
	if len(prov.created) != 0 {
		t.Fatalf("must not re-provision, created %v", prov.created)
	}
}

// Restart mid-Create after the runner actually launched (boot marker
// present): the task is adopted as Running instead of being torn down.
func TestAdoptBootedProvisioning(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}, booted: map[int]bool{100: true}}
	task := testTask(0)
	task.Status.Phase = v1alpha1.TaskProvisioning
	task.Status.Container = 100
	_ = st.UpsertTask(task)
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	task = get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskRunning {
		t.Fatalf("want Running, got %s", task.Status.Phase)
	}
	if task.Status.StartedAt == nil {
		t.Fatal("StartedAt should be set on adoption")
	}
	if len(prov.destroyed) != 0 {
		t.Fatalf("live runner must not be destroyed, got %v", prov.destroyed)
	}
}

// A boot probe that fails transiently (pct exec died while the container is
// running) must neither adopt nor destroy: the task stays in Provisioning
// and the next tick retries.
func TestBootProbeErrorRetries(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{
		exits:     map[int]int{},
		bootedErr: errors.New("pct exec: connection reset by peer"),
	}
	task := testTask(0)
	task.Status.Phase = v1alpha1.TaskProvisioning
	task.Status.Container = 100
	_ = st.UpsertTask(task)
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	task = get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskProvisioning {
		t.Fatalf("probe error must leave the task in Provisioning, got %s", task.Status.Phase)
	}
	if len(prov.destroyed) != 0 || len(prov.created) != 0 {
		t.Fatalf("probe error must neither destroy nor re-provision, destroyed %v created %v", prov.destroyed, prov.created)
	}
}

// A booted container that is not the task's (hostname mismatch) must not be
// adopted: adoption would run someone else's runner as this task. The
// foreign container is left alone and the task fails.
func TestAdoptRefusesForeignContainer(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{
		exits:     map[int]int{},
		booted:    map[int]bool{100: true},
		hostnames: map[int]string{100: "someone-elses-ct"},
	}
	task := testTask(0)
	task.Status.Phase = v1alpha1.TaskProvisioning
	task.Status.Container = 100
	_ = st.UpsertTask(task)
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	if len(prov.destroyed) != 0 {
		t.Fatalf("foreign container must not be destroyed, got %v", prov.destroyed)
	}
	task = get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskProvisionFail {
		t.Fatalf("want ProvisionFailed, got %s", task.Status.Phase)
	}
	if task.Status.Container != 0 {
		t.Fatalf("want container cleared, got %d", task.Status.Container)
	}
	if !strings.Contains(task.Status.Reason, "not owned") {
		t.Fatalf("reason should note the ownership skip, got %q", task.Status.Reason)
	}
}

// A status write from a snapshot that predates a deletion request must not
// erase the request (the store-level race behind TestDeleteSurvivesRestart).
func TestUpsertKeepsDeletionMark(t *testing.T) {
	st := newMemStore()
	_ = st.UpsertTask(testTask(0))
	if err := st.MarkTaskDeleted("t1", time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = st.UpsertTask(testTask(0)) // stale snapshot, no mark
	cur := st.tasks["t1"]
	if cur.Status.DeletionTimestamp == nil {
		t.Fatal("deletion mark was erased by a stale snapshot write")
	}
}

// Delete racing an interrupted provision: the record disappears even though
// no container was ever recorded, and nothing is destroyed or created.
func TestDeleteWithoutContainer(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	task := testTask(0)
	task.Status.Phase = v1alpha1.TaskProvisioning
	_ = st.UpsertTask(task)
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	if err := ctl.RequestDestroy("t1"); err != nil {
		t.Fatal(err)
	}
	runOnce(ctl)

	if _, ok := st.tasks["t1"]; ok {
		t.Fatal("task record should be deleted")
	}
	if len(prov.destroyed) != 0 || len(prov.created) != 0 {
		t.Fatalf("want no destroy/create, destroyed %v created %v", prov.destroyed, prov.created)
	}
}

func TestDoubleDelete(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	_ = st.UpsertTask(testTask(0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	if err := ctl.RequestDestroy("t1"); err != nil {
		t.Fatal(err)
	}
	if err := ctl.RequestDestroy("t1"); err != nil {
		t.Fatalf("second delete must be idempotent, got %v", err)
	}
	runOnce(ctl)
	if _, ok := st.tasks["t1"]; ok {
		t.Fatal("task record should be deleted")
	}
	if len(prov.destroyed) != 1 {
		t.Fatalf("want exactly one destroy, got %v", prov.destroyed)
	}
}

// Deleting a finished task bypasses the TTL and destroys immediately.
func TestDeleteFinishedTaskIgnoresTTL(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	_ = st.UpsertTask(testTask(600))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	prov.mu.Lock()
	prov.exits[100] = 0
	prov.mu.Unlock()
	runOnce(ctl) // -> Succeeded, TTL clock starts

	if err := ctl.RequestDestroy("t1"); err != nil {
		t.Fatal(err)
	}
	runOnce(ctl)
	if _, ok := st.tasks["t1"]; ok {
		t.Fatal("task record should be deleted despite TTL")
	}
	if len(prov.destroyed) != 1 || prov.destroyed[0] != 100 {
		t.Fatalf("want destroy [100], got %v", prov.destroyed)
	}
}

func TestDeleteRetriesOnDestroyFailure(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}, destroyErr: errors.New("pve down")}
	_ = st.UpsertTask(testTask(0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl) // -> Running, vmid 100
	if err := ctl.RequestDestroy("t1"); err != nil {
		t.Fatal(err)
	}
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
