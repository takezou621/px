// Package store persists px objects in an embedded SQLite database.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/kawai/px/internal/apis/v1alpha1"
)

var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
)

type Store struct {
	db *sql.DB
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
	} {
		if _, err := db.Exec(ddl); err != nil {
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// UpsertTask stores the task spec and merges the given status.
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
		ON CONFLICT(name) DO UPDATE SET spec=excluded.spec, status=excluded.status, updated_at=datetime('now')`,
		t.Metadata.Name, string(spec), string(status))
	return err
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
	w := &v1alpha1.Workspace{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindWorkspace}
	var spec string
	if err := row.Scan(&w.Metadata.Name, &spec); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(spec), &w.Spec); err != nil {
		return nil, err
	}
	return w, nil
}

type rowScanner interface{ Scan(dest ...any) error }

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
