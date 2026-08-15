package cache

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/mathcslearner/agentloom/internal/plugin"
)

// This file is the definition half of ticket 9.6's invalidation & ops
// surface: the Redis key-glob patterns an operator busts by, and the
// namespace and shape of the per-plugin stats counters the ops surface
// reports. Like the rest of internal/cache it stays a leaf — the redisstore
// subpackage turns these patterns into SCAN/UNLINK and HGETALL, and the API
// exposes them; this file only decides what a "bust everything for provider
// X" pattern is and where the counters live.

// BustMatch selects which cache entries an admin bust removes, at the
// namespace granularity RedisKey encodes (ADR-011 — the key prefix carries
// the KeySchemaVersion and the concrete plugin's kind and name, nothing
// finer). It deliberately cannot select a single run's entries: a
// run-scoped entry mixes the run id into the *hash*, not the Redis key, so
// its only bound is its TTL.
//
// The three valid shapes:
//
//   - zero value ({}, both empty) — every cache entry under the prefix.
//   - Kind set, Name empty — every entry for one plugin kind
//     (all model providers, all tools, all retrievers).
//   - Kind and Name set — every entry for one concrete plugin
//     (one provider, one tool, one retriever).
//
// A Name without a Kind is meaningless (names are unique only within a
// kind) and is rejected by BustPattern.
type BustMatch struct {
	Kind plugin.Kind
	Name string
}

// All reports whether the match selects every entry under the prefix.
func (m BustMatch) All() bool { return m.Kind == "" && m.Name == "" }

// BustPattern builds the Redis glob a SCAN-batched bust matches on. It
// mirrors RedisKey's layout — "<prefix>:v<ver>:<kind>:<name>:<hash>" — but
// matches "v*" across every KeySchemaVersion, so a bust also sweeps entries
// stranded behind an earlier key format (a version bump strands them behind
// an unreachable prefix; an explicit bust should still be able to reclaim
// that memory). Every literal segment (the operator-supplied prefix, the
// plugin name) is glob-escaped so a metacharacter in it can never widen the
// match.
//
// The "stats:" namespace can never be caught by these patterns: it does not
// begin with "v", so "<prefix>:v*:..." excludes it — a bust reclaims cache
// entries without erasing the cumulative counters the stats endpoint reads.
func BustPattern(prefix string, m BustMatch) (string, error) {
	base := globEscape(prefix) + ":v*"
	switch {
	case m.Kind == "" && m.Name == "":
		return base + ":*", nil
	case m.Kind == "" && m.Name != "":
		return "", fmt.Errorf("cache: bust match has a name %q but no kind", m.Name)
	case !m.Kind.Valid():
		return "", fmt.Errorf("cache: bust match kind %q is not a plugin kind", m.Kind)
	case m.Name == "":
		return base + ":" + globEscape(string(m.Kind)) + ":*", nil
	default:
		return base + ":" + globEscape(string(m.Kind)) + ":" + globEscape(m.Name) + ":*", nil
	}
}

// statsSegment is the fixed namespace segment separating the per-plugin
// stats counters from the cache entries under the same prefix.
const statsSegment = "stats"

// Stats field names inside a plugin's stats hash. hits+misses is the lookup
// total; the three mirror the engine's Prometheus cache counters on the
// normal path (redisstore records one on each corresponding Redis op), so
// the ops surface can reconcile against them.
const (
	StatsFieldHits   = "hits"
	StatsFieldMisses = "misses"
	StatsFieldStores = "stores"
)

// StatsRedisKey is the Redis key of one concrete plugin's stats hash:
// "<prefix>:stats:<kind>:<name>". It shares the entry prefix but sits in the
// "stats" segment, outside every BustPattern, so counters survive a bust.
func StatsRedisKey(prefix string, p PluginRef) string {
	return prefix + ":" + statsSegment + ":" + string(p.Kind) + ":" + p.Name
}

// StatsPattern is the SCAN glob matching every plugin's stats hash under the
// prefix. The prefix is glob-escaped; the "stats" segment and the
// kind/name tail are literal/wildcard.
func StatsPattern(prefix string) string {
	return globEscape(prefix) + ":" + statsSegment + ":*"
}

// PluginStats is one concrete plugin's cumulative cache counters, as read
// back from its stats hash. Hits/Misses/Stores are lifetime totals (bounded
// by the counters' TTL re-armed on every update); HitRate is Hits/(Hits+Misses),
// zero when there were no lookups.
type PluginStats struct {
	Kind    plugin.Kind
	Name    string
	Hits    int64
	Misses  int64
	Stores  int64
	HitRate float64
}

// ParseStatsKey recovers the plugin identity from a stats hash key produced
// by StatsRedisKey, given the same prefix. It returns ok=false for a key
// that does not belong to this prefix's stats namespace or is malformed
// (missing the kind/name tail), so a SCAN sweep can skip stray keys rather
// than fail. The kind is the first tail segment (kinds never contain ":");
// everything after it is the name, so a name is recovered verbatim even in
// the theoretical case it contained a colon.
func ParseStatsKey(prefix, key string) (PluginRef, bool) {
	want := prefix + ":" + statsSegment + ":"
	tail, ok := strings.CutPrefix(key, want)
	if !ok {
		return PluginRef{}, false
	}
	kind, name, ok := strings.Cut(tail, ":")
	if !ok || kind == "" || name == "" {
		return PluginRef{}, false
	}
	return PluginRef{Kind: plugin.Kind(kind), Name: name}, true
}

// NewPluginStats assembles a PluginStats from a concrete plugin and its raw
// counter values, computing HitRate. It is the store's projection helper,
// kept in the leaf so the derived hit-rate rule lives with the fields.
func NewPluginStats(p PluginRef, hits, misses, stores int64) PluginStats {
	s := PluginStats{Kind: p.Kind, Name: p.Name, Hits: hits, Misses: misses, Stores: stores}
	if lookups := hits + misses; lookups > 0 {
		s.HitRate = float64(hits) / float64(lookups)
	}
	return s
}

// ParseCounter parses one hash-field value into a counter, tolerating an
// absent field (empty string → 0). A malformed value is an error the caller
// surfaces — a corrupt counter is worth reporting, unlike a corrupt entry.
func ParseCounter(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cache: malformed counter value %q: %w", raw, err)
	}
	return n, nil
}

// globEscape backslash-escapes the Redis glob metacharacters so a literal
// segment (prefix or plugin name) matches only itself. Redis KEYS/SCAN
// globs treat *, ?, [, ], and \ specially (redis util.c stringmatchlen).
func globEscape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '*', '?', '[', ']', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
