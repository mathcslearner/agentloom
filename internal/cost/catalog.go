// Package cost is the M10 cost model (ADR-012): the versioned pricing catalog
// and the pure attribution/estimation arithmetic that later tickets meter
// attempts against.
//
// A pricing catalog maps a resource name — "<resolved-provider>:<model>"
// (e.g. "anthropic:claude-sonnet-5", "mock:sim-1"), the wildcard form
// "<provider>:*", or "tool:<name>" — to an effective-dated $/1M-token rate
// (or, for tools, a flat per-call price). The naming is deliberately the same
// key the ADR-010 limiter uses (internal/limits): a step's model
// "mock/sim-1" resolves to resource "mock:sim-1" for both rate limiting and
// pricing, so one convention governs both scheduler-path features.
//
// This package is a config leaf (stdlib only): it parses, validates, merges,
// and resolves catalogs, and computes costs, but imports no other agentloom
// package. The 10.2 ledger and 10.3 budget checks consume it; keeping it a
// leaf lets engine/exec/store depend on it without a cycle (the internal/cache
// and internal/limits precedent).
//
// Money is integer nano-USD (int64) everywhere a cost is computed or stored:
// 1 USD = 1e9 nano-USD (NanoPerUSD). Integer costs make 10.2's "run aggregate
// equals the exact sum of its ledger rows" property hold without float drift;
// catalog rates are authored as decimal $/1M tokens and converted at pricing
// time. int64 nano-USD saturates near $9.2e9, far above any real run.
package cost

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"
)

// CatalogSchemaVersion is the only pricing-catalog schema version v1 accepts.
// It is bumped when the document shape changes incompatibly; an override
// authored against a newer schema fails to load rather than being
// misinterpreted.
const CatalogSchemaVersion = 1

// Rate is a per-model price authored as USD per one million tokens, split by
// direction. Zero in either direction means that direction is free (a valid,
// if unusual, price). Converted to nano-USD at pricing time (see price.go).
type Rate struct {
	// InputPerMTok is the USD price per 1,000,000 input (prompt) tokens.
	InputPerMTok float64 `json:"input_per_mtok"`
	// OutputPerMTok is the USD price per 1,000,000 output (completion) tokens.
	OutputPerMTok float64 `json:"output_per_mtok"`
}

// ModelEntry is one effective-dated price for one model resource name. A
// catalog may carry several entries for the same name with different
// EffectiveFrom dates; Lookup picks the newest one effective at the query
// time, so a scheduled price change is authored as a second entry rather than
// an in-place edit (the old row keeps pricing historical runs).
type ModelEntry struct {
	// Name is the resource key: "<provider>:<model>" or the wildcard
	// "<provider>:*". Must not carry the "tool:" prefix (that namespace is
	// tools, below).
	Name string `json:"name"`
	// EffectiveFrom is the UTC calendar date this price takes effect
	// (inclusive). Required.
	EffectiveFrom Date `json:"effective_from"`
	// ContextWindow is the model's maximum context window in tokens (ADR-014,
	// ticket 12.6): the hard ceiling on assembled context + completion the
	// provider will accept. Optional (0 = unknown); when set it must be
	// positive. The M12 provider-window guardrail reads it to default a step's
	// context budget (window − max_tokens − headroom) and to reject an
	// oversize request before any provider call. A model with no window (or a
	// resource resolving only to the fallback) is unguarded — nothing to
	// compare against, the ADR-010 "unlimited by omission" stance.
	ContextWindow int64 `json:"context_window,omitempty"`
	// Rate is the price, flattened into the entry's JSON object
	// (input_per_mtok / output_per_mtok).
	Rate
}

// ToolEntry is one effective-dated flat price for a priced tool. Tools with no
// catalog entry are free (most built-in tools are), so the tool namespace has
// no unknown-entry policy: absent means $0, not "warn".
type ToolEntry struct {
	// Name is the resource key and must carry the "tool:" prefix.
	Name string `json:"name"`
	// EffectiveFrom is the UTC calendar date this price takes effect
	// (inclusive). Required.
	EffectiveFrom Date `json:"effective_from"`
	// PerCallUSD is the flat USD price charged per tool invocation.
	PerCallUSD float64 `json:"per_call_usd"`
}

// Catalog is a validated, immutable pricing catalog with name resolution.
// Construct one with Parse, Load, or Default; combine an override onto the
// embedded defaults with Merge. The zero value is unusable — use Default for
// an empty-override baseline. A nil *Catalog prices nothing (every lookup
// misses); the methods are nil-safe.
type Catalog struct {
	// models and tools are keyed by resource name; each value is the entry
	// list sorted by EffectiveFrom descending, so effective-date selection is
	// a linear scan for the first entry not after the query time.
	models      map[string][]ModelEntry
	tools       map[string][]ToolEntry
	fallback    Rate
	hasFallback bool
}

// catalogDoc is the wire shape of a catalog document.
type catalogDoc struct {
	SchemaVersion int          `json:"schema_version"`
	Models        []ModelEntry `json:"models"`
	Tools         []ToolEntry  `json:"tools"`
	// Fallback prices an unknown model under the estimate policy (ADR-012). A
	// standalone override may omit it to inherit the merged-onto catalog's
	// fallback; the runtime catalog (defaults, possibly overridden) always has
	// one.
	Fallback *Rate `json:"fallback"`
}

// Parse decodes and validates one pricing-catalog document. Decoding is strict
// (unknown fields rejected) and validation reports every problem at once,
// joined into one error — the config package's house style. A nil Fallback is
// permitted here (an override document need not restate it); Load and Default
// guarantee the runtime catalog has one.
func Parse(data []byte) (*Catalog, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var doc catalogDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("cost: decoding catalog: %w", err)
	}
	if dec.More() {
		return nil, errors.New("cost: catalog has trailing content after the JSON object")
	}

	var errs []error
	if doc.SchemaVersion != CatalogSchemaVersion {
		errs = append(errs, fmt.Errorf("schema_version: got %d, want %d", doc.SchemaVersion, CatalogSchemaVersion))
	}

	cat := &Catalog{
		models: make(map[string][]ModelEntry),
		tools:  make(map[string][]ToolEntry),
	}
	// Track (name, date) pairs so a duplicate price for the same model on the
	// same day is rejected rather than silently shadowing.
	modelSeen := make(map[string]bool)
	for i, m := range doc.Models {
		path := fmt.Sprintf("models[%d]", i)
		ok := true
		if err := validateModelName(m.Name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			ok = false
		}
		if m.EffectiveFrom.IsZero() {
			errs = append(errs, fmt.Errorf("%s (%q): effective_from is required", path, m.Name))
			ok = false
		}
		if m.ContextWindow < 0 {
			errs = append(errs, fmt.Errorf("%s (%q): context_window must be positive when set, got %d", path, m.Name, m.ContextWindow))
		}
		errs = validateRate(errs, path, m.Rate)
		if ok {
			key := m.Name + "@" + m.EffectiveFrom.String()
			if modelSeen[key] {
				errs = append(errs, fmt.Errorf("%s: duplicate price for %q effective %s", path, m.Name, m.EffectiveFrom))
			} else {
				modelSeen[key] = true
				cat.models[m.Name] = append(cat.models[m.Name], m)
			}
		}
	}
	toolSeen := make(map[string]bool)
	for i, tl := range doc.Tools {
		path := fmt.Sprintf("tools[%d]", i)
		ok := true
		if err := validateToolName(tl.Name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			ok = false
		}
		if tl.EffectiveFrom.IsZero() {
			errs = append(errs, fmt.Errorf("%s (%q): effective_from is required", path, tl.Name))
			ok = false
		}
		if tl.PerCallUSD < 0 {
			errs = append(errs, fmt.Errorf("%s (%q): per_call_usd must not be negative, got %v", path, tl.Name, tl.PerCallUSD))
		}
		if ok {
			key := tl.Name + "@" + tl.EffectiveFrom.String()
			if toolSeen[key] {
				errs = append(errs, fmt.Errorf("%s: duplicate price for %q effective %s", path, tl.Name, tl.EffectiveFrom))
			} else {
				toolSeen[key] = true
				cat.tools[tl.Name] = append(cat.tools[tl.Name], tl)
			}
		}
	}
	if doc.Fallback != nil {
		errs = validateRate(errs, "fallback", *doc.Fallback)
		cat.fallback = *doc.Fallback
		cat.hasFallback = true
	}

	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("cost: invalid pricing catalog:\n%w", err)
	}
	cat.sortEntries()
	return cat, nil
}

// Load builds the runtime catalog from the embedded defaults and at most one
// operator override (an inline JSON document or a file path — both non-empty
// is an error). The override is merged onto the defaults by Merge (an override
// entry replaces that resource name's entries; unlisted defaults survive), so
// an operator adds or reprices without restating the whole catalog and can
// never accidentally un-price a model. Both empty returns the embedded
// defaults unchanged. Whitespace-only sources count as empty.
func Load(inline, file string) (*Catalog, error) {
	inline = strings.TrimSpace(inline)
	file = strings.TrimSpace(file)
	if inline != "" && file != "" {
		return nil, errors.New("cost: provide an inline catalog or a catalog file, not both")
	}
	base, err := Default()
	if err != nil {
		return nil, err
	}
	var override *Catalog
	switch {
	case inline != "":
		override, err = Parse([]byte(inline))
		if err != nil {
			return nil, fmt.Errorf("cost: from inline catalog: %w", err)
		}
	case file != "":
		// The path is operator-supplied config (AGENTLOOM_PRICING_FILE), the
		// same trust level as the Postgres DSN — not attacker-controlled input.
		data, err := os.ReadFile(file) //nolint:gosec // G304: path is trusted operator config, not user input
		if err != nil {
			return nil, fmt.Errorf("cost: reading catalog file %q: %w", file, err)
		}
		override, err = Parse(data)
		if err != nil {
			return nil, fmt.Errorf("cost: from catalog file %q: %w", file, err)
		}
	default:
		return base, nil
	}
	return Merge(base, override), nil
}

// Merge overlays override onto base: every resource name present in override
// replaces base's entries for that name outright (whole-list replacement, not
// per-date union — so a name's price history is authored in one place), and
// override's fallback replaces base's when set. Names only in base survive.
// Both inputs are assumed already validated (Parse guarantees it). base must
// be non-nil; override may be nil (returns base).
func Merge(base, override *Catalog) *Catalog {
	if override == nil {
		return base
	}
	out := &Catalog{
		models:      make(map[string][]ModelEntry, len(base.models)),
		tools:       make(map[string][]ToolEntry, len(base.tools)),
		fallback:    base.fallback,
		hasFallback: base.hasFallback,
	}
	for name, entries := range base.models {
		out.models[name] = entries
	}
	for name, entries := range base.tools {
		out.tools[name] = entries
	}
	for name, entries := range override.models {
		out.models[name] = entries
	}
	for name, entries := range override.tools {
		out.tools[name] = entries
	}
	if override.hasFallback {
		out.fallback = override.fallback
		out.hasFallback = true
	}
	return out
}

// Fallback returns the catalog's unknown-model fallback rate and whether one
// is set. The runtime catalog (from Load or Default) always has one; a
// directly-Parsed override may not.
func (c *Catalog) Fallback() (Rate, bool) {
	if c == nil {
		return Rate{}, false
	}
	return c.fallback, c.hasFallback
}

// ModelCount reports the number of distinct model resource names priced.
// Nil-safe. Used for boot-time logging.
func (c *Catalog) ModelCount() int {
	if c == nil {
		return 0
	}
	return len(c.models)
}

// ToolCount reports the number of distinct priced tool names. Nil-safe.
func (c *Catalog) ToolCount() int {
	if c == nil {
		return 0
	}
	return len(c.tools)
}

// sortEntries orders every entry list newest-effective-first, so Lookup's scan
// returns the currently-effective price first.
func (c *Catalog) sortEntries() {
	for name := range c.models {
		entries := c.models[name]
		sort.SliceStable(entries, func(i, j int) bool {
			return entries[i].EffectiveFrom.After(entries[j].EffectiveFrom.Time)
		})
		c.models[name] = entries
	}
	for name := range c.tools {
		entries := c.tools[name]
		sort.SliceStable(entries, func(i, j int) bool {
			return entries[i].EffectiveFrom.After(entries[j].EffectiveFrom.Time)
		})
		c.tools[name] = entries
	}
}

// validateModelName enforces the ADR-010 resource-name rules (non-empty, no
// whitespace, "*" only as a trailing ":*" wildcard) plus the model/tool
// namespace split. Duplicated from internal/limits rather than imported to
// keep cost a leaf — the same small rule, cross-referenced in both.
func validateModelName(name string) error {
	if err := validateResourceName(name); err != nil {
		return err
	}
	if strings.HasPrefix(name, "tool:") {
		return fmt.Errorf("model name %q must not use the reserved \"tool:\" prefix", name)
	}
	return nil
}

// validateToolName enforces the resource-name rules and requires the "tool:"
// namespace prefix so tool and model prices can never collide.
func validateToolName(name string) error {
	if err := validateResourceName(name); err != nil {
		return err
	}
	if !strings.HasPrefix(name, "tool:") || name == "tool:" {
		return fmt.Errorf("tool name %q must be of the form \"tool:<name>\"", name)
	}
	return nil
}

func validateResourceName(name string) error {
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

// validateRate enforces non-negative, finite rates in both directions. A
// zero-in-both-directions rate is permitted (a legitimately free model).
func validateRate(errs []error, path string, r Rate) []error {
	errs = validateRateComponent(errs, path+".input_per_mtok", r.InputPerMTok)
	errs = validateRateComponent(errs, path+".output_per_mtok", r.OutputPerMTok)
	return errs
}

func validateRateComponent(errs []error, path string, v float64) []error {
	if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		errs = append(errs, fmt.Errorf("%s: must be a non-negative finite number, got %v", path, v))
	}
	return errs
}

// Date is a UTC calendar date authored as "YYYY-MM-DD". Prices are
// effective-dated at day granularity — finer precision would imply a
// within-day price change the format deliberately cannot express.
type Date struct{ time.Time }

const dateLayout = "2006-01-02"

// UnmarshalJSON parses a "YYYY-MM-DD" string into a UTC midnight date.
func (d *Date) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("effective_from must be a %q string: %w", dateLayout, err)
	}
	t, err := time.ParseInLocation(dateLayout, s, time.UTC)
	if err != nil {
		return fmt.Errorf("invalid date %q (want %s): %w", s, dateLayout, err)
	}
	d.Time = t
	return nil
}

// MarshalJSON renders the date back as a "YYYY-MM-DD" string.
func (d Date) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Format(dateLayout))
}

// String renders the date as "YYYY-MM-DD".
func (d Date) String() string { return d.Format(dateLayout) }
