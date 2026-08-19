// Package loadgen is the load generator's engine (ticket 19.2): open-loop
// arrival control, run-lifecycle tracking, latency histograms, and report
// artifacts. It reuses internal/loadtest's scenario contract (19.1) so the
// generator and the CI-validated corpus share one parser, and drives the
// engine only through internal/api's public wire types.
//
// The package imports internal/loadtest, internal/api (wire types), and
// internal/event (the firehose envelope), plus coder/websocket and stdlib.
package loadgen

import (
	"math"
	"sort"
	"time"
)

// Histogram is a log-linear High-Dynamic-Range histogram over durations. It
// keeps bounded memory regardless of sample count by bucketing values on a
// geometric scale (one bucket per multiplicative growth step), so a value is
// recorded with a bounded relative error (~growth-1). This is the HDR property
// the plan calls for; we hand-roll it rather than take a dependency so the
// module stays buildable offline (the tiktoken/embed precedent).
//
// The zero value is not usable; construct with NewHistogram. A Histogram is
// not safe for concurrent Record — callers serialize (the tracker holds a
// mutex).
type Histogram struct {
	minTrackable int64   // smallest distinguishable value, in the recorded unit
	logGrowth    float64 // ln(growth); a value's bucket is floor(ln(v/min)/logGrowth)
	growth       float64 // 1 + relative error per bucket (e.g. 1.01)
	buckets      []int64
	total        int64
	sum          float64 // sum of actual recorded values, for an exact mean
	min, max     int64
}

// NewHistogram builds a histogram whose buckets grow by relErr (a fraction,
// e.g. 0.01 for 1% relative error) starting at minTrackable (values below it
// are clamped up to it). Both are in whatever unit the caller records
// (microseconds throughout loadgen).
func NewHistogram(minTrackable int64, relErr float64) *Histogram {
	if minTrackable < 1 {
		minTrackable = 1
	}
	if relErr <= 0 {
		relErr = 0.01
	}
	g := 1 + relErr
	return &Histogram{
		minTrackable: minTrackable,
		growth:       g,
		logGrowth:    math.Log(g),
		min:          math.MaxInt64,
	}
}

// bucketOf returns the bucket index for a value (clamped to >= minTrackable).
func (h *Histogram) bucketOf(v int64) int {
	if v < h.minTrackable {
		v = h.minTrackable
	}
	return int(math.Log(float64(v)/float64(h.minTrackable)) / h.logGrowth)
}

// Record adds one observation. Negative values are clamped to zero-equivalent
// (minTrackable); this happens for a clock that briefly runs backwards.
func (h *Histogram) Record(v int64) {
	if v < 0 {
		v = 0
	}
	idx := h.bucketOf(v)
	if idx >= len(h.buckets) {
		grown := make([]int64, idx+1)
		copy(grown, h.buckets)
		h.buckets = grown
	}
	h.buckets[idx]++
	h.total++
	h.sum += float64(v)
	if v < h.min {
		h.min = v
	}
	if v > h.max {
		h.max = v
	}
}

// RecordDuration records a duration in microseconds (loadgen's unit).
func (h *Histogram) RecordDuration(d time.Duration) { h.Record(d.Microseconds()) }

// Count is the number of recorded observations.
func (h *Histogram) Count() int64 { return h.total }

// Mean is the exact arithmetic mean of the recorded values (loadgen keeps the
// running sum, so the mean is not subject to the bucketing error).
func (h *Histogram) Mean() float64 {
	if h.total == 0 {
		return 0
	}
	return h.sum / float64(h.total)
}

// Max is the exact maximum recorded value.
func (h *Histogram) Max() int64 {
	if h.total == 0 {
		return 0
	}
	return h.max
}

// Min is the exact minimum recorded value.
func (h *Histogram) Min() int64 {
	if h.total == 0 {
		return 0
	}
	return h.min
}

// ValueAtQuantile returns the value at the given quantile (0..1), reported as
// the upper edge of the containing bucket — a conservative (never-under)
// estimate within the bucket's relative error.
func (h *Histogram) ValueAtQuantile(q float64) int64 {
	if h.total == 0 {
		return 0
	}
	if q < 0 {
		q = 0
	}
	if q > 1 {
		q = 1
	}
	target := int64(math.Ceil(q * float64(h.total)))
	if target < 1 {
		target = 1
	}
	var cum int64
	for idx, c := range h.buckets {
		cum += c
		if cum >= target {
			// Upper edge of this bucket: min * growth^(idx+1), but never
			// beyond the true max (the top bucket's edge overshoots).
			edge := int64(float64(h.minTrackable) * math.Pow(h.growth, float64(idx+1)))
			if edge > h.max {
				edge = h.max
			}
			if edge < h.min {
				edge = h.min
			}
			return edge
		}
	}
	return h.max
}

// Percentiles is the standard quantile set loadgen reports.
type Percentiles struct {
	Count int64   `json:"count"`
	Mean  float64 `json:"mean_us"`
	Min   int64   `json:"min_us"`
	P50   int64   `json:"p50_us"`
	P90   int64   `json:"p90_us"`
	P95   int64   `json:"p95_us"`
	P99   int64   `json:"p99_us"`
	P999  int64   `json:"p999_us"`
	Max   int64   `json:"max_us"`
}

// Snapshot computes the reported percentile set.
func (h *Histogram) Snapshot() Percentiles {
	return Percentiles{
		Count: h.Count(),
		Mean:  h.Mean(),
		Min:   h.Min(),
		P50:   h.ValueAtQuantile(0.50),
		P90:   h.ValueAtQuantile(0.90),
		P95:   h.ValueAtQuantile(0.95),
		P99:   h.ValueAtQuantile(0.99),
		P999:  h.ValueAtQuantile(0.999),
		Max:   h.Max(),
	}
}

// distributionRow is one CSV row of the recorded distribution.
type distributionRow struct {
	Quantile float64
	ValueUS  int64
	CountAt  int64
}

// distribution renders the histogram as an ascending quantile table for the
// per-histogram CSV artifact (a coarse fixed quantile grid plus the extremes).
func (h *Histogram) distribution() []distributionRow {
	if h.total == 0 {
		return nil
	}
	qs := []float64{
		0, 0.1, 0.2, 0.25, 0.3, 0.4, 0.5, 0.6, 0.7, 0.75, 0.8, 0.9,
		0.95, 0.975, 0.99, 0.995, 0.999, 0.9999, 1,
	}
	seen := map[int64]bool{}
	rows := make([]distributionRow, 0, len(qs))
	for _, q := range qs {
		v := h.ValueAtQuantile(q)
		if seen[v] && q != 1 {
			continue
		}
		seen[v] = true
		rows = append(rows, distributionRow{Quantile: q, ValueUS: v})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Quantile < rows[j].Quantile })
	return rows
}
