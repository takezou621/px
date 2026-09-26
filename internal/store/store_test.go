package store

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	_ "modernc.org/sqlite"

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
	if err := st.SaveSession("t1", "t1", archive, false); err != nil {
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
	if err := st.SaveSession("t1", "t1", replaced, false); err != nil {
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
	if err := st.SaveSession("empty", "empty", []byte{}, false); err != nil {
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
	if err := st.SaveSession("t1", "t1", []byte("stale"), false); err != nil {
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

// A capture under an explicit spec.session.name is user-owned. A default
// (task-named) capture colliding with that name — from a task that happens
// to share it — must not overwrite or delete the row: the write is refused
// (ErrSessionOwned), the task-lifetime cleanups skip it, and only
// DeleteSession drops it. Without this, `px run -name conv` could silently
// clobber a user's named conversation.
func TestExplicitSessionOwnership(t *testing.T) {
	st := openTestStore(t)

	if err := st.SaveSession("conv", "w1", []byte("named-capture"), true); err != nil {
		t.Fatal(err)
	}

	// The writer's deletion (DeleteDefaultSession) must not touch it.
	if err := st.DeleteDefaultSession("conv"); err != nil {
		t.Fatal(err)
	}
	if data, err := st.GetSession("conv"); err != nil || string(data) != "named-capture" {
		t.Fatalf("explicit capture must survive the default cleanup, got %q err=%v", data, err)
	}

	// A same-named task's stale clear (CreateTask) must not touch it either.
	if err := st.CreateTask(testTask("conv")); err != nil {
		t.Fatal(err)
	}
	if data, err := st.GetSession("conv"); err != nil || string(data) != "named-capture" {
		t.Fatalf("explicit capture must survive a same-named task's creation, got %q err=%v", data, err)
	}

	// And that task's own default capture must not overwrite the row.
	err := st.SaveSession("conv", "conv", []byte("default-capture"), false)
	if !errors.Is(err, ErrSessionOwned) {
		t.Fatalf("default write onto an explicit capture: want ErrSessionOwned, got %v", err)
	}
	if data, err := st.GetSession("conv"); err != nil || string(data) != "named-capture" {
		t.Fatalf("refused write must not touch the row, got %q err=%v", data, err)
	}

	// An explicit writer may still update the row (several tasks may write
	// one named conversation), and DeleteSession drops it on request.
	if err := st.SaveSession("conv", "w2", []byte("named-capture-v2"), true); err != nil {
		t.Fatal(err)
	}
	if data, err := st.GetSession("conv"); err != nil || string(data) != "named-capture-v2" {
		t.Fatalf("explicit resave must replace, got %q err=%v", data, err)
	}
	if err := st.DeleteSession("conv"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSession("conv"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteSession must drop the explicit row, got %v", err)
	}

	// Sanity: the default path still works for rows that are not explicit.
	if err := st.SaveSession("conv", "conv", []byte("default-now"), false); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteDefaultSession("conv"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSession("conv"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteDefaultSession must clear default rows, got %v", err)
	}
}

func TestCreateTaskClearsStaleEvents(t *testing.T) {
	st := openTestStore(t)

	if err := st.CreateTask(testTask("t1")); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordEvent("t1", time.Now().UTC(), "Provisioning", "past life"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteTask("t1"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(testTask("t1")); err != nil {
		t.Fatal(err)
	}
	if evs, err := st.ListTaskEvents("t1"); err != nil || len(evs) != 0 {
		t.Fatalf("a fresh record must clear stale events, got %d err=%v", len(evs), err)
	}
}

// Events are append-only per task, newest first on read, pruned to
// MaxTaskEvents per task, and they die with the task record: the delete is
// the record's end, and its history must not outlive it.
func TestEventLifecycle(t *testing.T) {
	st := openTestStore(t)
	_ = st.CreateTask(testTask("t1"))

	if evs, err := st.ListTaskEvents("t1"); err != nil || len(evs) != 0 {
		t.Fatalf("fresh task: want no events, got %v err=%v", evs, err)
	}

	base := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	// A record racing a task's delete must not become an orphan row in the
	// cross-task feed: recording for a vanished task is a silent no-op.
	if err := st.RecordEvent("ghost", base, "Deleting", "no such task"); err != nil {
		t.Fatal(err)
	}
	if n, err := st.CountEvents(); err != nil || n != 0 {
		t.Fatalf("orphan record: CountEvents = %d err=%v, want 0", n, err)
	}

	for i, r := range []string{"Provisioning", "Scheduled", "Running"} {
		if err := st.RecordEvent("t1", base.Add(time.Duration(i)*time.Second), r, "msg "+r); err != nil {
			t.Fatal(err)
		}
	}

	evs, err := st.ListTaskEvents("t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 || evs[0].Reason != "Running" || evs[2].Reason != "Provisioning" {
		t.Fatalf("want newest-first [Running Scheduled Provisioning], got %+v", evs)
	}
	if evs[0].Task != "t1" || !evs[0].Time.Equal(base.Add(2*time.Second)) || evs[0].Message != "msg Running" {
		t.Fatalf("event fields not round-tripped: %+v", evs[0])
	}

	// The cross-task feed honors the limit, taking the newest rows.
	if err := st.RecordEvent("t1", base.Add(3*time.Second), "Succeeded", "msg Succeeded"); err != nil {
		t.Fatal(err)
	}
	feed, err := st.ListEvents(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed) != 2 || feed[0].Reason != "Succeeded" || feed[1].Reason != "Running" {
		t.Fatalf("feed must be the 2 newest, got %+v", feed)
	}
	if n, err := st.CountEvents(); err != nil || n != 4 {
		t.Fatalf("CountEvents = %d err=%v, want 4", n, err)
	}

	// The per-task cap prunes the oldest rows, newest kept.
	for i := 0; i < v1alpha1.MaxTaskEvents; i++ {
		if err := st.RecordEvent("t1", base.Add(time.Duration(4+i)*time.Second), "Tick", "filler"); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := st.CountEvents(); err != nil || n != v1alpha1.MaxTaskEvents {
		t.Fatalf("after cap: CountEvents = %d err=%v, want %d", n, err, v1alpha1.MaxTaskEvents)
	}
	oldest, err := st.ListTaskEvents("t1")
	if err != nil {
		t.Fatal(err)
	}
	// 4 real rows + 200 fillers = 204; the prune drops the 4 oldest.
	if last := oldest[len(oldest)-1]; last.Reason != "Tick" {
		t.Fatalf("prune must drop the oldest rows first, oldest kept = %s", last.Reason)
	}

	// Deleting the task drops its history.
	if err := st.DeleteTask("t1"); err != nil {
		t.Fatal(err)
	}
	if n, err := st.CountEvents(); err != nil || n != 0 {
		t.Fatalf("events must die with the task: CountEvents = %d err=%v", n, err)
	}
}

// SessionBytesTotal backs px_sessions_bytes — the one number showing how
// much of the store is conversation history.
func TestSessionBytesTotal(t *testing.T) {
	st := openTestStore(t)

	if n, err := st.SessionBytesTotal(); err != nil || n != 0 {
		t.Fatalf("empty store: got %d err=%v, want 0", n, err)
	}
	if err := st.SaveSession("t1", "t1", []byte("12345"), false); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSession("t2", "t2", []byte("abc"), false); err != nil {
		t.Fatal(err)
	}
	if n, err := st.SessionBytesTotal(); err != nil || n != 8 {
		t.Fatalf("got %d err=%v, want 8", n, err)
	}
}

// Sessions grew a last_task column in M11. An M8-era database file has the
// old three-column sessions table, so reopening it must add the columns and
// backfill the rows — those captures were keyed by their writing task's
// name, so last_task reads back as that name, and they stay default
// captures (explicit=0), dying with their tasks as they always did.
func TestSessionsMigrationBackfillsLastTask(t *testing.T) {
	path := t.TempDir() + "/px.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (
		task TEXT PRIMARY KEY,
		data BLOB NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions (task, data) VALUES ('m8task', ?)`, []byte("old-capture")); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	info, err := st.GetSessionInfo("m8task")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "m8task" || info.LastTask != "m8task" || info.Bytes != len("old-capture") {
		t.Fatalf("backfill wrong: %+v", info)
	}

	var explicit int
	if err := st.db.QueryRow(`SELECT explicit FROM sessions WHERE task = 'm8task'`).Scan(&explicit); err != nil {
		t.Fatal(err)
	}
	if explicit != 0 {
		t.Fatalf("migrated rows must be default captures, explicit=%d", explicit)
	}
	if err := st.DeleteDefaultSession("m8task"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSession("m8task"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("migrated rows must clear with the task lifetime, got %v", err)
	}

	// And the migrated schema still writes.
	if err := st.SaveSession("m8task", "newer", []byte("again"), false); err != nil {
		t.Fatal(err)
	}
	info, err = st.GetSessionInfo("m8task")
	if err != nil {
		t.Fatal(err)
	}
	if info.LastTask != "newer" {
		t.Fatalf("post-migration write lost lastTask: %+v", info)
	}
}

// GetSessionInfo and ListSessions back the sessions API and the CLI: the
// metadata rows must carry the writing task, and a second writer under the
// same name must move lastTask (last-writer-wins is the contract).
func TestSessionInfoListAndCount(t *testing.T) {
	st := openTestStore(t)

	sessions, err := st.ListSessions()
	if err != nil || len(sessions) != 0 {
		t.Fatalf("empty store: got %+v err=%v", sessions, err)
	}
	if _, err := st.GetSessionInfo("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session: want ErrNotFound, got %v", err)
	}

	if err := st.SaveSession("one", "writer-a", []byte("aaaa"), false); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSession("two", "writer-b", []byte("bb"), false); err != nil {
		t.Fatal(err)
	}
	info, err := st.GetSessionInfo("one")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "one" || info.LastTask != "writer-a" || info.Bytes != 4 {
		t.Fatalf("info wrong: %+v", info)
	}
	if info.WrittenAt.IsZero() {
		t.Fatalf("WrittenAt must be set, got %+v", info)
	}

	sessions, err = st.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].Name != "one" || sessions[1].Name != "two" {
		t.Fatalf("list wrong: %+v", sessions)
	}

	// The same-named capture rewritten by a different task: bytes move and
	// lastTask follows the writer, the row itself is not duplicated.
	if err := st.SaveSession("one", "writer-c", []byte("cccccc"), false); err != nil {
		t.Fatal(err)
	}
	sessions, err = st.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("a named rewrite must upsert, got %+v", sessions)
	}
	info, err = st.GetSessionInfo("one")
	if err != nil {
		t.Fatal(err)
	}
	if info.LastTask != "writer-c" || info.Bytes != 6 {
		t.Fatalf("last writer must win: %+v", info)
	}

	if n, err := st.CountSessions(); err != nil || n != 2 {
		t.Fatalf("count: got %d err=%v, want 2", n, err)
	}
}

// The CLI shows a capture's written-at as the last writer's time, so a
// resave must move the timestamp — an upsert that kept the first capture's
// time would show a stale conversation as fresh.
func TestSessionResaveMovesWrittenAt(t *testing.T) {
	st := openTestStore(t)

	if err := st.SaveSession("t1", "w1", []byte("first"), false); err != nil {
		t.Fatal(err)
	}
	// Rewind the stored time to prove the next save moves it forward
	// (datetime('now') keeps second precision, so an immediate resave
	// would otherwise land on the same second).
	if _, err := st.db.Exec(`UPDATE sessions SET created_at = '2000-01-01 00:00:00' WHERE task = 't1'`); err != nil {
		t.Fatal(err)
	}
	info, err := st.GetSessionInfo("t1")
	if err != nil {
		t.Fatal(err)
	}
	if !info.WrittenAt.Equal(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("rewind failed, got %v", info.WrittenAt)
	}

	if err := st.SaveSession("t1", "w2", []byte("second"), false); err != nil {
		t.Fatal(err)
	}
	info, err = st.GetSessionInfo("t1")
	if err != nil {
		t.Fatal(err)
	}
	if info.LastTask != "w2" {
		t.Fatalf("resave lost lastTask: %+v", info)
	}
	if !info.WrittenAt.After(time.Now().Add(-time.Minute)) {
		t.Fatalf("resave must move written-at forward, got %v", info.WrittenAt)
	}
}
