package engine

import (
	"testing"
	"time"
)

// TestOutputHash covers the no-progress guard's hash primitive (ticket 14.4):
// it is stable under JSON whitespace/key-order differences (canonicalization),
// resolves a pointer into the output, distinguishes different values, and
// errors on an unresolvable pointer (which the guard treats as skip).
func TestOutputHash(t *testing.T) {
	t.Parallel()

	a, err := outputHash([]byte(`{"text":"hello",  "n": 1}`), "")
	if err != nil {
		t.Fatalf("outputHash: %v", err)
	}
	// Same logical value, different whitespace and key order → identical hash.
	b, err := outputHash([]byte(`{"n":1,"text":"hello"}`), "")
	if err != nil {
		t.Fatalf("outputHash: %v", err)
	}
	if a != b {
		t.Errorf("canonicalization failed: %q != %q", a, b)
	}

	// A different value → a different hash.
	c, err := outputHash([]byte(`{"text":"world","n":1}`), "")
	if err != nil {
		t.Fatalf("outputHash: %v", err)
	}
	if a == c {
		t.Errorf("distinct outputs hashed equal: %q", a)
	}

	// Pointer selects a sub-value: two outputs differing only outside /text
	// hash equal at /text.
	p1, err := outputHash([]byte(`{"text":"same","meta":1}`), "/text")
	if err != nil {
		t.Fatalf("outputHash: %v", err)
	}
	p2, err := outputHash([]byte(`{"text":"same","meta":2}`), "/text")
	if err != nil {
		t.Fatalf("outputHash: %v", err)
	}
	if p1 != p2 {
		t.Errorf("pointer selection failed: %q != %q", p1, p2)
	}

	// An unresolvable pointer errors (the guard skips on this).
	if _, err := outputHash([]byte(`{"text":"x"}`), "/missing"); err == nil {
		t.Error("outputHash: want error for a missing pointer, got nil")
	}
}

// TestLoopInstanceID pins the authored/instance id mapping (ticket 14.4):
// iteration 0 is the authored id, k>=1 is "<id>#k".
func TestLoopInstanceID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		id   string
		iter int
		want string
	}{
		{"writer", 0, "writer"},
		{"writer", 1, "writer#1"},
		{"writer", 3, "writer#3"},
		{"draft", -1, "draft"},
	}
	for _, tc := range cases {
		if got := loopInstanceID(tc.id, tc.iter); got != tc.want {
			t.Errorf("loopInstanceID(%q, %d) = %q, want %q", tc.id, tc.iter, got, tc.want)
		}
	}
}

// TestDeadlineGuardEvent covers the wall-clock guard event's current/cap
// seconds (ticket 14.4): elapsed since start vs the configured deadline window.
func TestDeadlineGuardEvent(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	deadline := start.Add(60 * time.Second)
	now := start.Add(75 * time.Second) // 15s overdue

	ev := deadlineGuardEvent(start, deadline, now)
	if ev.Guard != "max_wall_clock" || ev.Unit != "seconds" || ev.Action != "cancel" {
		t.Errorf("event framing = %+v", ev)
	}
	if ev.Current != 75 {
		t.Errorf("current = %d, want 75", ev.Current)
	}
	if ev.Cap != 60 {
		t.Errorf("cap = %d, want 60", ev.Cap)
	}

	// A zero start (a deadline-exceeded run that never started, defensive)
	// reports zeros rather than a nonsensical huge cap.
	zero := deadlineGuardEvent(time.Time{}, deadline, now)
	if zero.Current != 0 || zero.Cap != 0 {
		t.Errorf("zero-start event = %+v, want zero current/cap", zero)
	}
}

// TestCapBreachUnit pins the expansion-cap unit mapping (ticket 14.4).
func TestCapBreachUnit(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"max_added_steps": "steps",
		"max_total_steps": "steps",
		"max_expansions":  "expansions",
		"max_depth":       "depth",
	}
	for limit, want := range cases {
		if got := capBreachUnit(limit); got != want {
			t.Errorf("capBreachUnit(%q) = %q, want %q", limit, got, want)
		}
	}
}
