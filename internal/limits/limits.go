// Package limits loads and validates the fleet-wide resource-limit
// configuration of ADR-010 (ticket 9.1): the named external resources whose
// request and token throughput the M9 limiter middleware governs across every
// worker process.
//
// A resource is a named capacity pool — "anthropic:<model>",
// "openai:<model>", "mock:<model>" (named by the *resolved* provider, so a
// step's model "mock/sim-1" binds to resource "mock:sim-1"), or a custom
// "tool:<name>" — carrying up to two limits: requests per minute and tokens
// per minute. The 9.2 middleware maps each onto a 6.3 Redis token bucket;
// this package is a config leaf (stdlib only) that parses, validates, and
// resolves names, so the two concerns stay independent — limits imports
// neither internal/ratelimit nor any engine package.
//
// Resolution (Lookup) is exact-match first, then a provider wildcard
// "<provider>:*", then not-found. Not-found means unlimited by decision
// (ADR-010's unknown-resource policy): limits are protective opt-in, and
// failing closed would brick the fleet the first time a new model name
// appeared before an operator added its limit.
package limits

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
)

// Rate is one token-bucket limit in the config's natural unit, per minute,
// with an optional burst ceiling.
type Rate struct {
	// PerMinute is the sustained refill rate. Strictly positive and finite
	// (validated) — v1 admits no fixed-quota, rate-zero buckets.
	PerMinute float64 `json:"per_minute"`
	// Burst is the maximum balance — the largest instantaneous spend the
	// bucket tolerates. Absent (zero) it defaults to one minute of refill, so
	// an un-bursted bucket refills to exactly its per-minute rate. Never
	// negative (validated).
	Burst int64 `json:"burst,omitempty"`
}

// RefillPerSec is the continuous refill rate the 9.2 middleware hands to
// ratelimit.Bucket.RefillPerSec. The config unit is per-minute for human
// legibility; the bucket math is per-second (ADR-010).
func (r Rate) RefillPerSec() float64 { return r.PerMinute / 60 }

// Capacity is the bucket capacity (burst) the 9.2 middleware hands to
// ratelimit.Bucket.Capacity: the explicit burst, or one minute of refill
// rounded up when unset — so a sub-integer per-minute rate still admits at
// least one unit of cost.
func (r Rate) Capacity() int64 {
	if r.Burst > 0 {
		return r.Burst
	}
	return int64(math.Ceil(r.PerMinute))
}

// Resource is one named capacity pool with up to two limits. At least one of
// Requests/Tokens is non-nil (validated); a nil limit means that dimension is
// unbounded for this resource.
type Resource struct {
	// Name is the resource key, "<provider>:<identifier>" (e.g.
	// "anthropic:claude-sonnet-5") or the wildcard form "<provider>:*".
	Name string `json:"name"`
	// Requests limits provider calls (cost 1 each); nil = unbounded.
	Requests *Rate `json:"requests,omitempty"`
	// Tokens limits estimated token throughput (cost = the pre-call
	// estimate); nil = unbounded.
	Tokens *Rate `json:"tokens,omitempty"`
}

// Set is a validated, immutable collection of resource limits with name
// resolution. A nil or empty Set means every resource is unlimited.
// Construct one with Parse or Load.
type Set struct {
	byName map[string]Resource
	names  []string // sorted, for deterministic Names()
}

// configFile is the top-level JSON shape.
type configFile struct {
	Resources []Resource `json:"resources"`
}

// Parse decodes and validates a resource-limit config document. Decoding is
// strict (unknown fields rejected) and validation reports every problem at
// once, joined into one error — the config package's house style.
func Parse(data []byte) (*Set, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var cf configFile
	if err := dec.Decode(&cf); err != nil {
		return nil, fmt.Errorf("limits: decoding config: %w", err)
	}
	if dec.More() {
		return nil, errors.New("limits: config has trailing content after the JSON object")
	}

	set := &Set{byName: make(map[string]Resource, len(cf.Resources))}
	var errs []error
	for i, r := range cf.Resources {
		path := fmt.Sprintf("resources[%d]", i)
		nameOK := true
		if err := validateName(r.Name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			nameOK = false
		} else if _, dup := set.byName[r.Name]; dup {
			errs = append(errs, fmt.Errorf("%s: duplicate resource name %q", path, r.Name))
			nameOK = false
		}
		if r.Requests == nil && r.Tokens == nil {
			errs = append(errs, fmt.Errorf("%s (%q): at least one of requests/tokens is required", path, r.Name))
		}
		errs = validateRate(errs, path+".requests", r.Requests)
		errs = validateRate(errs, path+".tokens", r.Tokens)
		if nameOK {
			set.byName[r.Name] = r
			set.names = append(set.names, r.Name)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("limits: invalid resource configuration:\n%w", err)
	}
	sort.Strings(set.names)
	return set, nil
}

// Load builds a Set from exactly one of an inline JSON document or a file
// path. Both empty means no configured limits (an empty Set — everything
// unlimited); both non-empty is an error. Whitespace-only sources count as
// empty. File reads and JSON parsing live here so the config package stays
// env-pure (it only carries the two strings and the mutual-exclusion check).
func Load(inline, file string) (*Set, error) {
	inline = strings.TrimSpace(inline)
	file = strings.TrimSpace(file)
	switch {
	case inline != "" && file != "":
		return nil, errors.New("limits: provide an inline config or a config file, not both")
	case inline != "":
		set, err := Parse([]byte(inline))
		if err != nil {
			return nil, fmt.Errorf("limits: from inline config: %w", err)
		}
		return set, nil
	case file != "":
		// The path is operator-supplied config (AGENTLOOM_RESOURCES_FILE), the
		// same trust level as the Postgres DSN — not attacker-controlled input.
		data, err := os.ReadFile(file) //nolint:gosec // G304: path is trusted operator config, not user input
		if err != nil {
			return nil, fmt.Errorf("limits: reading config file %q: %w", file, err)
		}
		set, err := Parse(data)
		if err != nil {
			return nil, fmt.Errorf("limits: from config file %q: %w", file, err)
		}
		return set, nil
	default:
		return &Set{byName: map[string]Resource{}}, nil
	}
}

// Lookup resolves a resource name to its limit: exact match, then the
// provider wildcard "<provider>:*", then not-found. A false second return
// means the resource is unlimited (ADR-010's unknown-resource policy). Safe
// on a nil Set.
func (s *Set) Lookup(name string) (Resource, bool) {
	if s == nil {
		return Resource{}, false
	}
	if r, ok := s.byName[name]; ok {
		return r, true
	}
	if i := strings.IndexByte(name, ':'); i >= 0 {
		if r, ok := s.byName[name[:i]+":*"]; ok {
			return r, true
		}
	}
	return Resource{}, false
}

// Len reports the number of configured resources. Safe on a nil Set.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.names)
}

// Names returns the configured resource names in sorted order. Safe on a nil
// Set. The returned slice is a copy — callers may not mutate the Set.
func (s *Set) Names() []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s.names))
	copy(out, s.names)
	return out
}

// validateName enforces the ADR-010 name rules: non-empty, no whitespace, and
// "*" only as a trailing ":*" wildcard segment.
func validateName(name string) error {
	if name == "" {
		return errors.New("resource name is required")
	}
	if strings.ContainsAny(name, " \t\r\n\v\f") {
		return fmt.Errorf("resource name %q must not contain whitespace", name)
	}
	if strings.Contains(name, "*") {
		if !strings.HasSuffix(name, ":*") || strings.Count(name, "*") != 1 {
			return fmt.Errorf("resource name %q: '*' is only allowed as a trailing \":*\" wildcard", name)
		}
	}
	return nil
}

// validateRate enforces a positive finite per-minute rate and a non-negative
// burst. A nil rate (dimension unbounded) is valid.
func validateRate(errs []error, path string, r *Rate) []error {
	if r == nil {
		return errs
	}
	if r.PerMinute <= 0 || math.IsNaN(r.PerMinute) || math.IsInf(r.PerMinute, 0) {
		errs = append(errs, fmt.Errorf("%s: per_minute must be a positive finite number, got %v", path, r.PerMinute))
	}
	if r.Burst < 0 {
		errs = append(errs, fmt.Errorf("%s: burst must not be negative, got %d", path, r.Burst))
	}
	return errs
}
