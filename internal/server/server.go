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
			if err := s.store.UpsertWorkspace(ws); err != nil {
				httpError(w, http.StatusInternalServerError, "upsert workspace %s: %v", m.Metadata.Name, err)
				return
			}
			results = append(results, fmt.Sprintf("workspace.px.io/%s configured", m.Metadata.Name))
		case v1alpha1.KindTask:
			t := &v1alpha1.Task{
				APIVersion: v1alpha1.APIVersion,
				Kind:       v1alpha1.KindTask,
				Metadata:   m.Metadata,
				Spec:       *m.Task,
			}
			t.Status.Phase = v1alpha1.TaskPending
			t.Status.Reason = "queued"
			if err := s.store.CreateTask(t); err != nil {
				if errors.Is(err, store.ErrExists) {
					httpError(w, http.StatusConflict, "task %q already exists; delete it first", m.Metadata.Name)
					return
				}
				httpError(w, http.StatusInternalServerError, "create task %s: %v", m.Metadata.Name, err)
				return
			}
			results = append(results, fmt.Sprintf("task.px.io/%s created", m.Metadata.Name))
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"results": results})
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
	name := r.PathValue("name")
	if _, err := s.store.GetTask(name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpError(w, http.StatusNotFound, "task not found")
			return
		}
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	s.ctl.RequestDestroy(name)
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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func httpError(w http.ResponseWriter, code int, format string, args ...any) {
	writeJSON(w, code, map[string]string{"error": fmt.Sprintf(format, args...)})
}
