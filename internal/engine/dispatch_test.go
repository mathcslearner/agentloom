package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/store"
)

// enqueueFunc adapts a function to the Enqueuer seam.
type enqueueFunc func(ctx context.Context, env queue.Envelope) (string, error)

func (f enqueueFunc) Enqueue(ctx context.Context, env queue.Envelope) (string, error) {
	return f(ctx, env)
}

// noopEnqueuer never enqueues; construction-only tests.
var noopEnqueuer = enqueueFunc(func(context.Context, queue.Envelope) (string, error) {
	return "0-0", nil
})

// unqueriedStore is a non-nil *store.Store for construction-only tests;
// touching the database through it would panic on the nil pool.
func unqueriedStore() *store.Store { return store.NewFromPool(nil) }

func validDispatcherConfig() DispatcherConfig {
	return DispatcherConfig{Interval: time.Second, Batch: 16}
}

func TestNewDispatcherValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		store   *store.Store
		enq     Enqueuer
		cfg     DispatcherConfig
		wantErr string
	}{
		{"nil store", nil, noopEnqueuer, validDispatcherConfig(), "requires a store"},
		{"nil enqueuer", unqueriedStore(), nil, validDispatcherConfig(), "requires an enqueuer"},
		{"zero interval", unqueriedStore(), noopEnqueuer, DispatcherConfig{Batch: 16}, "positive Interval"},
		{"zero batch", unqueriedStore(), noopEnqueuer, DispatcherConfig{Interval: time.Second}, "positive Batch"},
		{"negative batch", unqueriedStore(), noopEnqueuer, DispatcherConfig{Interval: time.Second, Batch: -1}, "positive Batch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewDispatcher(tc.store, tc.enq, tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("NewDispatcher error = %v, want mention of %q", err, tc.wantErr)
			}
		})
	}

	if _, err := NewDispatcher(unqueriedStore(), noopEnqueuer, validDispatcherConfig()); err != nil {
		t.Errorf("NewDispatcher with valid inputs: unexpected error %v", err)
	}
}

// TestNudgeNeverBlocks is the WithDispatchNudge contract: Nudge must be
// safe for concurrent calls and must not block — even with no Run loop
// draining the wake channel.
func TestNudgeNeverBlocks(t *testing.T) {
	t.Parallel()

	d, err := NewDispatcher(unqueriedStore(), noopEnqueuer, validDispatcherConfig())
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 100 {
					d.Nudge()
				}
			}()
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Nudge blocked with no consumer on the wake channel")
	}
}

func validReconcilerConfig() ReconcilerConfig {
	return ReconcilerConfig{
		Interval:     30 * time.Second,
		ReadyStale:   time.Minute,
		RunningStale: 5 * time.Minute,
		RetryStale:   time.Minute,
		Limit:        256,
	}
}

func TestNewReconcilerValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		store   *store.Store
		mutate  func(*ReconcilerConfig)
		wantErr string
	}{
		{"nil store", nil, func(*ReconcilerConfig) {}, "requires a store"},
		{"zero interval", unqueriedStore(), func(c *ReconcilerConfig) { c.Interval = 0 }, "positive Interval"},
		{"zero ready stale", unqueriedStore(), func(c *ReconcilerConfig) { c.ReadyStale = 0 }, "positive ReadyStale"},
		{"zero running stale", unqueriedStore(), func(c *ReconcilerConfig) { c.RunningStale = 0 }, "positive RunningStale"},
		{"zero retry stale", unqueriedStore(), func(c *ReconcilerConfig) { c.RetryStale = 0 }, "positive RetryStale"},
		{"zero limit", unqueriedStore(), func(c *ReconcilerConfig) { c.Limit = 0 }, "positive Limit"},
		{"negative limit", unqueriedStore(), func(c *ReconcilerConfig) { c.Limit = -5 }, "positive Limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := validReconcilerConfig()
			tc.mutate(&cfg)
			_, err := NewReconciler(tc.store, cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("NewReconciler error = %v, want mention of %q", err, tc.wantErr)
			}
		})
	}

	if _, err := NewReconciler(unqueriedStore(), validReconcilerConfig()); err != nil {
		t.Errorf("NewReconciler with valid inputs: unexpected error %v", err)
	}
}

// TestReconcilerJitterBounds pins the sweep spacing to Interval ±20%.
func TestReconcilerJitterBounds(t *testing.T) {
	t.Parallel()

	r, err := NewReconciler(unqueriedStore(), validReconcilerConfig())
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	lo := time.Duration(float64(r.cfg.Interval) * 0.8)
	hi := time.Duration(float64(r.cfg.Interval) * 1.2)
	for range 1000 {
		if d := r.jittered(); d < lo || d > hi {
			t.Fatalf("jittered() = %v, want within [%v, %v]", d, lo, hi)
		}
	}
}

// TestRecoverPassContainsPanic (post-M4 audit): a panic in a background
// duty pass becomes an error for the loop's log-and-retry path — never a
// worker-killing crash — while normal results pass through untouched.
func TestRecoverPassContainsPanic(t *testing.T) {
	t.Parallel()

	if err := recoverPass(func() error { panic("pass bug") }); err == nil ||
		!strings.Contains(err.Error(), "background pass panic: pass bug") {
		t.Errorf("recoverPass(panic) = %v, want the contained panic with its message", err)
	}
	if err := recoverPass(func() error { return nil }); err != nil {
		t.Errorf("recoverPass(nil) = %v, want nil", err)
	}
	want := errors.New("ordinary failure")
	if err := recoverPass(func() error { return want }); !errors.Is(err, want) {
		t.Errorf("recoverPass(err) = %v, want the error passed through", err)
	}
}
