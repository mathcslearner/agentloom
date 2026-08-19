package loadgen

import (
	"fmt"
	"time"
)

// TrackMode is how the generator learns a run's terminal status.
type TrackMode string

const (
	// TrackFirehose subscribes to the /v1/events/ws feed for low-latency
	// terminal detection, with per-run polling as the backstop.
	TrackFirehose TrackMode = "firehose"
	// TrackPoll uses only per-run polling — no WebSocket connection. The
	// authoritative fallback, and what the httptest tests exercise.
	TrackPoll TrackMode = "poll"
)

// Config is the fully-resolved load-generator invocation. cmd/loadgen builds
// it from flags; Run consumes it. All overrides (Rate, Ramp, Duration, Warmup)
// are applied over the named scenario before the campaign starts.
type Config struct {
	// APIBase is the API root URL (e.g. http://localhost:8080).
	APIBase string
	// APIKey is the bearer credential (submit + read scopes needed).
	APIKey string

	// ScenarioDir holds the scenario corpus (test/load/scenarios).
	ScenarioDir string
	// Scenario is the name to run.
	Scenario string

	// Overrides (zero = use the scenario's value).
	RateOverride     float64
	RampOverride     string // "from:to:step:dur", empty = none
	DurationOverride time.Duration
	WarmupOverride   time.Duration

	// MaxRuns caps the total submissions (0 = unbounded; the --runs dry-run
	// knob). When set, the campaign ends when either MaxRuns fires or the
	// duration elapses, whichever comes first.
	MaxRuns int

	// Track selects the terminal-detection channel.
	Track TrackMode
	// SchedSample is the fraction of runs (0..1) whose step_ready→step_claimed
	// scheduling latency is sampled from the firehose. 0 disables step-event
	// subscription entirely (Prometheus stays the authoritative source).
	SchedSample float64

	// Inline submits the definition body on every run instead of a stored
	// definition_id (exercises the submission-path validation cost, H6).
	Inline bool

	// Timeouts and cadences.
	SubmitTimeout time.Duration // per POST /v1/runs
	RunTimeout    time.Duration // submit → terminal before a run is a timeout
	PollAfter     time.Duration // grace before an active run is polled
	PollInterval  time.Duration // per-run poll cadence once overdue
	DrainTimeout  time.Duration // max wait for quiescence after arrivals stop
	Progress      time.Duration // progress-line cadence
	MaxInflight   int           // cap on concurrent in-flight submits (0 = unbounded)
	PollWorkers   int           // bounded pool draining overdue polls

	// Seed drives the mix draw (reproducible composite campaigns).
	Seed int64

	// OutDir is where report artifacts are written (created if absent).
	OutDir string

	// FailOnLost makes a non-zero exit when any submitted run is lost
	// (accepted submit, never reached a terminal status). Default true.
	FailOnLost bool
}

// withDefaults fills unset cadence/timeout fields with sensible values and
// validates the essentials, returning a copy.
func (c Config) withDefaults() (Config, error) {
	if c.APIBase == "" {
		return c, fmt.Errorf("loadgen: api base URL is required")
	}
	if c.Scenario == "" {
		return c, fmt.Errorf("loadgen: a scenario name is required")
	}
	if c.ScenarioDir == "" {
		c.ScenarioDir = "test/load/scenarios"
	}
	if c.Track == "" {
		c.Track = TrackFirehose
	}
	if c.Track != TrackFirehose && c.Track != TrackPoll {
		return c, fmt.Errorf("loadgen: track %q must be %q or %q", c.Track, TrackFirehose, TrackPoll)
	}
	if c.SchedSample < 0 || c.SchedSample > 1 {
		return c, fmt.Errorf("loadgen: sched_sample %g must be in [0,1]", c.SchedSample)
	}
	if c.SubmitTimeout <= 0 {
		c.SubmitTimeout = 10 * time.Second
	}
	if c.RunTimeout <= 0 {
		c.RunTimeout = 2 * time.Minute
	}
	if c.PollAfter <= 0 {
		c.PollAfter = 5 * time.Second
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 2 * time.Second
	}
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = 2 * time.Minute
	}
	if c.Progress <= 0 {
		c.Progress = 5 * time.Second
	}
	if c.PollWorkers <= 0 {
		c.PollWorkers = 16
	}
	if c.MaxInflight < 0 {
		c.MaxInflight = 0
	}
	return c, nil
}
