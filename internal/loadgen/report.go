package loadgen

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// Report is the campaign's summary artifact (summary.json). Every published
// number is here or in the companion CSVs; a downstream tool (19.3/19.6) reads
// this shape.
type Report struct {
	SchemaVersion    int                    `json:"schema_version"`
	GeneratedAt      time.Time              `json:"generated_at"`
	Version          string                 `json:"agentloom_version"`
	Host             hostView               `json:"host"`
	Scenario         json.RawMessage        `json:"scenario"`
	Config           configView             `json:"config"`
	Windows          windowsView            `json:"windows"`
	Definitions      map[string]string      `json:"definitions"`
	ClockSkewMs      float64                `json:"clock_skew_ms"`
	Rate             rateView               `json:"rate"`
	Counts           map[string]int         `json:"counts"`
	Taxonomy         map[string]*classTally `json:"taxonomy"`
	Latency          map[string]Percentiles `json:"latency"`
	ThroughputPerSec float64                `json:"throughput_terminal_per_sec"`
	ActiveMax        int                    `json:"active_runs_max"`
	Quiescence       quiesceView            `json:"quiescence"`
	SLO              *sloView               `json:"slo,omitempty"`
	Integrity        integrityView          `json:"integrity"`
}

type hostView struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
	CPUs int    `json:"cpus"`
}

type configView struct {
	APIBase     string  `json:"api_base"`
	Scenario    string  `json:"scenario"`
	Track       string  `json:"track"`
	SchedSample float64 `json:"sched_sample"`
	Inline      bool    `json:"inline"`
	MaxRuns     int     `json:"max_runs"`
	MaxInflight int     `json:"max_inflight"`
	Seed        int64   `json:"seed"`
}

type windowsView struct {
	CampaignStart time.Time `json:"campaign_start"`
	SteadyStart   time.Time `json:"steady_start"`
	SteadyEnd     time.Time `json:"steady_end"`
	ArrivalsEnd   time.Time `json:"arrivals_end"`
	WarmupSec     float64   `json:"warmup_sec"`
	SteadySec     float64   `json:"steady_sec"`
}

// rateView reports offered vs achieved arrival rate over the steady window —
// the coordinated-omission accuracy check.
type rateView struct {
	SteadyIntended  int     `json:"steady_intended"`
	SteadySubmitted int     `json:"steady_submitted"`
	SteadyAccepted  int     `json:"steady_accepted"`
	OfferedPerSec   float64 `json:"offered_per_sec"`
	AchievedPerSec  float64 `json:"achieved_per_sec"`
	RateErrorPct    float64 `json:"rate_error_pct"`
	PacerLagP99Ms   float64 `json:"pacer_lag_p99_ms"`
	PacerLagMaxMs   float64 `json:"pacer_lag_max_ms"`
}

type quiesceView struct {
	Reached       bool    `json:"reached"`
	ReadyDepth    int64   `json:"ready_depth"`
	Pending       int64   `json:"pending"`
	Delayed       int64   `json:"delayed"`
	OutboxBacklog int64   `json:"outbox_backlog"`
	WaitedSec     float64 `json:"waited_sec"`
}

type sloView struct {
	SchedulingP50Pass *bool  `json:"scheduling_p50_pass,omitempty"`
	SchedulingP99Pass *bool  `json:"scheduling_p99_pass,omitempty"`
	APIP99Pass        *bool  `json:"api_p99_pass,omitempty"`
	EndToEndP99Pass   *bool  `json:"end_to_end_p99_pass,omitempty"`
	Detail            string `json:"detail"`
}

type integrityView struct {
	LostRuns         int   `json:"lost_runs"`
	NonDeliberateDLQ int   `json:"non_deliberate_dead_letters"`
	DLQOpenStart     int64 `json:"dlq_open_start"`
	DLQOpenEnd       int64 `json:"dlq_open_end"`
}

// tsSample is one per-second timeseries observation for timeseries.csv.
type tsSample struct {
	AtSec      float64
	Submitted  int
	Accepted   int
	Active     int
	Terminal   int
	ReadyDepth int64
	Pending    int64
	Delayed    int64
	Outbox     int64
}

// writeArtifacts writes summary.json, summary.md, runs.csv, timeseries.csv,
// and one hist-*.csv per latency histogram into dir (created if absent).
func writeArtifacts(dir string, rep Report, rows []runState, ts []tsSample, hists map[string]*Histogram) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating out dir: %w", err)
	}
	if err := writeJSONFile(filepath.Join(dir, "summary.json"), rep); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "summary.md"), []byte(renderMarkdown(rep)), 0o644); err != nil { //nolint:gosec
		return err
	}
	if err := writeRunsCSV(filepath.Join(dir, "runs.csv"), rows, rep.Windows.CampaignStart); err != nil {
		return err
	}
	if err := writeTimeseriesCSV(filepath.Join(dir, "timeseries.csv"), ts); err != nil {
		return err
	}
	names := make([]string, 0, len(hists))
	for n := range hists {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := writeHistCSV(filepath.Join(dir, "hist-"+n+".csv"), hists[n]); err != nil {
			return err
		}
	}
	return nil
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644) //nolint:gosec
}

func writeRunsCSV(path string, rows []runState, start time.Time) error {
	f, err := os.Create(path) //nolint:gosec
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	w := csv.NewWriter(f)
	_ = w.Write([]string{
		"idx", "component", "intended_offset_ms", "submitted_offset_ms", "run_id",
		"http_status", "class", "status", "submit_ms", "e2e_ms",
		"steps_total", "steps_failed", "dlq", "in_steady",
	})
	for _, rs := range rows {
		intendedOff := ""
		if !rs.intended.IsZero() {
			intendedOff = fmtMs(rs.intended.Sub(start))
		}
		submittedOff := ""
		if !rs.submittedAt.IsZero() {
			submittedOff = fmtMs(rs.submittedAt.Sub(start))
		}
		_ = w.Write([]string{
			strconv.Itoa(rs.idx), rs.component, intendedOff, submittedOff, rs.runID,
			strconv.Itoa(rs.submitStatus), classOf(&rs), rs.status,
			strconv.FormatFloat(rs.submitMs, 'f', 3, 64),
			strconv.FormatFloat(rs.e2eMs, 'f', 3, 64),
			strconv.Itoa(rs.stepsTotal), strconv.Itoa(rs.stepsFailed),
			strconv.Itoa(rs.dlqCount), strconv.FormatBool(rs.inSteady),
		})
	}
	w.Flush()
	return w.Error()
}

func writeTimeseriesCSV(path string, ts []tsSample) error {
	f, err := os.Create(path) //nolint:gosec
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	w := csv.NewWriter(f)
	_ = w.Write([]string{"at_sec", "submitted", "accepted", "active", "terminal", "ready_depth", "pending", "delayed", "outbox"})
	for _, s := range ts {
		_ = w.Write([]string{
			strconv.FormatFloat(s.AtSec, 'f', 1, 64),
			strconv.Itoa(s.Submitted), strconv.Itoa(s.Accepted), strconv.Itoa(s.Active), strconv.Itoa(s.Terminal),
			strconv.FormatInt(s.ReadyDepth, 10), strconv.FormatInt(s.Pending, 10),
			strconv.FormatInt(s.Delayed, 10), strconv.FormatInt(s.Outbox, 10),
		})
	}
	w.Flush()
	return w.Error()
}

func writeHistCSV(path string, h *Histogram) error {
	f, err := os.Create(path) //nolint:gosec
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	w := csv.NewWriter(f)
	_ = w.Write([]string{"quantile", "value_ms"})
	for _, r := range h.distribution() {
		_ = w.Write([]string{
			strconv.FormatFloat(r.Quantile, 'f', 4, 64),
			strconv.FormatFloat(usToMs(r.ValueUS), 'f', 3, 64),
		})
	}
	w.Flush()
	return w.Error()
}

func fmtMs(d time.Duration) string {
	return strconv.FormatFloat(float64(d.Microseconds())/1000, 'f', 1, 64)
}

// jsonMarshal is json.Marshal exposed under a package-local name so callers
// need not import encoding/json just for a scenario re-encode.
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
