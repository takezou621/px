package store

import (
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
