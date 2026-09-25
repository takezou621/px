// Package server exposes the px control plane over plain HTTP/JSON.
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/kawai/px/internal/apis/v1alpha1"
	"github.com/kawai/px/internal/controller"
	"github.com/kawai/px/internal/store"
)

const maxBodyBytes = 4 << 20

type Server struct {
	store *store.Store
	ctl   *controller.Controller
	prov  controller.Provisioner
	log   *slog.Logger
}

func New(st *store.Store, ctl *controller.Controller, prov controller.Provisioner, log *slog.Logger) *Server {
	return &Server{store: st, ctl: ctl, prov: prov, log: log}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/apply", s.handleApply)
	mux.HandleFunc("GET /v1/tasks", s.handleListTasks)
	mux.HandleFunc("GET /v1/tasks/{name}", s.handleGetTask)
	mux.HandleFunc("DELETE /v1/tasks/{name}", s.handleDeleteTask)
	mux.HandleFunc("GET /v1/tasks/{name}/logs", s.handleTaskLogs)
	mux.HandleFunc("POST /v1/tasks/{name}/exec", s.handleTaskExec)
	mux.HandleFunc("POST /v1/tasks/{name}/suspend", s.handleTaskSuspend)
	mux.HandleFunc("POST /v1/tasks/{name}/resume", s.handleTaskResume)
	mux.HandleFunc("GET /v1/workspaces", s.handleListWorkspaces)
	mux.HandleFunc("GET /v1/workspaces/{name}", s.handleGetWorkspace)
	mux.HandleFunc("DELETE /v1/workspaces/{name}", s.handleDeleteWorkspace)
	mux.HandleFunc("GET /v1/models", s.handleListModels)
	mux.HandleFunc("GET /v1/models/{name}", s.handleGetModel)
	mux.HandleFunc("DELETE /v1/models/{name}", s.handleDeleteModel)
	mux.HandleFunc("GET /v1/gateways", s.handleListGateways)
	mux.HandleFunc("GET /v1/gateways/{name}", s.handleGetGateway)
	mux.HandleFunc("DELETE /v1/gateways/{name}", s.handleDeleteGateway)
	mux.HandleFunc("GET /v1/watch", s.handleWatch)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		httpError(w, http.StatusBadRequest, "read body: %v", err)
		return
	}
	if len(body) > maxBodyBytes {
		httpError(w, http.StatusRequestEntityTooLarge, "manifest exceeds %d bytes", maxBodyBytes)
		return
	}
	manifests, err := v1alpha1.ParseManifests(bytes.NewReader(body))
	if err != nil {
		httpError(w, http.StatusBadRequest, "%v", err)
		return
	}
	// An empty stream must not read as a successful no-op apply: a caller
	// whose manifest generation failed (empty stdin, unreadable file) would
	// otherwise see exit 0 and believe its objects were configured.
	if len(manifests) == 0 {
		httpError(w, http.StatusBadRequest, "no manifests in request body")
		return
	}
	var results []string
	err = s.store.InTx(func(tx *store.Store) error {
		var aerr error
		results, aerr = applyObjects(tx, manifests)
		return aerr
	})
	if err != nil {
		var conflict *applyConflictError
		if errors.As(err, &conflict) {
			httpError(w, http.StatusConflict, "%s", conflict.msg)
			return
		}
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"results": results})
}

// applyConflictError is a 409 apply (task name already taken). It aborts the
// apply transaction, so a partially applied batch never persists — a Task in
// a failing batch cannot outlive the Workspaces it references.
type applyConflictError struct{ msg string }

func (e *applyConflictError) Error() string { return e.msg }

// applyObjects persists every manifest in document order. It runs inside the
// caller's transaction, so the reconcile loop cannot observe the batch
// half-applied (e.g. a Task whose Workspace is not yet stored).
func applyObjects(st *store.Store, manifests []*v1alpha1.Manifest) ([]string, error) {
	var results []string
	for _, m := range manifests {
		switch m.Kind {
		case v1alpha1.KindWorkspace:
			ws := &v1alpha1.Workspace{
				APIVersion: v1alpha1.APIVersion,
				Kind:       v1alpha1.KindWorkspace,
				Metadata:   m.Metadata,
				Spec:       *m.Workspace,
			}
			if err := st.UpsertWorkspace(ws); err != nil {
				return nil, fmt.Errorf("upsert workspace %s: %w", m.Metadata.Name, err)
			}
			results = append(results, fmt.Sprintf("workspace.px.io/%s configured", m.Metadata.Name))
		case v1alpha1.KindModel:
			mo := &v1alpha1.Model{
				APIVersion: v1alpha1.APIVersion,
				Kind:       v1alpha1.KindModel,
				Metadata:   m.Metadata,
				Spec:       *m.Model,
			}
			if err := st.UpsertModel(mo); err != nil {
				return nil, fmt.Errorf("upsert model %s: %w", m.Metadata.Name, err)
			}
			results = append(results, fmt.Sprintf("model.px.io/%s configured", m.Metadata.Name))
		case v1alpha1.KindGateway:
			gw := &v1alpha1.Gateway{
				APIVersion: v1alpha1.APIVersion,
				Kind:       v1alpha1.KindGateway,
				Metadata:   m.Metadata,
				Spec:       *m.Gateway,
			}
			if err := st.UpsertGateway(gw); err != nil {
				return nil, fmt.Errorf("upsert gateway %s: %w", m.Metadata.Name, err)
			}
			results = append(results, fmt.Sprintf("gateway.px.io/%s configured", m.Metadata.Name))
		case v1alpha1.KindTask:
			t := &v1alpha1.Task{
				APIVersion: v1alpha1.APIVersion,
				Kind:       v1alpha1.KindTask,
				Metadata:   m.Metadata,
				Spec:       *m.Task,
			}
			t.Status.Phase = v1alpha1.TaskPending
			t.Status.Reason = "queued"
			if err := st.CreateTask(t); err != nil {
				if errors.Is(err, store.ErrExists) {
					return nil, &applyConflictError{msg: fmt.Sprintf("task %q already exists; delete it first", m.Metadata.Name)}
				}
				return nil, fmt.Errorf("create task %s: %w", m.Metadata.Name, err)
			}
			results = append(results, fmt.Sprintf("task.px.io/%s created", m.Metadata.Name))
		}
	}
	return results, nil
}

func (s *Server) handleListTasks(w http.ResponseWriter, _ *http.Request) {
	tasks, err := s.store.ListTasks()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if tasks == nil {
		tasks = []*v1alpha1.Task{}
	}
	writeJSON(w, http.StatusOK, tasks)
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.GetTask(r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	err := s.ctl.RequestDestroy(r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleting"})
}

// handleTaskSuspend accepts a suspend request: like delete, the API only
// flips the persisted phase (Suspending); reconcile does the freeze on its
// next tick. Running is the only phase that starts a freeze — the already
// suspended phases answer idempotently, and everything else (nothing to
// freeze, a freeze already in flight the other way, or finished) is refused
// rather than guessed about.
func (s *Server) handleTaskSuspend(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	t, err := s.store.GetTask(name)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if t.Status.DeletionTimestamp != nil {
		httpError(w, http.StatusConflict, "task %s is being deleted", name)
		return
	}
	switch t.Status.Phase {
	case v1alpha1.TaskRunning:
		if err := s.ctl.RequestSuspend(name, t.Status.Phase); err != nil {
			if errors.Is(err, store.ErrPhaseConflict) {
				// Reconcile moved the task (typically to a terminal phase)
				// between the read above and the mark; the caller's decision
				// was made on stale state, so surface it instead of pretending
				// the suspend was recorded.
				httpError(w, http.StatusConflict, "task %s changed state before the suspend was recorded — retry", name)
				return
			}
			httpError(w, http.StatusInternalServerError, "%v", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "suspending"})
	case v1alpha1.TaskSuspending, v1alpha1.TaskSuspended:
		// Already freezing or frozen; a repeat suspend is a no-op.
		writeJSON(w, http.StatusOK, map[string]string{"status": string(t.Status.Phase)})
	default:
		httpError(w, http.StatusConflict, "task %s is in phase %s — only a running task can suspend", name, t.Status.Phase)
	}
}

// handleTaskResume accepts a resume request: Suspended flips to Resuming and
// reconcile thaws on its next tick. A running task (or one already resuming)
// answers idempotently — resume is a convergence request, not a toggle —
// while Suspending is refused: a freeze is in flight and thawing under it
// would race the reconciler.
func (s *Server) handleTaskResume(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	t, err := s.store.GetTask(name)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if t.Status.DeletionTimestamp != nil {
		httpError(w, http.StatusConflict, "task %s is being deleted", name)
		return
	}
	switch t.Status.Phase {
	case v1alpha1.TaskSuspended:
		if err := s.ctl.RequestResume(name, t.Status.Phase); err != nil {
			if errors.Is(err, store.ErrPhaseConflict) {
				httpError(w, http.StatusConflict, "task %s changed state before the resume was recorded — retry", name)
				return
			}
			httpError(w, http.StatusInternalServerError, "%v", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "resuming"})
	case v1alpha1.TaskRunning, v1alpha1.TaskResuming:
		// Already thawed or thawing; a repeat resume is a no-op.
		writeJSON(w, http.StatusOK, map[string]string{"status": string(t.Status.Phase)})
	default:
		httpError(w, http.StatusConflict, "task %s is in phase %s — only a suspended task can resume", name, t.Status.Phase)
	}
}

func (s *Server) handleTaskLogs(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.GetTask(r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if t.Status.Container == 0 {
		http.Error(w, "(container gone; task already cleaned up)\n", http.StatusOK)
		return
	}
	switch t.Status.Phase {
	case v1alpha1.TaskSuspending, v1alpha1.TaskSuspended, v1alpha1.TaskResuming:
		// Logs ride pct exec, which blocks inside a frozen cgroup until
		// thaw — same reason exec refuses these phases. In Suspending and
		// Resuming the container may or may not still be frozen, so refuse
		// all three rather than hang until the exec timeout.
		httpError(w, http.StatusConflict, "task %s is %s — logs are unavailable while suspended (resume first)", t.Metadata.Name, t.Status.Phase)
		return
	}
	logs, err := s.prov.Logs(r.Context(), t.Status.Node, t.Status.Container)
	if err != nil {
		httpError(w, http.StatusBadGateway, "fetch logs: %v", err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(logs))
}

// exec request caps, in the apply style: enough for any debug command line,
// small enough that a hostile body cannot inflate the SSH command line past
// what sshd's limits allow.
const (
	execMaxArgs     = 16
	execMaxArgBytes = 4 << 10
	execMaxCmdBytes = 32 << 10
)

// execReady reports whether the task state names a live sandbox exec can use.
// An empty node means the record predates multi-node and reconcile has not
// repaired it yet — dispatch would need a node it cannot name. Checked twice
// per request: once on entry, once just before dispatch — the body read is a
// window in which the task can be deleted.
func execReady(t *v1alpha1.Task) bool {
	return t.Status.Phase == v1alpha1.TaskRunning && t.Status.Container != 0 && t.Status.Node != "" && t.Status.DeletionTimestamp == nil
}

func (s *Server) handleTaskExec(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	t, err := s.store.GetTask(name)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if !execReady(t) {
		httpError(w, http.StatusConflict, "task %s is not running (phase %s) — exec needs a live sandbox", t.Metadata.Name, t.Status.Phase)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		httpError(w, http.StatusBadRequest, "read body: %v", err)
		return
	}
	if len(body) > maxBodyBytes {
		httpError(w, http.StatusRequestEntityTooLarge, "body exceeds %d bytes", maxBodyBytes)
		return
	}
	var req struct {
		Command []string `json:"command"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "parse body: %v", err)
		return
	}
	// Only one JSON value counts as well-formed here: trailing garbage after
	// the object is a caller bug, not an ignored suffix.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		httpError(w, http.StatusBadRequest, "body must be a single JSON object")
		return
	}
	if len(req.Command) == 0 {
		httpError(w, http.StatusBadRequest, "command must be a non-empty list of arguments")
		return
	}
	if len(req.Command) > execMaxArgs {
		httpError(w, http.StatusBadRequest, "command has %d arguments, at most %d", len(req.Command), execMaxArgs)
		return
	}
	total := 0
	for i, a := range req.Command {
		// Empty elements are legal argv ("printf '%s\n' ''"); a NUL byte is
		// not — sshd hands the command to the shell as a C string, so it
		// would truncate the request mid-line.
		if strings.ContainsRune(a, '\x00') {
			httpError(w, http.StatusBadRequest, "command argument %d contains a NUL byte", i)
			return
		}
		if len(a) > execMaxArgBytes {
			httpError(w, http.StatusBadRequest, "command argument %d exceeds %d bytes", i, execMaxArgBytes)
			return
		}
		total += len(a)
	}
	if total > execMaxCmdBytes {
		httpError(w, http.StatusBadRequest, "command totals %d bytes, at most %d", total, execMaxCmdBytes)
		return
	}
	// Re-read the task and verify the container still belongs to it right
	// before dispatch: a delete (and PVE's later reuse of the CTID) during
	// the body read must not send exec into someone else's sandbox. This
	// narrows the race to milliseconds, same discipline as DestroyOwned —
	// full serialization with deletion is not attempted (threat model).
	t, err = s.store.GetTask(name)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusConflict, "task %s was deleted while the request was in flight", name)
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if !execReady(t) {
		httpError(w, http.StatusConflict, "task %s is not running (phase %s) — exec needs a live sandbox", t.Metadata.Name, t.Status.Phase)
		return
	}
	owned, err := s.prov.Owned(r.Context(), name, t.Status.Node, t.Status.Container)
	if err != nil {
		httpError(w, http.StatusBadGateway, "check ownership of ct %d: %v", t.Status.Container, err)
		return
	}
	if !owned {
		httpError(w, http.StatusConflict, "container %d no longer belongs to task %s — refusing to exec", t.Status.Container, name)
		return
	}
	res, err := s.prov.Exec(r.Context(), t.Status.Node, t.Status.Container, req.Command)
	if err != nil {
		httpError(w, http.StatusBadGateway, "exec: %v", err)
		return
	}
	out := map[string]any{
		"stdout":    res.Stdout,
		"stderr":    res.Stderr,
		"exitCode":  res.ExitCode,
		"truncated": res.Truncated,
	}
	if res.PctFailed {
		out["pctFailed"] = true
		out["pctReason"] = res.PctReason
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListWorkspaces(w http.ResponseWriter, _ *http.Request) {
	wss, err := s.store.ListWorkspaces()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if wss == nil {
		wss = []*v1alpha1.Workspace{}
	}
	writeJSON(w, http.StatusOK, wss)
}

func (s *Server) handleGetWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, err := s.store.GetWorkspace(r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "workspace not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

// Deleting a Workspace is immediate (declarative config, no status): tasks
// already provisioned keep their cloned copy, and only future applies that
// reference the name fail to resolve at provision time.
func (s *Server) handleDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	err := s.store.DeleteWorkspace(r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "workspace not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleListModels(w http.ResponseWriter, _ *http.Request) {
	models, err := s.store.ListModels()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	out := make([]*v1alpha1.Model, 0, len(models))
	for _, m := range models {
		out = append(out, redacted(m))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetModel(w http.ResponseWriter, r *http.Request) {
	m, err := s.store.GetModel(r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "model not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, redacted(m))
}

func (s *Server) handleDeleteModel(w http.ResponseWriter, r *http.Request) {
	err := s.store.DeleteModel(r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "model not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleListGateways(w http.ResponseWriter, _ *http.Request) {
	gws, err := s.store.ListGateways()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if gws == nil {
		gws = []*v1alpha1.Gateway{}
	}
	writeJSON(w, http.StatusOK, gws)
}

func (s *Server) handleGetGateway(w http.ResponseWriter, r *http.Request) {
	g, err := s.store.GetGateway(r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "gateway not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (s *Server) handleDeleteGateway(w http.ResponseWriter, r *http.Request) {
	err := s.store.DeleteGateway(r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "gateway not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// redacted returns a copy of the model with the API key masked: the key is
// write-only — it was accepted at apply time and must never leave the
// server again, so even an authenticated GET only sees the placeholder.
func redacted(m *v1alpha1.Model) *v1alpha1.Model {
	cp := *m
	cp.Spec = m.Spec
	cp.Spec.APIKey = v1alpha1.RedactedAPIKey
	return &cp
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false) // keep "<redacted>" readable instead of <
	_ = enc.Encode(v)
}

func httpError(w http.ResponseWriter, code int, format string, args ...any) {
	writeJSON(w, code, map[string]string{"error": fmt.Sprintf(format, args...)})
}
