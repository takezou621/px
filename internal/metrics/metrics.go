// Package metrics holds the px-server's own counters, rendered by the API's
// /v1/metrics endpoint. Prometheus text is written by hand at the server —
// no client library (single-binary axis).
package metrics

import (
	"sync/atomic"
	"time"
)

// Metrics carries the counters the controller feeds between ticks and the
// metrics endpoint reads concurrently. All methods are nil-receiver safe:
// a caller without metrics wired (tests, the CLI) must not crash.
type Metrics struct {
	// TickNanos sums every reconcile tick's wall time; TickCount counts
	// them. The pair renders as sum/count — mean tick = sum/count.
	TickNanos      atomic.Int64
	TickCount      atomic.Int64
	ProvisionFails atomic.Int64 // failed provisions since start (failProvision)
}

func (m *Metrics) ObserveTick(d time.Duration) {
	if m == nil {
		return
	}
	m.TickNanos.Add(int64(d))
	m.TickCount.Add(1)
}

func (m *Metrics) IncProvisionFailures() {
	if m == nil {
		return
	}
	m.ProvisionFails.Add(1)
}
