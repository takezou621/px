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
	mux.HandleFunc("GET /v1/workspaces", s.handleListWorkspaces)
	mux.HandleFunc("GET /v1/workspaces/{name}", s.handleGetWorkspace)
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
	logs, err := s.prov.Logs(r.Context(), t.Status.Container)
	if err != nil {
		httpError(w, http.StatusBadGateway, "fetch logs: %v", err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(logs))
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
