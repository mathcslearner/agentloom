package loadgen

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/loadtest"
)

// component is one submittable workload: its definition spec bytes, the
// registered definition id (for by-ref submission), and its run params.
type component struct {
	name   string
	spec   json.RawMessage
	defID  string
	params json.RawMessage
}

// Run executes a load campaign and returns the report. It uses the real clock;
// the pure pacing/schedule logic is unit-tested separately with a fake clock.
func Run(ctx context.Context, cfg Config, logger *slog.Logger, out io.Writer) (Report, error) {
	return run(ctx, cfg, logger, out, realClock{})
}

func run(ctx context.Context, cfg Config, logger *slog.Logger, out io.Writer, clk clock) (Report, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return Report{}, err
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	scenarios, err := loadtest.LoadDir(cfg.ScenarioDir)
	if err != nil {
		return Report{}, fmt.Errorf("loading scenario corpus: %w", err)
	}
	byName := map[string]*loadtest.Scenario{}
	for _, s := range scenarios {
		byName[s.Name] = s
	}
	sc := byName[cfg.Scenario]
	if sc == nil {
		return Report{}, fmt.Errorf("scenario %q not found in %s", cfg.Scenario, cfg.ScenarioDir)
	}
	applyOverrides(sc, cfg)

	client := newAPIClient(cfg.APIBase, cfg.APIKey, cfg.SubmitTimeout)
	if err := client.health(ctx); err != nil {
		return Report{}, fmt.Errorf("api health check: %w", err)
	}

	comps, err := resolveComponents(ctx, client, cfg, sc, byName)
	if err != nil {
		return Report{}, err
	}
	compByName := map[string]*component{}
	for _, c := range comps {
		compByName[c.name] = c
	}

	// Clock-skew estimate + DLQ baseline from the stats endpoint.
	skew, dlqStart := probeSkewAndDLQ(ctx, client, clk)

	tr := newTracker(cfg.SchedSample, skew)

	// Firehose (optional).
	var fh *firehoseClient
	fctx, fcancel := context.WithCancel(ctx)
	defer fcancel()
	if cfg.Track == TrackFirehose {
		fh = newFirehoseClient(cfg.APIBase, cfg.APIKey, cfg.SchedSample > 0, tr.applyEvent, logger)
		go fh.Run(fctx)
		time.Sleep(200 * time.Millisecond) // let the subscribe install before submissions
	}

	// Schedule + windows.
	warmup := sc.Warmup.D()
	total := warmup + sc.Duration.D()
	var mix *mixDraw
	if len(sc.Mix) > 0 {
		mix = newMixDraw(sc.Mix, cfg.Seed)
	}
	fires := buildSchedule(sc, total, cfg.MaxRuns, mix)
	start := clk.Now()
	steadyStart := start.Add(warmup)
	steadyEnd := steadyStart.Add(sc.Duration.D())
	inSteady := func(t time.Time) bool { return !t.Before(steadyStart) && t.Before(steadyEnd) }

	pacerLag := NewHistogram(1, 0.01)

	// Poll pool + progress + timeseries all run until drain completes.
	stopBg := make(chan struct{})
	var bgWG sync.WaitGroup
	var tsMu sync.Mutex
	var ts []tsSample

	bgWG.Add(1)
	go func() { // per-run poll backstop
		defer bgWG.Done()
		tick := time.NewTicker(cfg.PollInterval)
		defer tick.Stop()
		for {
			select {
			case <-stopBg:
				return
			case <-tick.C:
				pollOverdue(ctx, client, tr, cfg, clk.Now())
			}
		}
	}()
	bgWG.Add(1)
	go func() { // progress + timeseries + stats
		defer bgWG.Done()
		tick := time.NewTicker(cfg.Progress)
		defer tick.Stop()
		for {
			select {
			case <-stopBg:
				return
			case <-tick.C:
				snap := tr.snapshot()
				stats, _ := client.systemStats(ctx)
				elapsed := clk.Now().Sub(start).Seconds()
				printProgress(out, "run", elapsed, snap, stats, fh)
				tsMu.Lock()
				ts = append(ts, tsSample{
					AtSec: elapsed, Submitted: snap.Total, Accepted: snap.Accepted,
					Active: snap.Active, Terminal: snap.Terminal,
					ReadyDepth: queueField(stats, func(q *api.QueueStatsView) int64 { return q.ReadyDepth }),
					Pending:    queueField(stats, func(q *api.QueueStatsView) int64 { return q.Pending }),
					Delayed:    queueField(stats, func(q *api.QueueStatsView) int64 { return q.Delayed }),
					Outbox:     stats.Outbox.Backlog,
				})
				tsMu.Unlock()
			}
		}
	}()

	// Arrival phase: pace submissions (non-blocking dispatch → open loop).
	var sem chan struct{}
	if cfg.MaxInflight > 0 {
		sem = make(chan struct{}, cfg.MaxInflight)
	}
	var submitWG sync.WaitGroup
	dispatch := func(idx int, f fire, lag time.Duration) {
		intended := start.Add(f.Offset)
		steady := inSteady(intended)
		if steady {
			pacerLag.RecordDuration(lag)
		}
		tr.registerFire(idx, f.Component, intended, steady)
		comp := compByName[f.Component]
		if comp == nil && f.Component == "" && len(comps) == 1 {
			comp = comps[0]
		}
		if comp == nil {
			tr.recordSubmit(idx, submitResult{Err: fmt.Errorf("no component for %q", f.Component)}, clk.Now())
			return
		}
		if sem != nil {
			select {
			case sem <- struct{}{}:
			default:
				tr.recordSkip(idx)
				return
			}
		}
		submitWG.Add(1)
		go func() {
			defer submitWG.Done()
			if sem != nil {
				defer func() { <-sem }()
			}
			defID := comp.defID
			var spec json.RawMessage
			if cfg.Inline {
				defID, spec = "", comp.spec
			}
			res := client.submit(ctx, defID, spec, comp.params)
			tr.recordSubmit(idx, res, clk.Now())
		}()
	}
	runPacer(ctx, fires, start, clk, dispatch)
	submitWG.Wait()
	arrivalsEnd := clk.Now()

	// Drain: wait for accepted runs to reach terminal, bounded by RunTimeout.
	drainRuns(ctx, tr, cfg, clk, arrivalsEnd, out, start)
	tr.finalizeOpen()

	// Reconcile: list every campaign definition's runs, cross-check the
	// submitted set (the "no lost runs" assertion).
	lost := reconcile(ctx, client, tr, comps, start)

	// Quiescence: wait for the queue/outbox to drain, bounded by DrainTimeout.
	quiesce, finalStats := waitQuiescent(ctx, client, cfg, clk)

	close(stopBg)
	bgWG.Wait()

	// Assemble the report.
	rep := assembleReport(cfg, sc, comps, tr, pacerLag, ts, skew, start, steadyStart, steadyEnd, arrivalsEnd, quiesce, lost, dlqStart, dlqOpen(finalStats))

	if cfg.OutDir != "" {
		hists := map[string]*Histogram{
			"submit_rtt":           tr.submitRTT,
			"submit_from_intended": tr.submitCorr,
			"end_to_end":           tr.e2e,
			"scheduling":           tr.sched,
		}
		if err := writeArtifacts(cfg.OutDir, rep, tr.runRows(), ts, hists); err != nil {
			return rep, fmt.Errorf("writing artifacts: %w", err)
		}
		fmt.Fprintf(out, "\nreport written to %s\n", filepath.Clean(cfg.OutDir)) //nolint:errcheck // stdio write
	}
	return rep, nil
}

// applyOverrides applies the CLI overrides onto a scenario in place.
func applyOverrides(sc *loadtest.Scenario, cfg Config) {
	if cfg.RateOverride > 0 {
		sc.Arrival = loadtest.Arrival{Mode: loadtest.ArrivalConstant, RatePerSec: cfg.RateOverride, MaxInflight: sc.Arrival.MaxInflight}
	}
	if cfg.RampOverride != "" {
		if r, ok := parseRampOverride(cfg.RampOverride); ok {
			sc.Arrival = loadtest.Arrival{Mode: loadtest.ArrivalRamp, Ramp: r}
		}
	}
	if cfg.DurationOverride > 0 {
		sc.Duration = loadtest.Duration(cfg.DurationOverride)
	}
	if cfg.WarmupOverride > 0 {
		sc.Warmup = loadtest.Duration(cfg.WarmupOverride)
	}
	// A --runs dry run with no explicit windows still needs a finite duration
	// to bound the schedule; a large ceiling lets MaxRuns end it first.
	if cfg.MaxRuns > 0 && sc.Duration.D() <= 0 {
		sc.Duration = loadtest.Duration(24 * time.Hour)
	}
}

// resolveComponents registers each workload's definition and returns the
// submittable components. For a single-definition scenario there is one
// component (name ""); for a mixed scenario one per mix entry.
func resolveComponents(ctx context.Context, client *apiClient, cfg Config, sc *loadtest.Scenario, byName map[string]*loadtest.Scenario) ([]*component, error) {
	var specs []struct {
		name   string
		path   string
		params json.RawMessage
	}
	if len(sc.Mix) > 0 {
		for _, m := range sc.Mix {
			child := byName[m.Scenario]
			if child == nil || child.Definition == "" {
				return nil, fmt.Errorf("mixed scenario references %q which has no definition", m.Scenario)
			}
			specs = append(specs, struct {
				name   string
				path   string
				params json.RawMessage
			}{m.Scenario, filepath.Join(cfg.ScenarioDir, child.Definition), child.Params})
		}
	} else {
		specs = append(specs, struct {
			name   string
			path   string
			params json.RawMessage
		}{"", filepath.Join(cfg.ScenarioDir, sc.Definition), sc.Params})
	}

	var comps []*component
	for _, s := range specs {
		raw, err := os.ReadFile(s.path) //nolint:gosec // corpus path from a validated scenario
		if err != nil {
			return nil, fmt.Errorf("reading definition %s: %w", s.path, err)
		}
		defName, derr := definitionName(raw)
		if derr != nil {
			return nil, fmt.Errorf("definition %s: %w", s.path, derr)
		}
		id, err := client.registerDefinition(ctx, defName, raw)
		if err != nil {
			return nil, fmt.Errorf("registering %s: %w", defName, err)
		}
		comps = append(comps, &component{name: s.name, spec: raw, defID: id, params: s.params})
	}
	return comps, nil
}

// definitionName extracts the top-level "name" from a definition document.
func definitionName(raw json.RawMessage) (string, error) {
	var head struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return "", err
	}
	if head.Name == "" {
		return "", fmt.Errorf("definition has no name")
	}
	return head.Name, nil
}

func dlqOpen(s api.SystemStatsResponse) int64 { return s.DeadLetters.Open }

func queueField(s api.SystemStatsResponse, f func(*api.QueueStatsView) int64) int64 {
	if s.Queue == nil {
		return -1
	}
	return f(s.Queue)
}

// probeSkewAndDLQ estimates server−client clock skew and reads the DLQ baseline.
func probeSkewAndDLQ(ctx context.Context, client *apiClient, clk clock) (time.Duration, int64) {
	t0 := clk.Now()
	stats, err := client.systemStats(ctx)
	t1 := clk.Now()
	if err != nil || stats.ObservedAt.IsZero() {
		return 0, 0
	}
	clientMid := t0.Add(t1.Sub(t0) / 2)
	return stats.ObservedAt.Sub(clientMid), stats.DeadLetters.Open
}
