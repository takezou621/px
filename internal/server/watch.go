package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/kawai/px/internal/apis/v1alpha1"
)

const watchInterval = time.Second

// snapshot is one frame of the watch stream: the full object lists, so the
// client can diff phases without server-side cursor state.
type snapshot struct {
	Tasks      []*v1alpha1.Task      `json:"tasks"`
	Workspaces []*v1alpha1.Workspace `json:"workspaces"`
}

// handleWatch streams NDJSON snapshots: the first immediately on connect,
// then one per watchInterval until the client disconnects. Plain chunked
// HTTP — no SSE/WebSocket dependency.
func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	fill := func(snap *snapshot) bool {
		tasks, err := s.store.ListTasks()
		if err != nil {
			return false
		}
		snap.Tasks = tasks
		wss, err := s.store.ListWorkspaces()
		if err != nil {
			return false
		}
		snap.Workspaces = wss
		return true
	}

	// Load the first snapshot before committing the 200, so an unreadable
	// store surfaces as a 500 instead of an empty stream a client would read
	// as "no tasks".
	var snap snapshot
	if !fill(&snap) {
		httpError(w, http.StatusInternalServerError, "list objects failed")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	if enc.Encode(snap) != nil {
		return
	}
	flusher.Flush()

	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			var next snapshot
			if !fill(&next) || enc.Encode(next) != nil {
				return
			}
			flusher.Flush()
		}
	}
}
