//go:build integration

package ratelimit_test

import (
	"context"
	"testing"

	"github.com/mathcslearner/agentloom/internal/ratelimit"
)

// BenchmarkAcquire is acceptance criterion 3: the acquire-latency
// benchmark against a local Redis (<1ms target; measured numbers live in
// the 6.3 progress-log entry). The bucket is sized so every acquire is a
// grant — the grant and deny paths do identical work, and grants are the
// steady state being budgeted for.
//
// Run with:
//
//	go test -tags integration -bench BenchmarkAcquire -run xxx ./internal/ratelimit
func BenchmarkAcquire(b *testing.B) {
	ctx := context.Background()
	l, _, key := newTestLimiter(b)
	bucket := ratelimit.Bucket{Key: key, Capacity: 1_000_000_000, RefillPerSec: 1e9}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := l.Acquire(ctx, bucket, 1)
		if err != nil {
			b.Fatalf("Acquire: %v", err)
		}
		if !res.Allowed {
			b.Fatalf("Acquire denied at iteration %d — size the benchmark bucket up", i)
		}
	}
}

// BenchmarkAcquireParallel is the same acquire under GOMAXPROCS-parallel
// callers — closer to the 6.4 middleware's shape, where every in-flight
// request acquires concurrently.
func BenchmarkAcquireParallel(b *testing.B) {
	ctx := context.Background()
	l, _, key := newTestLimiter(b)
	bucket := ratelimit.Bucket{Key: key, Capacity: 1_000_000_000, RefillPerSec: 1e9}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			res, err := l.Acquire(ctx, bucket, 1)
			if err != nil {
				b.Errorf("Acquire: %v", err)
				return
			}
			if !res.Allowed {
				b.Errorf("Acquire denied — size the benchmark bucket up")
				return
			}
		}
	})
}
