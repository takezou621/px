// Package store persists px objects in an embedded SQLite database.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kawai/px/internal/apis/v1alpha1"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrExists        = errors.New("already exists")
	ErrPhaseConflict = errors.New("task phase changed since it was read")
)

// executor is the subset of *sql.DB and *sql.Tx the store needs, so InTx can
// bind every store method to a transaction.
type executor interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

type Store struct {
	db executor
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS tasks (
			name TEXT PRIMARY KEY,
			spec TEXT NOT NULL,
			status TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS workspaces (
			name TEXT PRIMARY KEY,
			spec TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS models (
			name TEXT PRIMARY KEY,
			spec TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS gateways (
			name TEXT PRIMARY KEY,
			spec TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	// The database holds Model API keys in plaintext, so it is a credential
	// store: enforce 0600 regardless of the umask that created it. The WAL
	// and SHM siblings carry recently written pages, so they get the same
	// mode; they may be absent after a clean close.
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(f, 0o600); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("chmod %s: %w", f, err)
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.(*sql.DB).Close() }

// InTx runs fn with every store method bound to one SQLite transaction: fn
// either commits all its writes or none of them. handleApply uses this so a
// reconcile tick can never observe a Task whose referenced Workspaces are
// still on their way into the store.
func (s *Store) InTx(fn func(*Store) error) error {
	tx, err := s.db.(*sql.DB).Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op once Commit succeeded
	if err := fn(&Store{db: tx}); err != nil {
		return err
	}
	return tx.Commit()
}

// UpsertTask stores the task spec and merges the given status. An already
// persisted deletionTimestamp survives: the controller works on snapshots,
// and a status write must never erase a deletion request that arrived while
// the snapshot was being processed.
func (s *Store) UpsertTask(t *v1alpha1.Task) error {
	spec, err := json.Marshal(t.Spec)
	if err != nil {
		return err
	}
	status, err := json.Marshal(t.Status)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO tasks (name, spec, status) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			spec=excluded.spec,
			status=CASE
				WHEN json_extract(excluded.status,'$.deletionTimestamp') IS NULL
					AND json_extract(tasks.status,'$.deletionTimestamp') IS NOT NULL
				THEN json_set(excluded.status,'$.deletionTimestamp',json_extract(tasks.status,'$.deletionTimestamp'))
				ELSE excluded.status
			END,
			updated_at=datetime('now')`,
		t.Metadata.Name, string(spec), string(status))
	return err
}

// MarkTaskDeleted sets the deletion timestamp in a single statement, without
// touching any other status field: a delete racing with reconcile cannot
// clobber a concurrently persisted VMID.
func (s *Store) MarkTaskDeleted(name string, at time.Time) error {
	res, err := s.db.Exec(
		`UPDATE tasks SET status = json_set(status, '$.deletionTimestamp', ?), updated_at=datetime('now') WHERE name = ?`,
		at.UTC().Format(time.RFC3339Nano), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkTaskPhase sets one task's phase and reason in a single statement —
// same reasoning as MarkTaskDeleted: suspend/resume requests are API-side
// writes that reconcile observes later, so they must never overwrite a
// VMID or timestamps a concurrent persist just landed. The update is guarded
// on the phase the caller read: if reconcile transitioned the task (say to
// Succeeded) between the caller's read and this write, the row does not
// match, nothing is written, and ErrPhaseConflict tells the caller the
// decision was made on stale state instead of letting the mark freeze a
// finished task into Suspending forever.
func (s *Store) MarkTaskPhase(name string, expect, phase v1alpha1.TaskPhase, reason string) error {
	res, err := s.db.Exec(
		`UPDATE tasks SET status = json_set(json_set(status, '$.phase', ?), '$.reason', ?), updated_at=datetime('now')
		 WHERE name = ? AND json_extract(status,'$.phase') = ?`,
		string(phase), reason, name, string(expect))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrPhaseConflict
	}
	return nil
}

// CreateTask inserts a new task atomically; it fails with ErrExists if the
// name is taken (unlike UpsertTask, which overwrites).
func (s *Store) CreateTask(t *v1alpha1.Task) error {
	spec, err := json.Marshal(t.Spec)
	if err != nil {
		return err
	}
	status, err := json.Marshal(t.Status)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO tasks (name, spec, status) VALUES (?, ?, ?)`,
		t.Metadata.Name, string(spec), string(status))
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrExists
	}
	return err
}

func (s *Store) GetTask(name string) (*v1alpha1.Task, error) {
	row := s.db.QueryRow(`SELECT name, spec, status FROM tasks WHERE name = ?`, name)
	return scanTask(row)
}

func (s *Store) ListTasks() ([]*v1alpha1.Task, error) {
	rows, err := s.db.Query(`SELECT name, spec, status FROM tasks ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []*v1alpha1.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func (s *Store) DeleteTask(name string) error {
	res, err := s.db.Exec(`DELETE FROM tasks WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) UpsertWorkspace(w *v1alpha1.Workspace) error {
	spec, err := json.Marshal(w.Spec)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO workspaces (name, spec) VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET spec=excluded.spec`, w.Metadata.Name, string(spec))
	return err
}

func (s *Store) GetWorkspace(name string) (*v1alpha1.Workspace, error) {
	row := s.db.QueryRow(`SELECT name, spec FROM workspaces WHERE name = ?`, name)
	return scanWorkspace(row)
}

func (s *Store) ListWorkspaces() ([]*v1alpha1.Workspace, error) {
	rows, err := s.db.Query(`SELECT name, spec FROM workspaces ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var wss []*v1alpha1.Workspace
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		wss = append(wss, w)
	}
	return wss, rows.Err()
}

func (s *Store) DeleteWorkspace(name string) error {
	res, err := s.db.Exec(`DELETE FROM workspaces WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpsertModel stores the model spec (including its API key — write-only at
// the API layer, see server.redacted).
func (s *Store) UpsertModel(m *v1alpha1.Model) error {
	spec, err := json.Marshal(m.Spec)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO models (name, spec) VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET spec=excluded.spec`, m.Metadata.Name, string(spec))
	return err
}

func (s *Store) GetModel(name string) (*v1alpha1.Model, error) {
	row := s.db.QueryRow(`SELECT name, spec FROM models WHERE name = ?`, name)
	return scanModel(row)
}

func (s *Store) ListModels() ([]*v1alpha1.Model, error) {
	rows, err := s.db.Query(`SELECT name, spec FROM models ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var models []*v1alpha1.Model
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		models = append(models, m)
	}
	return models, rows.Err()
}

func (s *Store) DeleteModel(name string) error {
	res, err := s.db.Exec(`DELETE FROM models WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) UpsertGateway(g *v1alpha1.Gateway) error {
	spec, err := json.Marshal(g.Spec)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO gateways (name, spec) VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET spec=excluded.spec`, g.Metadata.Name, string(spec))
	return err
}

func (s *Store) GetGateway(name string) (*v1alpha1.Gateway, error) {
	row := s.db.QueryRow(`SELECT name, spec FROM gateways WHERE name = ?`, name)
	return scanGateway(row)
}

func (s *Store) ListGateways() ([]*v1alpha1.Gateway, error) {
	rows, err := s.db.Query(`SELECT name, spec FROM gateways ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var gws []*v1alpha1.Gateway
	for rows.Next() {
		g, err := scanGateway(rows)
		if err != nil {
			return nil, err
		}
		gws = append(gws, g)
	}
	return gws, rows.Err()
}

func (s *Store) DeleteGateway(name string) error {
	res, err := s.db.Exec(`DELETE FROM gateways WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanWorkspace(row rowScanner) (*v1alpha1.Workspace, error) {
	w := &v1alpha1.Workspace{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindWorkspace}
	var spec string
	if err := row.Scan(&w.Metadata.Name, &spec); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(spec), &w.Spec); err != nil {
		return nil, fmt.Errorf("workspace %s spec: %w", w.Metadata.Name, err)
	}
	return w, nil
}

func scanTask(row rowScanner) (*v1alpha1.Task, error) {
	t := &v1alpha1.Task{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindTask}
	var spec, status string
	if err := row.Scan(&t.Metadata.Name, &spec, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(spec), &t.Spec); err != nil {
		return nil, fmt.Errorf("task %s spec: %w", t.Metadata.Name, err)
	}
	if err := json.Unmarshal([]byte(status), &t.Status); err != nil {
		return nil, fmt.Errorf("task %s status: %w", t.Metadata.Name, err)
	}
	return t, nil
}

func scanModel(row rowScanner) (*v1alpha1.Model, error) {
	m := &v1alpha1.Model{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindModel}
	var spec string
	if err := row.Scan(&m.Metadata.Name, &spec); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(spec), &m.Spec); err != nil {
		return nil, fmt.Errorf("model %s spec: %w", m.Metadata.Name, err)
	}
	return m, nil
}

func scanGateway(row rowScanner) (*v1alpha1.Gateway, error) {
	g := &v1alpha1.Gateway{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindGateway}
	var spec string
	if err := row.Scan(&g.Metadata.Name, &spec); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(spec), &g.Spec); err != nil {
		return nil, fmt.Errorf("gateway %s spec: %w", g.Metadata.Name, err)
	}
	return g, nil
}
