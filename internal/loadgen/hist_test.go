package loadgen

import (
	"math"
	"testing"
	"time"
)

func TestHistogramBasics(t *testing.T) {
	h := NewHistogram(1, 0.01)
	if h.Count() != 0 || h.ValueAtQuantile(0.5) != 0 {
		t.Fatal("empty histogram must report zero")
	}
	for i := 1; i <= 1000; i++ {
		h.Record(int64(i))
	}
	if h.Count() != 1000 {
		t.Fatalf("count = %d, want 1000", h.Count())
	}
	// p50 near 500, within 1% bucket error.
	p50 := h.ValueAtQuantile(0.5)
	if p50 < 495 || p50 > 510 {
		t.Errorf("p50 = %d, want ~500", p50)
	}
	// Exact min/max/mean.
	if h.Min() != 1 || h.Max() != 1000 {
		t.Errorf("min/max = %d/%d, want 1/1000", h.Min(), h.Max())
	}
	if m := h.Mean(); math.Abs(m-500.5) > 0.01 {
		t.Errorf("mean = %g, want 500.5", m)
	}
}

func TestHistogramRelativeError(t *testing.T) {
	h := NewHistogram(1, 0.01)
	// Everything equals 1_000_000µs; the reported quantile must be within 1%.
	for i := 0; i < 100; i++ {
		h.RecordDuration(time.Second)
	}
	got := h.ValueAtQuantile(0.99)
	want := int64(1_000_000)
	if float64(got) < float64(want)*0.99 || float64(got) > float64(want)*1.01 {
		t.Errorf("p99 = %d, want within 1%% of %d", got, want)
	}
}

func TestHistogramDistribution(t *testing.T) {
	h := NewHistogram(1, 0.02)
	for i := 1; i <= 500; i++ {
		h.Record(int64(i * 100))
	}
	rows := h.distribution()
	if len(rows) == 0 {
		t.Fatal("distribution empty")
	}
	// Ascending in both quantile and value.
	for i := 1; i < len(rows); i++ {
		if rows[i].Quantile < rows[i-1].Quantile || rows[i].ValueUS < rows[i-1].ValueUS {
			t.Fatalf("distribution not monotonic at %d: %+v", i, rows[i])
		}
	}
}
