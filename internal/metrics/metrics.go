package metrics

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

// Counters are process-wide, following the same "initialise once, call via
// package functions" rule as the logger.
var (
	pollCycles     atomic.Int64
	mediaDelivered atomic.Int64
	errorsTotal    atomic.Int64
)

// IncPollCycle records a completed poll cycle.
func IncPollCycle() { pollCycles.Add(1) }

// IncMediaDelivered records a media item successfully sent to Telegram.
func IncMediaDelivered() { mediaDelivered.Add(1) }

// IncErrors records an error-level event.
func IncErrors() { errorsTotal.Add(1) }

// Handler renders the counters in Prometheus text exposition format.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/plain; version=0.0.4")

		fmt.Fprintf(w, "# TYPE instalker_poll_cycles_total counter\ninstalker_poll_cycles_total %d\n", pollCycles.Load())
		fmt.Fprintf(w, "# TYPE instalker_media_delivered_total counter\ninstalker_media_delivered_total %d\n", mediaDelivered.Load())
		fmt.Fprintf(w, "# TYPE instalker_errors_total counter\ninstalker_errors_total %d\n", errorsTotal.Load())
	})
}
