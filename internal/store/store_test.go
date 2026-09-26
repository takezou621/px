package store

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/kawai/px/internal/apis/v1alpha1"
)

func testTask(name string) *v1alpha1.Task {
	return &v1alpha1.Task{
		APIVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata:   v1alpha1.ObjectMeta{Name: name},
		Spec: v1alpha1.TaskSpec{
			Image:  "tmpl",
			Runner: v1alpha1.RunnerSpec{Command: []string{"true"}},
		},
		Status: v1alpha1.TaskStatus{Phase: v1alpha1.TaskPending},
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir() + "/px.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// The database holds Model API keys in plaintext, so Open must tighten it
// (and its WAL/SHM siblings) to 0600 no matter the umask that created it —
// including a database restored from a permissive backup.
func TestOpenEnforcesDatabaseMode(t *testing.T) {
	path := t.TempDir() + "/px.db"
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(f, 0o644); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(f)
		if err != nil {
			if !os.IsNotExist(err) {
				t.Fatal(err)
			}
			continue
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %o, want 600", f, got)
		}
	}
}

func TestTaskLifecycle(t *testing.T) {
	st := openTestStore(t)

	if err := st.CreateTask(testTask("t1")); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(testTask("t1")); err != ErrExists {
		t.Fatalf("want ErrExists, got %v", err)
	}

	got, err := st.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != v1alpha1.TaskPending {
		t.Fatalf("want Pending, got %s", got.Status.Phase)
	}

	if err := st.DeleteTask("t1"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteTask("t1"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestMarkTaskDeleted(t *testing.T) {
	st := openTestStore(t)
	task := testTask("t1")
	task.Status.Phase = v1alpha1.TaskRunning
	task.Status.Container = 123
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}

	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	if err := st.MarkTaskDeleted("t1", at); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.DeletionTimestamp == nil {
		t.Fatal("deletionTimestamp not set")
	}
	if !got.Status.DeletionTimestamp.Equal(at) {
		t.Fatalf("want %v, got %v", at, got.Status.DeletionTimestamp)
	}
	// Only the mark may change: VMID and phase must survive the update.
	if got.Status.Container != 123 || got.Status.Phase != v1alpha1.TaskRunning {
		t.Fatalf("mark clobbered other fields: %+v", got.Status)
	}

	if err := st.MarkTaskDeleted("nope", at); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// MarkTaskPhase is a compare-and-set on the phase the caller read: a mark
// computed from a stale read (reconcile finished the task in between) must
// write nothing — freezing a task that already exited into a Suspending
// record is exactly the failure this guards against.
func TestMarkTaskPhaseCAS(t *testing.T) {
	st := openTestStore(t)
	task := testTask("t1")
	task.Status.Phase = v1alpha1.TaskRunning
	if err := st.CreateTask(task); err != nil {
		t.Fatal(err)
	}

	// Matching expect: the write lands.
	if err := st.MarkTaskPhase("t1", v1alpha1.TaskRunning, v1alpha1.TaskSuspending, "suspend requested"); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != v1alpha1.TaskSuspending || got.Status.Reason != "suspend requested" {
		t.Fatalf("want Suspending/suspend requested, got %s/%s", got.Status.Phase, got.Status.Reason)
	}

	// Stale expect: nothing is written, ErrPhaseConflict comes back.
	if err := st.MarkTaskPhase("t1", v1alpha1.TaskRunning, v1alpha1.TaskSuspending, "stale"); !errors.Is(err, ErrPhaseConflict) {
		t.Fatalf("want ErrPhaseConflict, got %v", err)
	}
	got, err = st.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != v1alpha1.TaskSuspending || got.Status.Reason != "suspend requested" {
		t.Fatalf("conflicting mark must not touch the task, got %s/%s", got.Status.Phase, got.Status.Reason)
	}

	if err := st.MarkTaskPhase("nope", v1alpha1.TaskRunning, v1alpha1.TaskSuspending, "x"); !errors.Is(err, ErrPhaseConflict) {
		t.Fatalf("missing task must conflict too, got %v", err)
	}
}

func TestWorkspaceDeletion(t *testing.T) {
	st := openTestStore(t)
	if err := st.UpsertWorkspace(testWorkspace("ws1")); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteWorkspace("ws1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetWorkspace("ws1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
	if err := st.DeleteWorkspace("ws1"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound for a second delete, got %v", err)
	}
}

// UpsertTask must never erase a deletion request that was persisted while the
// caller held a stale snapshot. Also exercises json_set/json_extract, which
// the mark-preserving UPSERT depends on.
func TestUpsertKeepsDeletionMark(t *testing.T) {
	st := openTestStore(t)
	if err := st.CreateTask(testTask("t1")); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTaskDeleted("t1", time.Now()); err != nil {
		t.Fatal(err)
	}

	stale := testTask("t1")
	stale.Status.Phase = v1alpha1.TaskRunning
	stale.Status.Container = 42
	if err := st.UpsertTask(stale); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.DeletionTimestamp == nil {
		t.Fatal("deletion mark was erased by a stale upsert")
	}
	if got.Status.Container != 42 || got.Status.Phase != v1alpha1.TaskRunning {
		t.Fatalf("spec/status not updated: %+v", got.Status)
	}
}

func TestReopenPersistsTasksAndMark(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir + "/px.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(testTask("t1")); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTaskDeleted("t1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := Open(dir + "/px.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()

	got, err := st2.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.DeletionTimestamp == nil {
		t.Fatal("deletion mark lost across reopen")
	}
}

func testWorkspace(name string) *v1alpha1.Workspace {
	return &v1alpha1.Workspace{
		APIVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindWorkspace,
		Metadata:   v1alpha1.ObjectMeta{Name: name},
		Spec:       v1alpha1.WorkspaceSpec{Git: v1alpha1.GitSpec{Repo: "https://example.com/a.git"}},
	}
}

// InTx must commit everything fn wrote when it returns nil — and nothing at
// all when fn errors. handleApply relies on the "nothing" half: a Task in a
// failing batch must not outlive the Workspaces it references.
func TestInTxCommitAndRollback(t *testing.T) {
	st := openTestStore(t)

	if err := st.InTx(func(tx *Store) error {
		if err := tx.CreateTask(testTask("t1")); err != nil {
			return err
		}
		if err := tx.UpsertWorkspace(testWorkspace("ws1")); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetTask("t1"); err != nil {
		t.Fatalf("committed task missing: %v", err)
	}
	if _, err := st.GetWorkspace("ws1"); err != nil {
		t.Fatalf("committed workspace missing: %v", err)
	}

	err := st.InTx(func(tx *Store) error {
		if err := tx.CreateTask(testTask("t2")); err != nil {
			return err
		}
		return errors.New("boom")
	})
	if err == nil || err.Error() != "boom" {
		t.Fatalf("want fn error propagated, got %v", err)
	}
	if _, err := st.GetTask("t2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rolled-back task must not persist, got %v", err)
	}
}

// Store methods must keep working outside a transaction after InTx changes.
func TestNonTxPathStillWorks(t *testing.T) {
	st := openTestStore(t)
	if err := st.CreateTask(testTask("t1")); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertWorkspace(testWorkspace("ws1")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ListTasks(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ListWorkspaces(); err != nil {
		t.Fatal(err)
	}
}

func testModel(name string) *v1alpha1.Model {
	return &v1alpha1.Model{
		APIVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindModel,
		Metadata:   v1alpha1.ObjectMeta{Name: name},
		Spec:       v1alpha1.ModelSpec{Provider: v1alpha1.ProviderAnthropic, APIKey: "sk-x"},
	}
}

func TestModelLifecycle(t *testing.T) {
	st := openTestStore(t)

	if _, err := st.GetModel("m1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound before upsert, got %v", err)
	}
	if err := st.UpsertModel(testModel("m1")); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetModel("m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.Provider != v1alpha1.ProviderAnthropic || got.Spec.APIKey != "sk-x" {
		t.Fatalf("spec not persisted: %+v", got.Spec)
	}
	if got.Kind != v1alpha1.KindModel {
		t.Fatalf("Kind = %q", got.Kind)
	}

	// Upsert replaces the spec.
	updated := testModel("m1")
	updated.Spec = v1alpha1.ModelSpec{Provider: v1alpha1.ProviderOpenAI, APIKey: "sk-oai", BaseURL: "https://p.example.com"}
	if err := st.UpsertModel(updated); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetModel("m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.Provider != v1alpha1.ProviderOpenAI || got.Spec.APIKey != "sk-oai" {
		t.Fatalf("upsert did not replace spec: %+v", got.Spec)
	}

	if _, err := st.ListModels(); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteModel("m1"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteModel("m1"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// Session archives are upsert-by-task (a task owns at most one capture) and
// the row dies with the task record — continuations read the capture only
// while the source task exists.
func TestSessionLifecycle(t *testing.T) {
	st := openTestStore(t)

	if _, err := st.GetSession("t1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound before save, got %v", err)
	}
	archive := []byte("tar.gz-archive-bytes")
	if err := st.SaveSession("t1", archive); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSession("t1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, archive) {
		t.Fatalf("archive not persisted: %q", got)
	}

	// Resaving replaces the blob (a retried capture overwrites nothing).
	replaced := []byte("second-capture")
	if err := st.SaveSession("t1", replaced); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetSession("t1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, replaced) {
		t.Fatalf("resave did not replace: %q", got)
	}

	if err := st.DeleteSession("t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSession("t1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
	if err := st.DeleteSession("t1"); err != nil {
		t.Fatalf("delete must be idempotent, got %v", err)
	}

	// A capture with no bytes is stored and read back empty-but-present:
	// "the source ran, there was nothing to capture" — distinct from a
	// missing row. (The controller only saves non-empty captures today, so
	// this pins the store contract, not a path it drives.)
	if err := st.SaveSession("empty", []byte{}); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetSession("empty")
	if err != nil || len(got) != 0 {
		t.Fatalf("empty capture: got %q err=%v", got, err)
	}
}

// Recreating a task name clears any session row the previous record left
// behind (a DeleteSession that failed mid-destroy, say): without this, a
// later continueFrom on that name would silently restore a dead task's
// session.
func TestCreateTaskClearsStaleSessionRow(t *testing.T) {
	st := openTestStore(t)

	if err := st.CreateTask(testTask("t1")); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSession("t1", []byte("stale")); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteTask("t1"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(testTask("t1")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSession("t1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a fresh record must clear the stale session row, got %v", err)
	}
}
