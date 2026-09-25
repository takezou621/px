package store

import (
	"errors"
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
