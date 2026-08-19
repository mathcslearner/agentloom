// Command loadgen is the agentloom load generator (ticket 19.2): it drives the
// scenario corpus (test/load/scenarios, validated by internal/loadtest) against
// a running API under open-loop arrival control, tracks each run's full
// lifecycle (submit → terminal), and writes an HDR-percentile report artifact.
//
// It talks to the engine only through the public HTTP API. Example:
//
//	loadgen --scenario linear-10 --runs 100 --rate 20 --out results/dry
//	loadgen --scenario mixed --out results/mixed          # full scenario windows
//
// Credentials come from --key or AGENTLOOM_API_KEY (submit + read scopes).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mathcslearner/agentloom/internal/loadgen"
	"github.com/mathcslearner/agentloom/internal/loadtest"
)

func main() {
	if err := realMain(); err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
}

func realMain() error {
	var (
		apiBase   = flag.String("api", envOr("AGENTLOOM_API_URL", "http://localhost:8080"), "API base URL")
		apiKey    = flag.String("key", os.Getenv("AGENTLOOM_API_KEY"), "bearer API key (submit + read scopes)")
		scenDir   = flag.String("scenarios", "test/load/scenarios", "scenario corpus directory")
		scenario  = flag.String("scenario", "", "scenario name to run (required)")
		listOnly  = flag.Bool("list-scenarios", false, "list available scenarios and exit")
		rate      = flag.Float64("rate", 0, "override arrival rate (constant, per second)")
		ramp      = flag.String("ramp", "", "override arrival ramp as from:to:step:dur (e.g. 2:60:2:30s)")
		duration  = flag.Duration("duration", 0, "override steady-state window")
		warmup    = flag.Duration("warmup", 0, "override warmup window")
		maxRuns   = flag.Int("runs", 0, "cap total submissions (0 = unbounded; the dry-run knob)")
		track     = flag.String("track", "firehose", "terminal detection: firehose | poll")
		schedSamp = flag.Float64("sched-sample", 0.1, "fraction of runs whose scheduling latency is sampled from the firehose (0 disables step events)")
		inline    = flag.Bool("inline", false, "submit the definition body on every run (exercises the submission-path cost)")
		maxInf    = flag.Int("max-inflight", 0, "cap concurrent in-flight submits (0 = unbounded open loop)")
		submitTO  = flag.Duration("submit-timeout", 10*time.Second, "per-submit HTTP timeout")
		runTO     = flag.Duration("run-timeout", 2*time.Minute, "submit→terminal timeout before a run is counted as timed out")
		drainTO   = flag.Duration("drain-timeout", 2*time.Minute, "max wait for queue quiescence after arrivals stop")
		progress  = flag.Duration("progress", 5*time.Second, "progress line cadence")
		seed      = flag.Int64("seed", 1, "seed for the mix draw (reproducible composite campaigns)")
		out       = flag.String("out", "", "report output directory (default: results/<scenario>-<utc>)")
		failLost  = flag.Bool("fail-on-lost", true, "exit non-zero if any submitted run is lost")
	)
	flag.Parse()

	if *listOnly {
		return listScenarios(*scenDir)
	}
	if *scenario == "" {
		flag.Usage()
		return fmt.Errorf("--scenario is required")
	}

	outDir := *out
	if outDir == "" {
		outDir = fmt.Sprintf("results/%s-%s", *scenario, time.Now().UTC().Format("20060102-150405"))
	}

	cfg := loadgen.Config{
		APIBase: *apiBase, APIKey: *apiKey, ScenarioDir: *scenDir, Scenario: *scenario,
		RateOverride: *rate, RampOverride: *ramp, DurationOverride: *duration, WarmupOverride: *warmup,
		MaxRuns: *maxRuns, Track: loadgen.TrackMode(*track), SchedSample: *schedSamp, Inline: *inline,
		MaxInflight: *maxInf, SubmitTimeout: *submitTO, RunTimeout: *runTO, DrainTimeout: *drainTO,
		Progress: *progress, Seed: *seed, OutDir: outDir, FailOnLost: *failLost,
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	rep, err := loadgen.Run(ctx, cfg, logger, os.Stdout)
	if err != nil {
		return err
	}
	if cfg.FailOnLost && rep.Integrity.LostRuns > 0 {
		return fmt.Errorf("%d run(s) lost — see %s", rep.Integrity.LostRuns, outDir)
	}
	return nil
}

func listScenarios(dir string) error {
	scenarios, err := loadtest.LoadDir(dir)
	if err != nil {
		return err
	}
	for _, s := range scenarios {
		kind := "single"
		if len(s.Mix) > 0 {
			kind = "mixed"
		}
		fmt.Printf("%-16s %-7s %s\n", s.Name, kind, s.Description)
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
