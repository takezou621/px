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
	"github.com/kawai/px/internal/store"
)

// The fake schedules every task onto node "n1" and ignores node arguments,
// so the node dimension of the provisioner interface is untested here —
// cluster scheduling has its own tests in provisioner_test.go.
const fakeNode = "n1"

type fakeProv struct {
	mu          sync.Mutex
	allocateErr error        // returned by Allocate as a provision failure
	createErr   error        // returned by Create as a provision failure
	scheduleErr error        // returned by Schedule as a provision failure
	nodeOfErr   error        // returned by NodeOf (wrap ErrGuestGone for a vanished container)
	exitErr     error        // returned by Exit as a probe failure
	bootedErr   error        // returned by Booted as a probe failure
	frozenErr   error        // returned by Frozen as a probe failure
	freezeErr   error        // returned by Freeze
	thawErr     error        // returned by Thaw
	runningErr  error        // returned by Running as a probe failure
	destroyErr  error        // returned by Destroy until cleared
	exits       map[int]int  // vmid -> exit code; missing = still running
	dead        map[int]bool // vmids whose container is not running
	booted      map[int]bool // vmids whose container reached the runner launch
	frozen      map[int]bool // vmids whose cgroup is currently frozen
	created     []int
	thaws       []int               // vmids passed to Thaw, in call order
	mounts      []ResolvedWorkspace // mounts passed to the last Create
	model       *ResolvedModel      // model passed to the last Create
	gw          *ResolvedGateway    // gateway passed to the last Create
	destroyed   []int
	hostnames   map[int]string            // vmid -> hostname; empty or missing = owned
	forwards    map[int]PortForward       // hostPort -> forward currently "running" on the node
	ensureErr   error                     // returned by EnsurePorts
	removeErr   error                     // returned by RemovePorts
	ensureFails map[int]bool              // hostPorts EnsurePorts reports as failed
	ctipOf      map[int]string            // vmid -> CTIP EnsurePorts reports (default link-local)
	removeCalls int                       // number of RemovePorts invocations, success or not
	removed     []int                     // hostPorts passed to RemovePorts, in call order
}

func (f *fakeProv) Allocate(_ context.Context) (int, error) {
	if f.allocateErr != nil {
		return 0, f.allocateErr
	}
	return 100 + len(f.created), nil
}

func (f *fakeProv) Schedule(_ context.Context, _ string) (string, error) {
	if f.scheduleErr != nil {
		return "", f.scheduleErr
	}
	return fakeNode, nil
}

func (f *fakeProv) NodeOf(_ context.Context, _ int) (string, error) {
	if f.nodeOfErr != nil {
		return "", f.nodeOfErr
	}
	return fakeNode, nil
}

func (f *fakeProv) Create(_ context.Context, _ *v1alpha1.Task, _ string, vmid int, mounts []ResolvedWorkspace, model *ResolvedModel, gw *ResolvedGateway) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.mu.Lock()
	f.created = append(f.created, vmid)
	f.mounts = mounts
	f.model = model
	f.gw = gw
	f.mu.Unlock()
	return nil
}

func (f *fakeProv) Booted(_ context.Context, _ string, vmid int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bootedErr != nil {
		return false, f.bootedErr
	}
	return f.booted[vmid], nil
}

func (f *fakeProv) Exit(_ context.Context, _ string, vmid int) (*int, error) {
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

func (f *fakeProv) Running(_ context.Context, _ string, vmid int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runningErr != nil {
		return false, f.runningErr
	}
	return !f.dead[vmid], nil
}

func (f *fakeProv) Logs(_ context.Context, _ string, _ int) (string, error) { return "log\n", nil }

func (f *fakeProv) Exec(_ context.Context, _ string, _ int, _ []string) (*ExecResult, error) {
	return &ExecResult{}, nil
}

func (f *fakeProv) Frozen(_ context.Context, _ string, vmid int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.frozenErr != nil {
		return false, f.frozenErr
	}
	return f.frozen[vmid], nil
}

func (f *fakeProv) Freeze(_ context.Context, _ string, vmid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.freezeErr != nil {
		return f.freezeErr
	}
	if f.frozen == nil {
		f.frozen = map[int]bool{}
	}
	f.frozen[vmid] = true
	return nil
}

func (f *fakeProv) Thaw(_ context.Context, _ string, vmid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.thaws = append(f.thaws, vmid)
	if f.thawErr != nil {
		return f.thawErr
	}
	if f.frozen == nil {
		f.frozen = map[int]bool{}
	}
	f.frozen[vmid] = false
	return nil
}

func (f *fakeProv) Destroy(_ context.Context, _ string, vmid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.destroyErr != nil {
		return f.destroyErr
	}
	f.destroyed = append(f.destroyed, vmid)
	return nil
}

func (f *fakeProv) EnsurePorts(_ context.Context, _ string, vmid int, fwds []PortForward) (PortForwardResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ensureErr != nil {
		return PortForwardResult{}, f.ensureErr
	}
	ctip := f.ctipOf[vmid]
	if ctip == "" {
		ctip = "192.0.2.10"
	}
	res := PortForwardResult{CTIP: ctip}
	if f.forwards == nil {
		f.forwards = map[int]PortForward{}
	}
	for _, fwd := range fwds {
		if f.ensureFails[fwd.HostPort] {
			res.Failed = append(res.Failed, fwd.HostPort)
			continue
		}
		f.forwards[fwd.HostPort] = fwd
	}
	return res, nil
}

func (f *fakeProv) RemovePorts(_ context.Context, _ string, hostPorts []int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeCalls++
	if f.removeErr != nil {
		return f.removeErr
	}
	for _, hp := range hostPorts {
		f.removed = append(f.removed, hp)
		delete(f.forwards, hp)
	}
	return nil
}

func (f *fakeProv) DestroyOwned(ctx context.Context, taskName, _ string, vmid int) error {
	f.mu.Lock()
	host := f.hostnames[vmid]
	f.mu.Unlock()
	if host != "" && host != "px-"+taskName {
		return fmt.Errorf("%w: ct %d hostname %q is not %q", ErrNotOwned, vmid, host, "px-"+taskName)
	}
	return f.Destroy(ctx, fakeNode, vmid)
}

// Owned mirrors DestroyOwned's hostname check without destroying: empty or
// missing means owned, since plain tests never set a hostname.
func (f *fakeProv) Owned(_ context.Context, taskName, _ string, vmid int) (bool, error) {
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
	gateways    map[string]*v1alpha1.Gateway
}

func newMemStore() *memStore {
	return &memStore{
		tasks:      map[string]*v1alpha1.Task{},
		workspaces: map[string]*v1alpha1.Workspace{},
		models:     map[string]*v1alpha1.Model{},
		gateways:   map[string]*v1alpha1.Gateway{},
	}
}

func (m *memStore) GetGateway(name string) (*v1alpha1.Gateway, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.gateways[name]
	if !ok {
		return nil, errors.New("not found")
	}
	return g, nil
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

// MarkTaskPhase mirrors the real store's compare-and-set semantics: phase
// and reason move together, nothing else on the record is touched, and a
// mismatched expect writes nothing.
func (m *memStore) MarkTaskPhase(name string, expect, phase v1alpha1.TaskPhase, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.tasks[name]
	if !ok {
		return store.ErrPhaseConflict
	}
	if cur.Status.Phase != expect {
		return store.ErrPhaseConflict
	}
	cur.Status.Phase = phase
	cur.Status.Reason = reason
	return nil
}

// copyTask deep-copies what the controller mutates through returned pointers
// (spec slices, status timestamps), so tests see the same aliasing rules as
// the real store.
func copyTask(t *v1alpha1.Task) *v1alpha1.Task {
	cp := *t
	cp.Spec.Workspaces = append([]v1alpha1.TaskWorkspace(nil), t.Spec.Workspaces...)
	cp.Spec.Runner.Command = append([]string(nil), t.Spec.Runner.Command...)
	cp.Spec.Ports = append([]v1alpha1.PortSpec(nil), t.Spec.Ports...)
	cp.Status.Ports = append([]v1alpha1.PortStatus(nil), t.Status.Ports...)
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
	// The pre-destroy thaw is guarded by the same ownership check: thawing a
	// container px does not own would unfreeze someone else's workload.
	if len(prov.thaws) != 0 {
		t.Fatalf("foreign container must not be thawed, got %v", prov.thaws)
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
	task.Status.Node = fakeNode
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

// A task gateway reference resolves against the stored Gateway and rides into
// Create as the egress allowlist.
func TestGatewayResolvedIntoCreate(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	st.gateways["locked"] = &v1alpha1.Gateway{
		APIVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindGateway,
		Metadata:   v1alpha1.ObjectMeta{Name: "locked"},
		Spec:       v1alpha1.GatewaySpec{Egress: []v1alpha1.EgressRule{{CIDR: "10.0.0.0/8", Ports: "443", Proto: "tcp"}}},
	}
	task := testTask(0)
	task.Spec.Gateway = "locked"
	_ = st.UpsertTask(task)
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	prov.mu.Lock()
	gw := prov.gw
	prov.mu.Unlock()
	task = get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskRunning {
		t.Fatalf("want Running, got %s", task.Status.Phase)
	}
	if gw == nil || gw.Name != "locked" || len(gw.Egress) != 1 || gw.Egress[0].CIDR != "10.0.0.0/8" {
		t.Fatalf("Create got wrong gateway: %+v", gw)
	}
}

// A task without a gateway reference passes nil, so no firewall is enabled
// and no rules are written for the container.
func TestNoGatewayPassesNil(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	_ = st.UpsertTask(testTask(0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	prov.mu.Lock()
	gw := prov.gw
	prov.mu.Unlock()
	if gw != nil {
		t.Fatalf("gateway-less task must pass nil, got %+v", gw)
	}
}

// A reference to a Gateway that was never applied fails the provision before
// any container work starts — never a container that boots fully open.
func TestUnknownGatewayFailsProvision(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	task := testTask(0)
	task.Spec.Gateway = "missing"
	_ = st.UpsertTask(task)
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	if len(prov.created) != 0 {
		t.Fatalf("Create must not run for an unresolved gateway, created %v", prov.created)
	}
	task = get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskProvisionFail {
		t.Fatalf("want ProvisionFailed, got %s", task.Status.Phase)
	}
	if task.Status.Container != 0 {
		t.Fatalf("want container 0, got %d", task.Status.Container)
	}
	if !strings.Contains(task.Status.Reason, "missing") {
		t.Fatalf("Reason should name the missing gateway, got %q", task.Status.Reason)
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
	task.Status.Node = fakeNode
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
	task.Status.Node = fakeNode
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
	task.Status.Node = fakeNode
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
	task.Status.Node = fakeNode
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

// A Schedule failure (no online node holds the template) is a provision
// failure before any container exists: no VMID is recorded, nothing created.
func TestScheduleError(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}, scheduleErr: errors.New("no online node holds template tmpl")}
	_ = st.UpsertTask(testTask(0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	if len(prov.created) != 0 {
		t.Fatalf("Create must not run after a failed Schedule, created %v", prov.created)
	}
	task := get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskProvisionFail {
		t.Fatalf("want ProvisionFailed, got %s", task.Status.Phase)
	}
	if task.Status.Container != 0 {
		t.Fatalf("want container 0, got %d", task.Status.Container)
	}
	if !strings.Contains(task.Status.Reason, "schedule") {
		t.Fatalf("Reason should name the scheduling failure, got %q", task.Status.Reason)
	}
}

// Provision records the scheduled node, and the whole lifecycle keeps it.
func TestProvisionRecordsNode(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	_ = st.UpsertTask(testTask(0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	task := get(t, st, "t1")
	if task.Status.Node != fakeNode {
		t.Fatalf("want node %s, got %q", fakeNode, task.Status.Node)
	}
	prov.mu.Lock()
	prov.exits[100] = 0
	prov.mu.Unlock()
	runOnce(ctl)
	task = get(t, st, "t1")
	if task.Status.Node != fakeNode {
		t.Fatalf("node must survive the finish transition, got %q", task.Status.Node)
	}
}

// suspend drives Running -> Suspending -> Suspended, resume drives
// Suspended -> Resuming -> Running, each state change confirmed by a Frozen
// probe on a later tick — and a suspended task is not polled, so its runner
// cannot report an exit underneath the freeze.
func TestSuspendResumeFlow(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	_ = st.UpsertTask(testTask(0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl) // -> Running
	task := get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskRunning {
		t.Fatalf("want Running, got %s", task.Status.Phase)
	}

	if err := ctl.RequestSuspend("t1", v1alpha1.TaskRunning); err != nil {
		t.Fatal(err)
	}
	runOnce(ctl) // Suspending: freezes, defers confirmation to the next tick
	task = get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskSuspending {
		t.Fatalf("want Suspending after the freeze tick, got %s", task.Status.Phase)
	}
	runOnce(ctl) // confirms frozen -> Suspended
	task = get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskSuspended {
		t.Fatalf("want Suspended, got %s", task.Status.Phase)
	}

	// The runner was told to exit while suspended: a frozen cgroup cannot
	// report it, and the suspended watch deliberately does not ask.
	prov.mu.Lock()
	prov.exits[100] = 0
	prov.mu.Unlock()
	runOnce(ctl)
	runOnce(ctl)
	task = get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskSuspended {
		t.Fatalf("a suspended task must not observe runner exit, got %s", task.Status.Phase)
	}

	if err := ctl.RequestResume("t1", v1alpha1.TaskSuspended); err != nil {
		t.Fatal(err)
	}
	runOnce(ctl) // Resuming: thaws, defers confirmation
	task = get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskResuming {
		t.Fatalf("want Resuming after the thaw tick, got %s", task.Status.Phase)
	}
	runOnce(ctl) // confirms thawed -> Running
	task = get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskRunning {
		t.Fatalf("want Running after resume confirms, got %s", task.Status.Phase)
	}

	runOnce(ctl) // back to polling: the pending exit surfaces now
	task = get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskSucceeded {
		t.Fatalf("want Succeeded after resume drains the exit, got %s", task.Status.Phase)
	}
}

// An external thaw while Suspended (pct on the node, host restart) is
// re-frozen instead of silently un-suspending the task.
func TestSuspendedReFreezesAfterExternalThaw(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	_ = st.UpsertTask(testTask(0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl) // -> Running
	if err := ctl.RequestSuspend("t1", v1alpha1.TaskRunning); err != nil {
		t.Fatal(err)
	}
	runOnce(ctl) // freeze
	runOnce(ctl) // confirm -> Suspended

	prov.mu.Lock()
	prov.frozen[100] = false // thaw out-of-band
	prov.mu.Unlock()
	runOnce(ctl) // re-freeze
	prov.mu.Lock()
	frozen := prov.frozen[100]
	prov.mu.Unlock()
	if !frozen {
		t.Fatal("an externally thawed suspended container must be re-frozen")
	}
	task := get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskSuspended {
		t.Fatalf("want still Suspended, got %s", task.Status.Phase)
	}
}

// A container frozen out-of-band while the task shows Running is adopted as
// Suspended: every pct exec into it would hang until thaw, so polling is
// impossible and the freeze means a suspend.
func TestRunningAdoptsFrozenContainer(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	_ = st.UpsertTask(testTask(0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl) // -> Running
	prov.mu.Lock()
	prov.frozen = map[int]bool{100: true}
	prov.mu.Unlock()
	runOnce(ctl)

	task := get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskSuspended {
		t.Fatalf("want Suspended, got %s", task.Status.Phase)
	}
}

// Records persisted before Status.Node existed are repaired from the cluster
// view on the first tick, then proceed as if they had always carried it.
// A Frozen-probe failure during any suspend phase is ambiguous (SSH blip vs
// dead guest); the decider is Running. A dead container must settle the task
// as Failed instead of retrying the probe forever — a Suspending/Suspended/
// Resuming record wedged on a vanished container never reaches a terminal
// phase, and resume would 409 against it.
func TestSuspendPhaseFailsOnDeadContainer(t *testing.T) {
	cases := []struct {
		name  string
		setup func(ctl *Controller)
	}{
		{
			name: "suspending",
			setup: func(ctl *Controller) {
				runOnce(ctl) // -> Running
				if err := ctl.RequestSuspend("t1", v1alpha1.TaskRunning); err != nil {
					t.Fatal(err)
				}
				runOnce(ctl) // freeze -> Suspending
			},
		},
		{
			name: "suspended",
			setup: func(ctl *Controller) {
				runOnce(ctl) // -> Running
				if err := ctl.RequestSuspend("t1", v1alpha1.TaskRunning); err != nil {
					t.Fatal(err)
				}
				runOnce(ctl) // freeze -> Suspending
				runOnce(ctl) // confirm -> Suspended
			},
		},
		{
			name: "resuming",
			setup: func(ctl *Controller) {
				runOnce(ctl) // -> Running
				if err := ctl.RequestSuspend("t1", v1alpha1.TaskRunning); err != nil {
					t.Fatal(err)
				}
				runOnce(ctl) // freeze
				runOnce(ctl) // confirm -> Suspended
				if err := ctl.RequestResume("t1", v1alpha1.TaskSuspended); err != nil {
					t.Fatal(err)
				}
				runOnce(ctl) // thaw -> Resuming
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newMemStore()
			prov := &fakeProv{exits: map[int]int{}, dead: map[int]bool{}}
			_ = st.UpsertTask(testTask(0))
			ctl := New(st, prov, slog.New(slog.DiscardHandler))
			tc.setup(ctl)

			prov.mu.Lock()
			prov.dead[100] = true
			prov.frozenErr = errors.New("cgroup.events unreadable")
			prov.mu.Unlock()
			runOnce(ctl)

			task := get(t, st, "t1")
			if task.Status.Phase != v1alpha1.TaskFailed {
				t.Fatalf("want Failed, got %s", task.Status.Phase)
			}
			if task.Status.EndedAt == nil {
				t.Fatal("EndedAt should be set")
			}
			if !strings.Contains(task.Status.Reason, "not running while "+tc.name) {
				t.Fatalf("Reason should name the dead container during %s, got %q", tc.name, task.Status.Reason)
			}
		})
	}
}
// A Frozen-probe failure with the container still alive is a transient error
// (SSH blip): the suspend phase must hold, not fail the task.
func TestSuspendPhaseSurvivesProbeErrorWithLiveContainer(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}, frozenErr: errors.New("ssh connection reset")}
	_ = st.UpsertTask(testTask(0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl) // -> Running
	if err := ctl.RequestSuspend("t1", v1alpha1.TaskRunning); err != nil {
		t.Fatal(err)
	}
	runOnce(ctl) // freeze -> Suspending
	runOnce(ctl) // probe fails, but container is up

	task := get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskSuspending {
		t.Fatalf("want still Suspending, got %s", task.Status.Phase)
	}
}

// A Frozen-probe failure where Running cannot be checked either (SSH down for
// both) is still treated as transient: settling the task as Failed on a
// dead-check error would kill live tasks on every node blip.
func TestSuspendPhaseSurvivesDeadCheckError(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}, frozenErr: errors.New("cgroup.events unreadable"),
		runningErr: errors.New("ssh down")}
	_ = st.UpsertTask(testTask(0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl) // -> Running
	if err := ctl.RequestSuspend("t1", v1alpha1.TaskRunning); err != nil {
		t.Fatal(err)
	}
	runOnce(ctl) // freeze -> Suspending
	runOnce(ctl) // probe fails; Running check fails too

	task := get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskSuspending {
		t.Fatalf("want still Suspending, got %s", task.Status.Phase)
	}
}

// RequestSuspend is a compare-and-set on the phase: a mark computed from a
// stale read — reconcile finished the task between the caller's read and the
// request — must fail instead of freezing an exited task into a Suspending
// zombie whose only exit is delete.
func TestSuspendRequestRejectsStalePhase(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	_ = st.UpsertTask(testTask(0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl) // -> Running
	prov.mu.Lock()
	prov.exits[100] = 0
	prov.mu.Unlock()
	runOnce(ctl) // runner exits -> Succeeded

	if err := ctl.RequestSuspend("t1", v1alpha1.TaskRunning); !errors.Is(err, store.ErrPhaseConflict) {
		t.Fatalf("want ErrPhaseConflict, got %v", err)
	}
	task := get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskSucceeded {
		t.Fatalf("task must stay Succeeded, got %s", task.Status.Phase)
	}
}

func TestRepairNode(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	task := testTask(0)
	task.Status.Phase = v1alpha1.TaskRunning
	task.Status.Container = 100
	_ = st.UpsertTask(task)
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl) // repair only
	task = get(t, st, "t1")
	if task.Status.Node != fakeNode {
		t.Fatalf("want node repaired to %s, got %q", fakeNode, task.Status.Node)
	}
	if task.Status.Phase != v1alpha1.TaskRunning {
		t.Fatalf("repair must not move the phase, got %s", task.Status.Phase)
	}

	prov.mu.Lock()
	prov.exits[100] = 0
	prov.mu.Unlock()
	runOnce(ctl) // now polls normally
	task = get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskSucceeded {
		t.Fatalf("want Succeeded after repair, got %s", task.Status.Phase)
	}
}

// A pre-multi-node record whose VMID no longer names any guest cannot be
// repaired: the task fails instead of wedging on an undeterminable node.
func TestRepairNodeGuestGone(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{
		exits:     map[int]int{},
		nodeOfErr: fmt.Errorf("lookup: %w", ErrGuestGone),
	}
	task := testTask(0)
	task.Status.Phase = v1alpha1.TaskRunning
	task.Status.Container = 100
	_ = st.UpsertTask(task)
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	task = get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskFailed {
		t.Fatalf("want Failed, got %s", task.Status.Phase)
	}
	if !strings.Contains(task.Status.Reason, "vanished") {
		t.Fatalf("Reason should note the vanished container, got %q", task.Status.Reason)
	}
	if task.Status.EndedAt == nil {
		t.Fatal("EndedAt should be set")
	}
}

// portedTask is a task with one spec.ports entry.
func portedTask(name string, port, hostPort int) *v1alpha1.Task {
	t := testTask(0)
	t.Metadata.Name = name
	t.Spec.Ports = []v1alpha1.PortSpec{{Name: "http", Port: port, HostPort: hostPort}}
	return t
}

// A Running task with an auto-assigned port gets its mapping resolved to the
// px range floor, persisted in status, and a node-side forward on the fake.
func TestPortsPublishedOnRunning(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	_ = st.UpsertTask(portedTask("t1", 8000, 0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl) // -> Running
	// Provision promotes Pending to Running in one tick; the port mapping
	// converges on the *next* running tick. Publishing early would persist
	// a record the node does not honor yet.
	task := get(t, st, "t1")
	if task.Status.Phase != v1alpha1.TaskRunning {
		t.Fatalf("want Running, got %s", task.Status.Phase)
	}
	if len(task.Status.Ports) != 0 || len(prov.forwards) != 0 {
		t.Fatalf("first tick must not publish ports yet, got status %v forwards %v",
			task.Status.Ports, prov.forwards)
	}

	runOnce(ctl) // port convergence happens on the running tick
	task = get(t, st, "t1")
	want := []v1alpha1.PortStatus{{Name: "http", Port: 8000, HostPort: v1alpha1.HostPortMin}}
	if len(task.Status.Ports) != 1 || task.Status.Ports[0] != want[0] {
		t.Fatalf("want status ports %v, got %v", want, task.Status.Ports)
	}
	if fwd, ok := prov.forwards[v1alpha1.HostPortMin]; !ok || fwd.Port != 8000 {
		t.Fatalf("want forward %d->8000 on the node, got %v", v1alpha1.HostPortMin, prov.forwards)
	}
}

// An explicit hostPort is honored verbatim — allocation never rewrites it.
func TestExplicitHostPortPassthrough(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	_ = st.UpsertTask(portedTask("t1", 8000, 31234))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	runOnce(ctl)
	task := get(t, st, "t1")
	if task.Status.Ports[0].HostPort != 31234 {
		t.Fatalf("want explicit hostPort 31234, got %d", task.Status.Ports[0].HostPort)
	}
	if _, ok := prov.forwards[31234]; !ok {
		t.Fatalf("want forward on 31234, got %v", prov.forwards)
	}
}

// Auto-allocation skips hostPorts other tasks claim — an explicit spec entry
// on another task, even while that task is merely Pending.
func TestAutoAllocationAvoidsClaimedPorts(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	_ = st.UpsertTask(portedTask("claimer", 9999, v1alpha1.HostPortMin))
	_ = st.UpsertTask(portedTask("t1", 8000, 0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	runOnce(ctl)
	task := get(t, st, "t1")
	if task.Status.Ports[0].HostPort != v1alpha1.HostPortMin+1 {
		t.Fatalf("want auto port %d (skipping the claimed floor), got %d",
			v1alpha1.HostPortMin+1, task.Status.Ports[0].HostPort)
	}
}

// An explicit entry later in the same spec also blocks the allocator: two
// entries of one task must never resolve to the same hostPort, or their
// socats could not coexist on the node.
func TestAutoAllocationAvoidsOwnExplicitPorts(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	task := portedTask("t1", 8000, 0)
	task.Spec.Ports = append(task.Spec.Ports,
		v1alpha1.PortSpec{Name: "https", Port: 8443, HostPort: v1alpha1.HostPortMin})
	_ = st.UpsertTask(task)
	ctl := New(st, prov, slog.New(slog.DiscardHandler))

	runOnce(ctl)
	runOnce(ctl)
	task = get(t, st, "t1")
	if task.Status.Ports[0].HostPort != v1alpha1.HostPortMin+1 {
		t.Fatalf("want auto port %d (skipping the same-spec explicit %d), got %d",
			v1alpha1.HostPortMin+1, v1alpha1.HostPortMin, task.Status.Ports[0].HostPort)
	}
	if task.Status.Ports[1].HostPort != v1alpha1.HostPortMin {
		t.Fatalf("want explicit port kept at %d, got %d",
			v1alpha1.HostPortMin, task.Status.Ports[1].HostPort)
	}
}

// The spec is the source of truth: a change in spec.ports is re-converged,
// and status entries never outlive their spec entries.
func TestPortsReconvergeOnSpecChange(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	_ = st.UpsertTask(portedTask("t1", 8000, 30001))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))
	runOnce(ctl)
	runOnce(ctl) // -> Running, forward on 30001

	cur := st.tasks["t1"]
	cur.Spec.Ports = []v1alpha1.PortSpec{{Name: "http", Port: 9000, HostPort: 30001}}

	runOnce(ctl)
	task := get(t, st, "t1")
	if task.Status.Ports[0].Port != 9000 {
		t.Fatalf("want spec change converged to port 9000, got %d", task.Status.Ports[0].Port)
	}
	if fwd, ok := prov.forwards[30001]; !ok || fwd.Port != 9000 {
		t.Fatalf("want forward 30001->9000, got %v", prov.forwards)
	}
}

// Shrinking spec.ports sweeps the removed mapping's node-side listener in
// the same pass — an entry dropped from the spec must not keep its socat.
func TestPortsSweptOnSpecShrink(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	task := portedTask("t1", 8000, 30001)
	task.Spec.Ports = append(task.Spec.Ports, v1alpha1.PortSpec{Name: "https", Port: 8443, HostPort: 30002})
	_ = st.UpsertTask(task)
	ctl := New(st, prov, slog.New(slog.DiscardHandler))
	runOnce(ctl)
	runOnce(ctl) // -> Running, forwards on 30001+30002

	cur := st.tasks["t1"]
	cur.Spec.Ports = []v1alpha1.PortSpec{{Name: "http", Port: 8000, HostPort: 30001}}

	runOnce(ctl)
	if fwd, ok := prov.forwards[30002]; ok {
		t.Fatalf("dropped entry's forward must be removed, got %v", fwd)
	}
	task = get(t, st, "t1")
	if len(task.Status.Ports) != 1 || task.Status.Ports[0].HostPort != 30001 {
		t.Fatalf("want only the kept entry in status, got %v", task.Status.Ports)
	}
}

// Emptying spec.ports while Running tears the forwards down, same as any
// other path that leaves the record holding ports the spec no longer names.
func TestPortsTornDownWhenSpecEmptied(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	_ = st.UpsertTask(portedTask("t1", 8000, 30001))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))
	runOnce(ctl)
	runOnce(ctl) // -> Running, forward on 30001

	cur := st.tasks["t1"]
	cur.Spec.Ports = nil

	runOnce(ctl)
	if len(prov.forwards) != 0 {
		t.Fatalf("emptied spec must tear forwards down, got %v", prov.forwards)
	}
	task := get(t, st, "t1")
	if len(task.Status.Ports) != 0 {
		t.Fatalf("status ports must clear, got %v", task.Status.Ports)
	}
}

// Finishing a task tears its forwards down and clears status.ports.
func TestPortsTornDownWhenFinished(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	_ = st.UpsertTask(portedTask("t1", 8000, 0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))
	runOnce(ctl) // -> Running, forward on 30000
	runOnce(ctl) // port convergence

	prov.mu.Lock()
	prov.exits[100] = 0
	prov.mu.Unlock()
	runOnce(ctl) // -> Succeeded
	runOnce(ctl) // the teardown hook runs on the pass after the finish

	if len(prov.forwards) != 0 {
		t.Fatalf("forwards must be removed once the task finishes, got %v", prov.forwards)
	}
	task := get(t, st, "t1")
	if len(task.Status.Ports) != 0 {
		t.Fatalf("status ports must clear after teardown, got %v", task.Status.Ports)
	}
}

// Suspend is not Running: the forwards close while the container is frozen
// and nothing re-ensures them in the suspended watch.
func TestPortsCloseWhileSuspended(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}}
	_ = st.UpsertTask(portedTask("t1", 8000, 0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))
	runOnce(ctl) // -> Running, forward on 30000
	runOnce(ctl) // port convergence
	if len(prov.forwards) != 1 {
		t.Fatalf("precondition: want a live forward, got %v", prov.forwards)
	}

	if err := ctl.RequestSuspend("t1", v1alpha1.TaskRunning); err != nil {
		t.Fatal(err)
	}
	runOnce(ctl) // freeze
	runOnce(ctl) // confirm -> Suspended

	if len(prov.forwards) != 0 {
		t.Fatalf("suspended container must not keep forwards, got %v", prov.forwards)
	}
	task := get(t, st, "t1")
	if len(task.Status.Ports) != 0 {
		t.Fatalf("status ports must clear on suspend, got %v", task.Status.Ports)
	}
}

// Delete removes the forwards before destroying the container, and a failed
// RemovePorts blocks both: destroying first would orphan a node-side socat
// that nothing remembers anymore.
func TestDeleteGatesOnPortsTeardown(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}, removeErr: errors.New("node unreachable")}
	_ = st.UpsertTask(portedTask("t1", 8000, 0))
	ctl := New(st, prov, slog.New(slog.DiscardHandler))
	runOnce(ctl) // -> Running
	runOnce(ctl) // port convergence

	if err := ctl.RequestDestroy("t1"); err != nil {
		t.Fatal(err)
	}
	runOnce(ctl)
	if _, ok := st.tasks["t1"]; !ok {
		t.Fatal("record must survive while the forwards cannot be removed")
	}
	if len(prov.destroyed) != 0 {
		t.Fatalf("destroy must wait for the ports teardown, got %v", prov.destroyed)
	}
	// The reconcile loop's own teardown attempt already failed this tick;
	// the delete path below it must not dial the unreachable node again
	// through its duplicate gate.
	if n := prov.removeCalls; n != 1 {
		t.Fatalf("failed tick must attempt the teardown once, got %d", n)
	}

	prov.mu.Lock()
	prov.removeErr = nil
	prov.mu.Unlock()
	runOnce(ctl)
	if _, ok := st.tasks["t1"]; ok {
		t.Fatal("record should be deleted once the forwards are gone")
	}
	if len(prov.destroyed) != 1 || prov.destroyed[0] != 100 {
		t.Fatalf("want destroy [100] after teardown, got %v", prov.destroyed)
	}
	if n, want := prov.removeCalls, 2; n != want {
		t.Fatalf("want %d RemovePorts calls in total (failed + successful), got %d", want, n)
	}
	if len(prov.removed) != 1 || prov.removed[0] != 30000 {
		t.Fatalf("want removed [30000], got %v", prov.removed)
	}
}

// TTL cleanup has the same gate: a finished task with unremovable forwards
// keeps its container and its port list until the node answers again.
func TestTTLCleanupGatesOnPortsTeardown(t *testing.T) {
	st := newMemStore()
	prov := &fakeProv{exits: map[int]int{}, removeErr: errors.New("node unreachable")}
	task := portedTask("t1", 8000, 0)
	task.Spec.TTLSecondsAfterFinished = 60
	_ = st.UpsertTask(task)
	ctl := New(st, prov, slog.New(slog.DiscardHandler))
	runOnce(ctl) // -> Running
	runOnce(ctl) // port convergence

	prov.mu.Lock()
	prov.exits[100] = 0
	prov.mu.Unlock()
	runOnce(ctl) // -> Succeeded; teardown already failed once, list kept

	ctl.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	runOnce(ctl)
	task = get(t, st, "t1")
	if task.Status.Container == 0 {
		t.Fatal("TTL cleanup must not run while the forwards cannot be removed")
	}
	if len(task.Status.Ports) == 0 {
		t.Fatal("port list must survive to drive the retry")
	}

	prov.mu.Lock()
	prov.removeErr = nil
	prov.mu.Unlock()
	runOnce(ctl)
	task = get(t, st, "t1")
	if task.Status.Container != 0 {
		t.Fatalf("want container cleaned after teardown succeeded, got %d", task.Status.Container)
	}
	if len(prov.forwards) != 0 {
		t.Fatalf("forwards must be gone, got %v", prov.forwards)
	}
}

// The allocator never hands two auto-assigned entries of the same task the
// same port, and skips whatever the rest of the cluster claims.
func TestAllocateHostPortSkipsTaken(t *testing.T) {
	claimed := map[int]string{v1alpha1.HostPortMin: "other"}
	var fwds []PortForward
	first := allocateHostPort(claimed, fwds)
	if first != v1alpha1.HostPortMin+1 {
		t.Fatalf("want %d, got %d", v1alpha1.HostPortMin+1, first)
	}
	fwds = append(fwds, PortForward{HostPort: first})
	second := allocateHostPort(claimed, fwds)
	if second != v1alpha1.HostPortMin+2 {
		t.Fatalf("want %d, got %d", v1alpha1.HostPortMin+2, second)
	}
}
