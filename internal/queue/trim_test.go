package queue_test

import (
	"testing"

	"github.com/mathcslearner/agentloom/internal/queue"
)

// TestSuccessorStreamID pins the XTRIM MINID threshold arithmetic: the
// successor is the smallest ID strictly greater than the input, so
// trimming at it removes the input entry while keeping everything after.
func TestSuccessorStreamID(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"5-3":                    "5-4",
		"0-0":                    "0-1",
		"1700000000000-41":       "1700000000000-42",
		"7-18446744073709551615": "8-0", // sequence overflow rolls the timestamp
	}
	for id, want := range cases {
		got, err := queue.SuccessorStreamID(id)
		if err != nil {
			t.Errorf("SuccessorStreamID(%q): unexpected error: %v", id, err)
			continue
		}
		if got != want {
			t.Errorf("SuccessorStreamID(%q) = %q, want %q", id, got, want)
		}
	}

	for _, id := range []string{"", "5", "abc", "5-x", "x-3"} {
		if _, err := queue.SuccessorStreamID(id); err == nil {
			t.Errorf("SuccessorStreamID(%q): want error, got nil", id)
		}
	}
}
