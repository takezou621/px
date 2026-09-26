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
	"unicode/utf8"

	_ "modernc.org/sqlite"

	"github.com/kawai/px/internal/apis/v1alpha1"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrExists        = errors.New("already exists")
	ErrPhaseConflict = errors.New("task phase changed since it was read")
	ErrSessionOwned  = errors.New("session is owned by an explicitly named capture")
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
		`CREATE TABLE IF NOT EXISTS sessions (
			task TEXT PRIMARY KEY,
			data BLOB NOT NULL,
			last_task TEXT NOT NULL DEFAULT '',
			explicit INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS events (
			seq INTEGER PRIMARY KEY AUTOINCREMENT,
			task TEXT NOT NULL,
			ts TEXT NOT NULL,
			reason TEXT NOT NULL,
			message TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_task ON events (task, seq)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	// M11: sessions grew two columns. last_task records who wrote the
	// capture last (feeds --continue-session's spec lookup); explicit
	// marks captures saved under a user-named spec.session.name — those
	// are user-owned and survive both their writing task's deletion and
	// the stale-row cleanup below. CREATE TABLE above only covers fresh
	// databases, so an older file gets the columns added here. Each
	// backfill is re-run on every open (empty last_task only ever means
	// a pre-M11 row): a crash between ALTER and backfill then repairs
	// itself on the next start instead of shipping blank metadata.
	rows, err := db.Query(`PRAGMA table_info(sessions)`)
	if err != nil {
		return nil, fmt.Errorf("migrate: inspect sessions: %w", err)
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return nil, fmt.Errorf("migrate: scan sessions schema: %w", err)
		}
		have[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: inspect sessions: %w", err)
	}
	for _, col := range []struct{ name, ddl string }{
		{"last_task", `ALTER TABLE sessions ADD COLUMN last_task TEXT NOT NULL DEFAULT ''`},
		{"explicit", `ALTER TABLE sessions ADD COLUMN explicit INTEGER NOT NULL DEFAULT 0`},
	} {
		if have[col.name] {
			continue
		}
		if _, err := db.Exec(col.ddl); err != nil {
			// Another opener of the same file can have raced us past the
			// PRAGMA check — SQLite serializes the ALTER, so "duplicate
			// column" means the work is already done, not a failure.
			if strings.Contains(err.Error(), "duplicate column name") {
				continue
			}
			return nil, fmt.Errorf("migrate: add sessions.%s: %w", col.name, err)
		}
	}
	if _, err := db.Exec(`UPDATE sessions SET last_task = task WHERE last_task = ''`); err != nil {
		return nil, fmt.Errorf("migrate: backfill sessions.last_task: %w", err)
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
	if err != nil {
		return err
	}
	// A fresh record invalidates any default-lifetime session row a
	// same-named predecessor could have left behind (a DeleteSession that
	// failed mid-destroy): without this, a later continueFrom on that name
	// would silently restore a dead task's session. Explicitly named rows
	// are user-owned and outlive tasks, so they are left alone.
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE task = ? AND explicit = 0`, t.Metadata.Name); err != nil {
		return fmt.Errorf("clear stale session row for %s: %w", t.Metadata.Name, err)
	}
	// Same shape as the session row above: a predecessor's orphaned events
	// must not read as the new task's history.
	if _, err := s.db.Exec(`DELETE FROM events WHERE task = ?`, t.Metadata.Name); err != nil {
		return fmt.Errorf("clear stale events for %s: %w", t.Metadata.Name, err)
	}
	return nil
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

// DeleteTask drops a task and its events in one transaction: torn halves
// would leave orphan events that nothing ever cleans up (they'd surface in
// the cross-task feed as history for a task that doesn't exist).
func (s *Store) DeleteTask(name string) error {
	return s.InTx(func(tx *Store) error {
		res, err := tx.db.Exec(`DELETE FROM tasks WHERE name = ?`, name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		// Events die with the task: they describe one record's life, and the
		// delete is the record's end. Same idempotent shape as DeleteSession.
		if _, err := tx.db.Exec(`DELETE FROM events WHERE task = ?`, name); err != nil {
			return fmt.Errorf("drop events for %s: %w", name, err)
		}
		return nil
	})
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

// SaveSession stores one captured session archive. Upsert by design: a
// capture retried after a partial write replaces the row, and a capture is
// a full snapshot — resaving with different content means the first capture
// was never settled, so last-writer-wins is the safe convergence.
// lastTask records the writer — the CLI's --continue-session sugar copies
// its spec. explicit marks a capture saved under a user-named
// spec.session.name: the row is user-owned, so a default (task-named)
// capture colliding with that name neither overwrites nor deletes it —
// such a write returns ErrSessionOwned instead — while only DeleteSession
// drops the row, never the task-lifetime cleanups (which target
// explicit=0 rows). The row key is the capture name (the task's
// spec.session.name, defaulting to the task name), so the parameter reads
// name even though the column is still "task" from the M8 schema. On an
// update the timestamp moves forward so the CLI shows the last writer's
// time, not the first capture's.
func (s *Store) SaveSession(name, lastTask string, data []byte, explicit bool) error {
	res, err := s.db.Exec(`INSERT INTO sessions (task, data, last_task, explicit) VALUES (?, ?, ?, ?)
		ON CONFLICT(task) DO UPDATE SET data=excluded.data, last_task=excluded.last_task, explicit=excluded.explicit, created_at=datetime('now')
		WHERE sessions.explicit = 0 OR excluded.explicit = 1`,
		name, data, lastTask, explicit)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: session %q keeps its explicit capture", ErrSessionOwned, name)
	}
	return nil
}

// GetSession returns the named capture's archive, or ErrNotFound when no
// row carries that name. The name is the capture's key: for the M8
// default it is the writing task's name, for an explicit
// spec.session.name it is that name.
func (s *Store) GetSession(name string) ([]byte, error) {
	var data []byte
	err := s.db.QueryRow(`SELECT data FROM sessions WHERE task = ?`, name).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// DeleteSession drops a capture row by name — whichever lifetime it has.
// Idempotent, so both the px delete session path and the task-destroy path
// can call it unconditionally.
func (s *Store) DeleteSession(name string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE task = ?`, name)
	return err
}

// DeleteDefaultSession drops the capture row a task's own name keys — the
// M8 default lifetime, where the row dies with the task record. Explicitly
// named rows (explicit=1) survive, and so does an explicit row that happens
// to share the task's name: the name collision must not turn one task's
// creation or deletion into another conversation's data loss.
func (s *Store) DeleteDefaultSession(name string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE task = ? AND explicit = 0`, name)
	return err
}

// ListSessions returns every capture's metadata, oldest first. WrittenAt
// comes from the schema's created_at default (SQLite datetime('now') is
// UTC "YYYY-MM-DD HH:MM:SS"); a row it cannot parse keeps its zero time
// rather than failing the listing.
func (s *Store) ListSessions() ([]v1alpha1.SessionInfo, error) {
	rows, err := s.db.Query(`SELECT task, LENGTH(data), last_task, created_at
		FROM sessions ORDER BY created_at ASC, task ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []v1alpha1.SessionInfo
	for rows.Next() {
		var info v1alpha1.SessionInfo
		var created string
		if err := rows.Scan(&info.Name, &info.Bytes, &info.LastTask, &created); err != nil {
			return nil, err
		}
		if t, err := time.Parse("2006-01-02 15:04:05", created); err == nil {
			info.WrittenAt = t.UTC()
		}
		out = append(out, info)
	}
	return out, rows.Err()
}

// GetSessionInfo returns one capture's metadata, or ErrNotFound. Same
// shape as ListSessions' rows; the archive itself is not served.
func (s *Store) GetSessionInfo(name string) (v1alpha1.SessionInfo, error) {
	var info v1alpha1.SessionInfo
	var created string
	err := s.db.QueryRow(`SELECT task, LENGTH(data), last_task, created_at
		FROM sessions WHERE task = ?`, name).
		Scan(&info.Name, &info.Bytes, &info.LastTask, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return v1alpha1.SessionInfo{}, ErrNotFound
	}
	if err != nil {
		return v1alpha1.SessionInfo{}, err
	}
	if t, err := time.Parse("2006-01-02 15:04:05", created); err == nil {
		info.WrittenAt = t.UTC()
	}
	return info, nil
}

// CountSessions returns the number of capture rows for the px_sessions
// gauge.
func (s *Store) CountSessions() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n)
	return n, err
}

// maxEventMessageBytes caps one event's message: a message is a summary
// written by the controller, and a runaway error string (say, an embedded
// panic dump) must not bloat the events table.
const maxEventMessageBytes = 4096

// RecordEvent appends one event for a task and prunes the task's history
// down to v1alpha1.MaxTaskEvents (newest kept). Both statements run per
// call rather than in a transaction: the prune is keyed on this INSERT's
// own task, so a concurrent record only makes the prune slightly late.
// The insert only lands when the task still exists; recording against a
// deleted task is a silent no-op, not an error.
func (s *Store) RecordEvent(task string, at time.Time, reason, message string) error {
	if len(message) > maxEventMessageBytes {
		// Back off to a rune boundary so the stored text stays valid UTF-8
		// (JSON marshaling would otherwise swap the torn bytes for U+FFFD).
		cut := maxEventMessageBytes
		for cut > 0 && !utf8.RuneStart(message[cut]) {
			cut--
		}
		message = message[:cut]
	}
	// Conditional insert: a task delete can race an in-flight record (the
	// controller records around the Mark* transitions), and the foreign key
	// added here wouldn't retrofit onto an existing database anyway. An
	// event whose task is gone must not become an orphan row; dropping the
	// write silently is correct — the task's history died with it.
	res, err := s.db.Exec(`INSERT INTO events (task, ts, reason, message)
		SELECT ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM tasks WHERE name = ?)`,
		task, at.UTC().Format(time.RFC3339Nano), reason, message, task)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	_, err = s.db.Exec(`DELETE FROM events WHERE task = ? AND seq NOT IN
		(SELECT seq FROM events WHERE task = ? ORDER BY seq DESC LIMIT ?)`,
		task, task, v1alpha1.MaxTaskEvents)
	return err
}

// ListTaskEvents returns one task's events, newest first.
func (s *Store) ListTaskEvents(task string) ([]*v1alpha1.Event, error) {
	rows, err := s.db.Query(`SELECT task, ts, reason, message FROM events WHERE task = ? ORDER BY seq DESC`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

// ListEvents returns up to limit recent events across all tasks, newest
// first — the feed behind GET /v1/events.
func (s *Store) ListEvents(limit int) ([]*v1alpha1.Event, error) {
	rows, err := s.db.Query(`SELECT task, ts, reason, message FROM events ORDER BY seq DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

// CountEvents returns the number of stored events (all tasks).
func (s *Store) CountEvents() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n)
	return n, err
}

func scanEvents(rows *sql.Rows) ([]*v1alpha1.Event, error) {
	var evs []*v1alpha1.Event
	for rows.Next() {
		var ev v1alpha1.Event
		var ts string
		if err := rows.Scan(&ev.Task, &ts, &ev.Reason, &ev.Message); err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return nil, fmt.Errorf("event ts %q: %w", ts, err)
		}
		ev.Time = t
		evs = append(evs, &ev)
	}
	return evs, rows.Err()
}

// SessionBytesTotal sums every captured session archive — the gauge behind
// px_sessions_bytes, the one number showing how much of the store is
// conversation history.
func (s *Store) SessionBytesTotal() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(LENGTH(data)), 0) FROM sessions`).Scan(&n)
	return n, err
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
