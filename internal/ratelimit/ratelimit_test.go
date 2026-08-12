package ratelimit_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/ratelimit"
)

// TestAcquireValidation pins the Go-side parameter checks: every invalid
// bucket or cost is rejected before any Redis round trip (the limiter here
// has a nil client — reaching Redis would panic).
func TestAcquireValidation(t *testing.T) {
	t.Parallel()

	valid := ratelimit.Bucket{Key: "ratelimit:test", Capacity: 10, RefillPerSec: 1}
	cases := []struct {
		name    string
		bucket  ratelimit.Bucket
		cost    int64
		wantSub string // substring of the error message
		wantIs  error  // sentinel, when one applies
	}{
		{
			name:    "empty key",
			bucket:  ratelimit.Bucket{Capacity: 10, RefillPerSec: 1},
			cost:    1,
			wantSub: "key must be non-empty",
		},
		{
			name:    "zero capacity",
			bucket:  ratelimit.Bucket{Key: "k", RefillPerSec: 1},
			cost:    1,
			wantSub: "capacity must be positive",
		},
		{
			name:    "negative capacity",
			bucket:  ratelimit.Bucket{Key: "k", Capacity: -5, RefillPerSec: 1},
			cost:    1,
			wantSub: "capacity must be positive",
		},
		{
			name:    "negative rate",
			bucket:  ratelimit.Bucket{Key: "k", Capacity: 10, RefillPerSec: -1},
			cost:    1,
			wantSub: "refill rate must be finite and non-negative",
		},
		{
			name:    "NaN rate",
			bucket:  ratelimit.Bucket{Key: "k", Capacity: 10, RefillPerSec: math.NaN()},
			cost:    1,
			wantSub: "refill rate must be finite and non-negative",
		},
		{
			name:    "infinite rate",
			bucket:  ratelimit.Bucket{Key: "k", Capacity: 10, RefillPerSec: math.Inf(1)},
			cost:    1,
			wantSub: "refill rate must be finite and non-negative",
		},
		{
			name:    "zero cost",
			bucket:  valid,
			cost:    0,
			wantSub: "cost must be positive",
		},
		{
			name:    "negative cost",
			bucket:  valid,
			cost:    -3,
			wantSub: "cost must be positive",
		},
		{
			name:    "cost exceeds capacity",
			bucket:  valid,
			cost:    11,
			wantSub: "cost 11 > capacity 10",
			wantIs:  ratelimit.ErrCostExceedsCapacity,
		},
	}

	l := ratelimit.New(nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := l.Acquire(context.Background(), tc.bucket, tc.cost)
			if err == nil {
				t.Fatalf("Acquire(%+v, cost=%d): want error, got nil", tc.bucket, tc.cost)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err, tc.wantSub)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("errors.Is(%q, %v) = false, want true", err, tc.wantIs)
			}
		})
	}
}

// TestAcquireAtRequiresNow pins the injected-clock seam's zero-time guard.
func TestAcquireAtRequiresNow(t *testing.T) {
	t.Parallel()
	l := ratelimit.New(nil)
	b := ratelimit.Bucket{Key: "k", Capacity: 10, RefillPerSec: 1}
	_, _, err := l.AcquireAt(context.Background(), b, 1, time.Time{})
	if err == nil || !strings.Contains(err.Error(), "zero now") {
		t.Fatalf("AcquireAt with zero now: want zero-now error, got %v", err)
	}
}

// TestParseAcquireReply pins the reply decoder: the happy shapes and every
// malformed shape a broken script edit could produce.
func TestParseAcquireReply(t *testing.T) {
	t.Parallel()

	t.Run("allowed", func(t *testing.T) {
		t.Parallel()
		res, tokens, err := ratelimit.ParseAcquireReply([]any{int64(1), "7.5", int64(0)})
		if err != nil {
			t.Fatalf("ParseAcquireReply: %v", err)
		}
		if !res.Allowed || res.Remaining != 7 || res.RetryAfter != 0 || tokens != 7.5 {
			t.Errorf("got %+v tokens=%v, want allowed remaining=7 retry=0 tokens=7.5", res, tokens)
		}
	})

	t.Run("denied with retry", func(t *testing.T) {
		t.Parallel()
		res, tokens, err := ratelimit.ParseAcquireReply([]any{int64(0), "0.25", int64(1_500_000)})
		if err != nil {
			t.Fatalf("ParseAcquireReply: %v", err)
		}
		if res.Allowed || res.Remaining != 0 || res.RetryAfter != 1500*time.Millisecond || tokens != 0.25 {
			t.Errorf("got %+v tokens=%v, want denied remaining=0 retry=1.5s tokens=0.25", res, tokens)
		}
	})

	t.Run("denied never", func(t *testing.T) {
		t.Parallel()
		res, _, err := ratelimit.ParseAcquireReply([]any{int64(0), "2", int64(-1)})
		if err != nil {
			t.Fatalf("ParseAcquireReply: %v", err)
		}
		if res.Allowed || res.RetryAfter != ratelimit.RetryAfterNever {
			t.Errorf("got %+v, want denied with RetryAfterNever", res)
		}
	})

	malformed := []struct {
		name string
		raw  any
	}{
		{"not a slice", "nope"},
		{"wrong length", []any{int64(1), "2"}},
		{"non-int allowed", []any{"1", "2", int64(0)}},
		{"non-string tokens", []any{int64(1), int64(2), int64(0)}},
		{"unparseable tokens", []any{int64(1), "not-a-float", int64(0)}},
		{"non-int retry", []any{int64(1), "2", "0"}},
	}
	for _, tc := range malformed {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := ratelimit.ParseAcquireReply(tc.raw); err == nil {
				t.Errorf("ParseAcquireReply(%v): want error, got nil", tc.raw)
			}
		})
	}
}
