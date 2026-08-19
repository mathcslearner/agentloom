package loadgen

import (
	"context"
	"errors"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/event"
)

// Failure/outcome taxonomy classes (report §"Failure taxonomy in the report").
const (
	classRunSucceeded  = "run_succeeded"
	classRunFailed     = "run_failed"
	classRunCancelled  = "run_cancelled"
	classRunTimeout    = "run_timeout"
	classRunLost       = "run_lost"
	classSubmit4xx     = "submit_http_4xx"
	classSubmit5xx     = "submit_http_5xx"
	classSubmitErr     = "submit_transport_error"
	classSubmitTimeout = "submit_timeout"
	classInflightCap   = "skipped_inflight_cap"
)

// runState is one submission's tracked lifecycle. Fields are guarded by the
// tracker's mutex.
type runState struct {
	idx         int
	component   string
	intended    time.Time // scheduled fire time (campaign clock)
	inSteady    bool      // intended falls in the steady window
	submittedAt time.Time // client-side submit completion
	runID       string

	// Submission outcome.
	submitStatus int
	submitCode   string
	submitClass  string  // non-empty only for a rejected/skipped submit
	submitMs     float64 // client RTT

	// Terminal outcome.
	terminal    bool
	status      string    // last observed run status
	terminalSrv time.Time // server-side terminal timestamp (finished_at / event ts)
	e2eMs       float64   // corrected end-to-end latency
	stepsTotal  int
	stepsFailed int
	dlqCount    int

	// Scheduling-latency sampling (firehose step events), sampled runs only.
	schedSampled bool
	readyAt      map[string]time.Time
}

// tracker holds the campaign's run states and latency histograms. All mutation
// funnels through its methods under one mutex; the firehose reader, the submit
// goroutines, the poll pool, and the reconciler all call it.
type tracker struct {
	mu     sync.Mutex
	byID   map[string]*runState
	byIdx  map[int]*runState
	order  []*runState
	skew   time.Duration // serverClock - clientClock (subtracted from server ts)
	sample float64

	submitRTT  *Histogram // client submit round-trip
	submitCorr *Histogram // submit from intended time (coordinated-omission view)
	e2e        *Histogram // end-to-end (steady window only)
	sched      *Histogram // ready→running (sampled)

	// Aggregate counters (steady-window submissions unless noted).
	submitted int64
	accepted  int64
}

func newTracker(sample float64, skew time.Duration) *tracker {
	return &tracker{
		byID:       map[string]*runState{},
		byIdx:      map[int]*runState{},
		skew:       skew,
		sample:     sample,
		submitRTT:  NewHistogram(1, 0.01),
		submitCorr: NewHistogram(1, 0.01),
		e2e:        NewHistogram(1, 0.01),
		sched:      NewHistogram(1, 0.01),
	}
}

// registerFire records an intended submission before it fires, so an inflight
// cap skip or a submit error still has a tracked state (nothing is silently
// omitted). inSteady marks whether the intended time is in the steady window.
func (t *tracker) registerFire(idx int, component string, intended time.Time, inSteady bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rs := &runState{idx: idx, component: component, intended: intended, inSteady: inSteady}
	t.byIdx[idx] = rs
	t.order = append(t.order, rs)
}

// recordSkip marks a fire that never left because the inflight cap was full.
func (t *tracker) recordSkip(idx int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if rs := t.byIdx[idx]; rs != nil {
		rs.submitClass = classInflightCap
	}
}

// recordSubmit files a submission's outcome. now is the campaign clock at
// submit completion (for the from-intended histogram).
func (t *tracker) recordSubmit(idx int, res submitResult, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rs := t.byIdx[idx]
	if rs == nil {
		return
	}
	rs.submittedAt = now
	rs.submitMs = float64(res.RTT.Microseconds()) / 1000
	rs.submitStatus = res.Status
	rs.submitCode = res.Code
	if rs.inSteady {
		t.submitRTT.RecordDuration(res.RTT)
		// From-intended latency: the corrected view that a slow submitter
		// (behind the schedule) does not hide as low RTT.
		t.submitCorr.RecordDuration(now.Sub(rs.intended))
		t.submitted++
	}
	switch {
	case res.Err != nil:
		if isTimeout(res.Err) {
			rs.submitClass = classSubmitTimeout
		} else {
			rs.submitClass = classSubmitErr
		}
	case res.Status >= 500:
		rs.submitClass = classSubmit5xx
	case res.Status >= 400:
		rs.submitClass = classSubmit4xx
	case res.RunID != "":
		rs.runID = res.RunID
		t.byID[res.RunID] = rs
		if rs.inSteady {
			t.accepted++
		}
		if t.sample > 0 && sampleHash(res.RunID) < t.sample {
			rs.schedSampled = true
			rs.readyAt = map[string]time.Time{}
		}
	default:
		rs.submitClass = classSubmitErr // 2xx without a run id: treat as an error
	}
}

// feedEvent is loadgen's lightweight decode of a firehose event frame — only
// the fields the tracker needs, so it never has to decode the typed payload.
type feedEvent struct {
	RunID  string
	Seq    int64
	Type   event.Type
	StepID string
	Ts     time.Time
}

// applyEvent folds one firehose event into the tracked run. Unknown runs (not
// submitted by this campaign) are ignored. Returns true if it advanced the run
// to terminal (for progress accounting).
func (t *tracker) applyEvent(ev feedEvent) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	rs := t.byID[ev.RunID]
	if rs == nil {
		return false
	}
	switch ev.Type {
	case event.TypeStepReady:
		if rs.schedSampled && ev.StepID != "" {
			if _, ok := rs.readyAt[ev.StepID]; !ok {
				rs.readyAt[ev.StepID] = ev.Ts
			}
		}
	case event.TypeStepClaimed:
		if rs.schedSampled && ev.StepID != "" {
			if r, ok := rs.readyAt[ev.StepID]; ok {
				if d := ev.Ts.Sub(r); d >= 0 {
					t.sched.RecordDuration(d)
				}
				delete(rs.readyAt, ev.StepID) // a retry's claim has no ready pair
			}
		}
	case event.TypeRunSucceeded:
		return t.markTerminalLocked(rs, "succeeded", ev.Ts)
	case event.TypeRunFailed:
		return t.markTerminalLocked(rs, "failed", ev.Ts)
	case event.TypeRunCancelled:
		return t.markTerminalLocked(rs, "cancelled", ev.Ts)
	}
	return false
}

// applyRunBody folds a polled/reconciled run body into the tracked run,
// marking it terminal when its status is terminal.
func (t *tracker) applyRunBody(body api.RunResponse) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rs := t.byID[body.Run.ID]
	if rs == nil {
		return
	}
	rs.stepsTotal = body.Run.StepsTotal
	rs.stepsFailed = body.Run.StepsFailed
	rs.dlqCount = len(body.DeadLetters)
	if isTerminalStatus(body.Run.Status) {
		srv := body.Run.CreatedAt
		if body.Run.FinishedAt != nil {
			srv = *body.Run.FinishedAt
		}
		t.markTerminalLocked(rs, body.Run.Status, srv)
		return
	}
	rs.status = body.Run.Status
}

// markTerminalLocked records a terminal transition once (first writer wins),
// computing the skew-corrected end-to-end latency. Caller holds the mutex.
func (t *tracker) markTerminalLocked(rs *runState, status string, serverTs time.Time) bool {
	if rs.terminal {
		return false
	}
	rs.terminal = true
	rs.status = status
	rs.terminalSrv = serverTs
	if !rs.submittedAt.IsZero() && !serverTs.IsZero() {
		// Correct server ts into the client frame, then subtract submit time.
		e2e := serverTs.Add(-t.skew).Sub(rs.submittedAt)
		if e2e < 0 {
			e2e = 0
		}
		rs.e2eMs = float64(e2e.Microseconds()) / 1000
		if rs.inSteady {
			t.e2e.RecordDuration(e2e)
		}
	}
	return true
}

// markLostAndTimeout finalizes any still-open runs after the drain deadline:
// an accepted run with no terminal status is a timeout; a run whose id we hold
// but that reconciliation could not find at all is lost. Returns the counts.
func (t *tracker) finalizeOpen() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, rs := range t.order {
		if rs.submitClass != "" || rs.terminal {
			continue
		}
		if rs.runID == "" {
			continue // never accepted; already classed at submit
		}
		rs.submitClass = classRunTimeout // accepted but never terminal by deadline
	}
}

// markLost flags an accepted run that reconciliation could not find as lost.
func (t *tracker) markLost(runID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if rs := t.byID[runID]; rs != nil && !rs.terminal {
		rs.submitClass = classRunTimeout // will be re-classed lost by classOf if unseen
		rs.status = "lost"
	}
}

// activeRunIDs returns the accepted-but-not-yet-terminal run ids whose grace
// period (submittedAt + pollAfter) has passed — the poll pool's work list.
func (t *tracker) overdueForPoll(pollAfter time.Duration, now time.Time) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var ids []string
	for _, rs := range t.order {
		if rs.runID == "" || rs.terminal || rs.submitClass != "" {
			continue
		}
		if now.Sub(rs.submittedAt) >= pollAfter {
			ids = append(ids, rs.runID)
		}
	}
	return ids
}

// classOf returns a run's final taxonomy class.
func classOf(rs *runState) string {
	switch {
	case rs.submitClass == classInflightCap, rs.submitClass == classSubmit4xx,
		rs.submitClass == classSubmit5xx, rs.submitClass == classSubmitErr,
		rs.submitClass == classSubmitTimeout:
		return rs.submitClass
	case rs.status == "lost":
		return classRunLost
	case rs.terminal:
		switch rs.status {
		case "succeeded":
			return classRunSucceeded
		case "cancelled":
			return classRunCancelled
		default:
			return classRunFailed
		}
	case rs.submitClass == classRunTimeout:
		return classRunTimeout
	case rs.runID != "":
		return classRunTimeout // accepted, never terminal, no explicit class
	default:
		return classSubmitErr
	}
}

// snapshot is a point-in-time view for the progress line.
type snapshot struct {
	Total      int
	Accepted   int
	Active     int
	Terminal   int
	Succeeded  int
	Failed     int
	Rejected   int
	E2EP50ms   float64
	E2EP99ms   float64
	SchedP50ms float64
	SchedP99ms float64
}

func (t *tracker) snapshot() snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	var s snapshot
	s.Total = len(t.order)
	for _, rs := range t.order {
		if rs.runID != "" {
			s.Accepted++
		}
		switch {
		case rs.terminal:
			s.Terminal++
			if rs.status == "succeeded" {
				s.Succeeded++
			} else {
				s.Failed++
			}
		case rs.runID != "" && rs.submitClass == "":
			s.Active++
		case rs.submitClass != "" && rs.submitClass != classRunTimeout:
			s.Rejected++
		}
	}
	s.E2EP50ms = usToMs(t.e2e.ValueAtQuantile(0.5))
	s.E2EP99ms = usToMs(t.e2e.ValueAtQuantile(0.99))
	s.SchedP50ms = usToMs(t.sched.ValueAtQuantile(0.5))
	s.SchedP99ms = usToMs(t.sched.ValueAtQuantile(0.99))
	return s
}

// taxonomy tallies every run's final class, plus up to sampleN example run ids
// per non-success class.
func (t *tracker) taxonomy(sampleN int) map[string]*classTally {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]*classTally{}
	for _, rs := range t.order {
		c := classOf(rs)
		tal := out[c]
		if tal == nil {
			tal = &classTally{}
			out[c] = tal
		}
		tal.Count++
		if c != classRunSucceeded && len(tal.Examples) < sampleN {
			ex := rs.runID
			if ex == "" {
				ex = rs.submitCode
			}
			if ex != "" {
				tal.Examples = append(tal.Examples, ex)
			}
		}
	}
	return out
}

type classTally struct {
	Count    int      `json:"count"`
	Examples []string `json:"examples,omitempty"`
}

// runRows returns per-run report rows in submission order.
func (t *tracker) runRows() []runState {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]runState, 0, len(t.order))
	sorted := make([]*runState, len(t.order))
	copy(sorted, t.order)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].idx < sorted[j].idx })
	for _, rs := range sorted {
		out = append(out, *rs)
	}
	return out
}

func usToMs(us int64) float64 { return float64(us) / 1000 }

func isTerminalStatus(s string) bool {
	return s == "succeeded" || s == "failed" || s == "cancelled"
}

// sampleHash maps a run id deterministically into [0,1) for sched sampling.
func sampleHash(runID string) float64 {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(runID); i++ {
		h ^= uint64(runID[i])
		h *= 1099511628211
	}
	return float64(h%10000) / 10000
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var te interface{ Timeout() bool }
	if errors.As(err, &te) {
		return te.Timeout()
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return false
}

// acceptedRunIDs returns every run id we hold (submission accepted).
func (t *tracker) acceptedRunIDs() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	ids := make([]string, 0, len(t.byID))
	for id := range t.byID {
		ids = append(ids, id)
	}
	return ids
}

// isOpen reports whether an accepted run is not yet terminal.
func (t *tracker) isOpen(runID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	rs := t.byID[runID]
	return rs != nil && !rs.terminal
}
