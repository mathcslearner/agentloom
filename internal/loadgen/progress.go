package loadgen

import (
	"fmt"
	"io"

	"github.com/mathcslearner/agentloom/internal/api"
)

// printProgress writes one live progress line to w. phase names the current
// campaign phase; qs is the latest queue snapshot (may be the zero value if the
// stats endpoint is unavailable).
func printProgress(w io.Writer, phase string, elapsed float64, s snapshot, qs api.SystemStatsResponse, fh *firehoseClient) {
	ready, pending, delayed, outbox := int64(-1), int64(-1), int64(-1), qs.Outbox.Backlog
	if qs.Queue != nil {
		ready, pending, delayed = qs.Queue.ReadyDepth, qs.Queue.Pending, qs.Queue.Delayed
	}
	wsState := "-"
	if fh != nil {
		if fh.connected.Load() {
			wsState = fmt.Sprintf("ws✓ f=%d r=%d", fh.frames.Load(), fh.reconnects.Load())
		} else {
			wsState = fmt.Sprintf("ws✗ r=%d", fh.reconnects.Load())
		}
	}
	fmt.Fprintf(w, //nolint:errcheck // stdio write
		"[%6.0fs] %-8s sub=%d acc=%d active=%d term=%d(ok=%d fail=%d) rej=%d | e2e p50=%.0fms p99=%.0fms sched p50=%.0fms p99=%.0fms | q ready=%d pel=%d dly=%d obx=%d | %s\n",
		elapsed, phase, s.Total, s.Accepted, s.Active, s.Terminal, s.Succeeded, s.Failed, s.Rejected,
		s.E2EP50ms, s.E2EP99ms, s.SchedP50ms, s.SchedP99ms,
		ready, pending, delayed, outbox, wsState)
}
