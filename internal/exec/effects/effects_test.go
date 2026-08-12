package effects

import (
	"testing"

	"github.com/google/uuid"
)

// TestKeyStableAndDistinct pins the idempotency-key contract: pure
// function of (run, step) — identical on every call, distinct across
// steps and runs, and a parseable UUID (opaque, fixed-length, header-safe
// for M8's Idempotency-Key use).
func TestKeyStableAndDistinct(t *testing.T) {
	t.Parallel()
	runA, runB := uuid.New(), uuid.New()

	k := Key(runA, "step-1")
	if k != Key(runA, "step-1") {
		t.Errorf("Key not deterministic: %q vs %q", k, Key(runA, "step-1"))
	}
	if k == Key(runA, "step-2") {
		t.Error("Key collides across steps of one run")
	}
	if k == Key(runB, "step-1") {
		t.Error("Key collides across runs for one step ID")
	}
	if _, err := uuid.Parse(k); err != nil {
		t.Errorf("Key %q is not a UUID: %v", k, err)
	}
}

// TestKeyGolden pins the derivation itself: namespace and input encoding
// must never change, or every in-flight run gets a new key and external
// dedupe breaks. If this test fails, the change is wrong — do not update
// the expected value.
func TestKeyGolden(t *testing.T) {
	t.Parallel()
	runID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	const want = "4984e495-6462-54a3-8099-27373e43f666"
	if got := Key(runID, "a"); got != want {
		t.Errorf("Key(%s, \"a\") = %q, want pinned %q", runID, got, want)
	}
}
