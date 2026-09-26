// Handlers for the observability endpoints: task events, the recent-events
// feed, and the /v1/metrics Prometheus text. The text is rendered by hand —
// no client library rides in the single binary.
package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/kawai/px/internal/apis/v1alpha1"
	"github.com/kawai/px/internal/store"
)

// eventsMaxLimit bounds GET /v1/events' page size: big enough to dump a
// healthy server's whole history in one request, small enough that a
// hostile limit cannot turn one read into a multi-second scan.
const eventsMaxLimit = 1000

func (s *Server) handleTaskEvents(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	// A real task with no events yet serializes as []; a mistyped name must
	// not silently read as an empty history.
	if _, err := s.store.GetTask(name); errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "task not found")
		return
	} else if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	evs, err := s.store.ListTaskEvents(name)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if evs == nil {
		evs = []*v1alpha1.Event{}
	}
	writeJSON(w, http.StatusOK, evs)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	limit := 500
	q := r.URL.Query()
	// Has, not Get: "?limit=" (empty) falls back to the default rather than
	// reading as a parse error.
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > eventsMaxLimit {
			httpError(w, http.StatusBadRequest, "limit must be an integer in 1..%d", eventsMaxLimit)
			return
		}
		limit = n
	}
	evs, err := s.store.ListEvents(limit)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if evs == nil {
		evs = []*v1alpha1.Event{}
	}
	writeJSON(w, http.StatusOK, evs)
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	text, err := s.metricsText()
	if err != nil {
		// A scrape that silently reports zeros reads as "cluster is idle"
		// downstream; fail loudly instead.
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(text))
}

// taskPhases enumerates every phase px_tasks reports, so a scrape always
// sees the full label set (zero-filled) no matter what the fleet is doing.
var taskPhases = []v1alpha1.TaskPhase{
	v1alpha1.TaskPending,
	v1alpha1.TaskProvisioning,
	v1alpha1.TaskRunning,
	v1alpha1.TaskSuspending,
	v1alpha1.TaskSuspended,
	v1alpha1.TaskResuming,
	v1alpha1.TaskSucceeded,
	v1alpha1.TaskFailed,
	v1alpha1.TaskProvisionFail,
}

// metricsText renders the scrape. Store read failures abort the scrape with
// an error instead of publishing a zero-filled snapshot — a fake zero looks
// exactly like an idle cluster to everything consuming the text.
func (s *Server) metricsText() (string, error) {
	var b strings.Builder

	// Tasks by phase. Names stay out of the metrics: labels on task
	// identity would turn a metrics endpoint into a per-task observability
	// side channel, and /v1/tasks already serves that.
	tasks, err := s.store.ListTasks()
	if err != nil {
		return "", err
	}
	counts := make(map[v1alpha1.TaskPhase]int, len(taskPhases))
	for _, t := range tasks {
		counts[t.Status.Phase]++
	}
	b.WriteString("# HELP px_tasks Tasks by phase.\n")
	b.WriteString("# TYPE px_tasks gauge\n")
	for _, p := range taskPhases {
		fmt.Fprintf(&b, "px_tasks{phase=%q} %d\n", string(p), counts[p])
	}

	// Tick timing as a quantile-less summary: rate(_sum)/rate(_count) then
	// computes mean tick cost per scrape interval, which is all a fixed-
	// interval reconcile loop has to offer.
	var tickSum, tickCount, provFails int64
	if s.Metrics != nil {
		tickSum = s.Metrics.TickNanos.Load()
		tickCount = s.Metrics.TickCount.Load()
		provFails = s.Metrics.ProvisionFails.Load()
	}
	b.WriteString("# HELP px_reconcile_tick_seconds Total reconcile pass wall time.\n")
	// summary, not counter: the _sum/_count sample suffixes make this a
	// summary family in the exposition format; declaring it counter leaves
	// scrapers parsing a counter with zero samples plus two stray series.
	b.WriteString("# TYPE px_reconcile_tick_seconds summary\n")
	// The accumulator is nanoseconds; the metric name promises seconds.
	fmt.Fprintf(&b, "px_reconcile_tick_seconds_sum %g\n", float64(tickSum)/1e9)
	fmt.Fprintf(&b, "px_reconcile_tick_seconds_count %d\n", tickCount)
	b.WriteString("# HELP px_provision_failures_total Provision failures since px-server start.\n")
	b.WriteString("# TYPE px_provision_failures_total counter\n")
	fmt.Fprintf(&b, "px_provision_failures_total %d\n", provFails)

	n, err := s.store.CountEvents()
	if err != nil {
		return "", err
	}
	b.WriteString("# HELP px_events Stored task events.\n")
	b.WriteString("# TYPE px_events gauge\n")
	fmt.Fprintf(&b, "px_events %d\n", n)

	nb, err := s.store.SessionBytesTotal()
	if err != nil {
		return "", err
	}
	b.WriteString("# HELP px_sessions_bytes Bytes of captured agent session archives.\n")
	b.WriteString("# TYPE px_sessions_bytes gauge\n")
	fmt.Fprintf(&b, "px_sessions_bytes %d\n", nb)

	// Disk sizes can't fail meaningfully (stat on optional siblings), so
	// this stays best-effort.
	if n, ok := storeBytes(s.DBPath); ok {
		b.WriteString("# HELP px_store_bytes Size of the SQLite store on disk (db + wal + shm).\n")
		b.WriteString("# TYPE px_store_bytes gauge\n")
		fmt.Fprintf(&b, "px_store_bytes %d\n", n)
	}
	return b.String(), nil
}

// storeBytes sums the database file and its WAL/SHM siblings; any missing
// sibling just contributes zero. An empty path — or one with no database
// file at all — omits the gauge rather than publishing a zero.
func storeBytes(path string) (int64, bool) {
	if path == "" {
		return 0, false
	}
	var total int64
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(f)
		if err != nil {
			continue
		}
		total += fi.Size()
	}
	return total, total > 0
}
