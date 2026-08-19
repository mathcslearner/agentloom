// Package loadtest defines the load-test scenario contract (ticket 19.1):
// a named, versioned config that binds a workflow definition to an arrival
// profile and SLO targets. Scenarios are authored as JSON under
// test/load/scenarios/ and consumed by the load generator (cmd/loadgen,
// ticket 19.2); keeping the type + strict parser in a leaf package lets the
// scenarios be validated in CI — "runnable as named configs" — before the
// generator exists, and lets the generator reuse one parser.
//
// The package imports only internal/dag (to validate that a scenario's
// referenced definition is a well-formed workflow) plus stdlib, so it stays
// a leaf the load generator and tests can depend on freely.
package loadtest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// SchemaVersion is the scenario wire-contract version. Bumped on a
// breaking change to the JSON shape.
const SchemaVersion = 1

// ArrivalMode is how the generator paces submissions.
type ArrivalMode string

const (
	// ArrivalConstant submits at a fixed rate for the whole run.
	ArrivalConstant ArrivalMode = "constant"
	// ArrivalRamp linearly increases the rate from From to To in Step
	// increments, holding each for StepDuration — the knee-finding profile.
	ArrivalRamp ArrivalMode = "ramp"
)

// Scenario is one named load-test workload. It is open-loop: the generator
// submits at the configured arrival rate regardless of how fast runs
// complete, so a saturated system shows up as growing latency, not as a
// throttled submitter (the coordinated-omission guard, ticket 19.2).
type Scenario struct {
	// SchemaVersion pins the wire contract; must equal SchemaVersion.
	SchemaVersion int `json:"schema_version"`
	// Name is the scenario's stable identifier, matched by the generator's
	// --scenario flag and used in report artifacts. Lowercase kebab-case.
	Name string `json:"name"`
	// Description is a one-line human summary.
	Description string `json:"description"`
	// Definition is the workflow definition file this scenario submits,
	// as a path relative to the scenario file's directory. Required unless
	// Mix is set (a mixed scenario references other scenarios instead).
	Definition string `json:"definition,omitempty"`
	// Params are the run parameters submitted with each run (opaque to the
	// engine; templated by the definition).
	Params json.RawMessage `json:"params,omitempty"`
	// Arrival is the submission-rate profile.
	Arrival Arrival `json:"arrival"`
	// Warmup is the leading window whose measurements are discarded before
	// the steady-state percentiles are computed.
	Warmup Duration `json:"warmup"`
	// Duration is the steady-state measurement window length.
	Duration Duration `json:"duration"`
	// TargetActiveRuns is the concurrently-active-run count this scenario
	// is meant to sustain (0 = unset, informational).
	TargetActiveRuns int `json:"target_active_runs,omitempty"`
	// SLO records the pass/fail thresholds for this scenario (informational
	// to the parser; enforced by the campaign tooling in 19.3/19.6).
	SLO *SLO `json:"slo,omitempty"`
	// Mix, when set, makes this a composite scenario: each entry names a
	// sibling scenario and a weight, and Definition must be empty. Weights
	// must sum to 1.0 (within a small epsilon).
	Mix []MixEntry `json:"mix,omitempty"`
}

// Arrival is a submission-rate profile.
type Arrival struct {
	Mode        ArrivalMode `json:"mode"`
	RatePerSec  float64     `json:"rate_per_sec,omitempty"`
	Ramp        *Ramp       `json:"ramp,omitempty"`
	MaxInflight int         `json:"max_inflight,omitempty"` // 0 = unbounded (pure open loop)
}

// Ramp parametrizes ArrivalRamp.
type Ramp struct {
	FromPerSec   float64  `json:"from_per_sec"`
	ToPerSec     float64  `json:"to_per_sec"`
	StepPerSec   float64  `json:"step_per_sec"`
	StepDuration Duration `json:"step_duration"`
}

// MixEntry is one component of a composite scenario.
type MixEntry struct {
	Scenario string  `json:"scenario"`
	Weight   float64 `json:"weight"`
}

// SLO records target thresholds for a scenario (informational to the parser).
type SLO struct {
	SchedulingP50 Duration `json:"scheduling_p50,omitempty"`
	SchedulingP99 Duration `json:"scheduling_p99,omitempty"`
	APIP99        Duration `json:"api_p99,omitempty"`
	EndToEndP99   Duration `json:"end_to_end_p99,omitempty"`
}

// Duration is a time.Duration that marshals as a Go-duration string
// ("250ms", "10m") on the wire.
type Duration time.Duration

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// MarshalJSON renders the duration as a Go-duration string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON parses a Go-duration string (empty = zero).
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// Parse strictly decodes one scenario document, rejecting unknown fields so
// a typo fails loudly. It validates the scenario's shape but NOT that the
// referenced definition file exists or is well-formed — that needs the
// directory context (see LoadDir).
func Parse(data []byte) (*Scenario, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s Scenario
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("loadtest: decoding scenario: %w", err)
	}
	if dec.More() {
		return nil, errors.New("loadtest: trailing data after scenario JSON document")
	}
	if err := s.validateShape(); err != nil {
		return nil, err
	}
	return &s, nil
}

// validateShape checks the scenario is internally consistent, independent of
// the filesystem.
func (s *Scenario) validateShape() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if s.SchemaVersion != SchemaVersion {
		add("schema_version = %d, want %d", s.SchemaVersion, SchemaVersion)
	}
	if !isKebab(s.Name) {
		add("name %q must be lowercase kebab-case", s.Name)
	}
	composite := len(s.Mix) > 0
	if composite && s.Definition != "" {
		add("a mixed scenario must not also set a definition")
	}
	if !composite && s.Definition == "" {
		add("scenario must set either a definition or a mix")
	}
	switch s.Arrival.Mode {
	case ArrivalConstant:
		if s.Arrival.RatePerSec <= 0 {
			add("constant arrival requires a positive rate_per_sec")
		}
		if s.Arrival.Ramp != nil {
			add("constant arrival must not set a ramp block")
		}
	case ArrivalRamp:
		if s.Arrival.Ramp == nil {
			add("ramp arrival requires a ramp block")
		} else {
			r := s.Arrival.Ramp
			if r.FromPerSec < 0 || r.ToPerSec <= 0 || r.StepPerSec <= 0 {
				add("ramp rates must be non-negative with a positive to_per_sec and step_per_sec")
			}
			if r.ToPerSec < r.FromPerSec {
				add("ramp to_per_sec %g is below from_per_sec %g", r.ToPerSec, r.FromPerSec)
			}
			if r.StepDuration.D() <= 0 {
				add("ramp step_duration must be positive")
			}
		}
	default:
		add("arrival mode %q must be %q or %q", s.Arrival.Mode, ArrivalConstant, ArrivalRamp)
	}
	if s.Arrival.MaxInflight < 0 {
		add("max_inflight must not be negative")
	}
	if s.Duration.D() <= 0 {
		add("duration must be positive")
	}
	if s.Warmup.D() < 0 {
		add("warmup must not be negative")
	}
	if s.TargetActiveRuns < 0 {
		add("target_active_runs must not be negative")
	}
	if composite {
		var sum float64
		seen := map[string]bool{}
		for i, m := range s.Mix {
			if m.Scenario == "" {
				add("mix entry %d names no scenario", i)
			}
			if seen[m.Scenario] {
				add("mix entry %d duplicates scenario %q", i, m.Scenario)
			}
			seen[m.Scenario] = true
			if m.Weight <= 0 {
				add("mix entry %d (%q) weight must be positive", i, m.Scenario)
			}
			sum += m.Weight
		}
		if abs(sum-1.0) > 1e-6 {
			add("mix weights sum to %g, want 1.0", sum)
		}
	}
	if s.Params != nil && !json.Valid(s.Params) {
		add("params is not valid JSON")
	}
	return errors.Join(errs...)
}

// LoadDir parses every *.json scenario in dir, then validates cross-scenario
// invariants: each definition file exists and is a well-formed workflow (dag
// Decode + Validate), and every mix entry references a scenario present in the
// directory. It returns the scenarios sorted by name.
func LoadDir(dir string) ([]*Scenario, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("loadtest: reading scenario dir %s: %w", dir, err)
	}
	byName := map[string]*Scenario{}
	var scenarios []*Scenario
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, rerr := os.ReadFile(path) //nolint:gosec // G304: scenario paths from a trusted local corpus dir
		if rerr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), rerr))
			continue
		}
		s, perr := Parse(data)
		if perr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), perr))
			continue
		}
		if prior, dup := byName[s.Name]; dup {
			errs = append(errs, fmt.Errorf("%s: duplicate scenario name %q (also in another file)", e.Name(), prior.Name))
			continue
		}
		byName[s.Name] = s
		scenarios = append(scenarios, s)
	}
	// Cross-scenario checks, once every file has parsed.
	for _, s := range scenarios {
		if s.Definition != "" {
			if verr := validateDefinitionFile(filepath.Join(dir, s.Definition)); verr != nil {
				errs = append(errs, fmt.Errorf("%s: definition %s: %w", s.Name, s.Definition, verr))
			}
		}
		for _, m := range s.Mix {
			if _, ok := byName[m.Scenario]; !ok {
				errs = append(errs, fmt.Errorf("%s: mix references unknown scenario %q", s.Name, m.Scenario))
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	sort.Slice(scenarios, func(i, j int) bool { return scenarios[i].Name < scenarios[j].Name })
	return scenarios, nil
}

// validateDefinitionFile confirms a referenced definition decodes and passes
// full dag validation — the same gate the API applies at submission, so a
// scenario that would be rejected at submit fails the scenario suite instead.
func validateDefinitionFile(path string) error {
	data, err := os.ReadFile(path) //nolint:gosec // G304: definition path from a validated scenario config
	if err != nil {
		return err
	}
	def, err := dag.Decode(data)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	issues, err := dag.Validate(def)
	if err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	if len(issues) > 0 {
		return fmt.Errorf("validate: %d issue(s), first: %s", len(issues), issues[0])
	}
	return nil
}

func isKebab(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return s[0] != '-' && s[len(s)-1] != '-'
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
