//go:build integration

package loadgen_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/event/pubsub"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/loadgen"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/retrieval"
	"github.com/mathcslearner/agentloom/internal/retrieval/pgfts"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
	"github.com/mathcslearner/agentloom/internal/tools"
)

type fhSub struct{ sub *pubsub.Subscriber }

func (w fhSub) SubscribeRun(ctx context.Context, runID uuid.UUID) (api.WSEventStream, error) {
	return w.sub.SubscribeRun(ctx, runID)
}

func (w fhSub) SubscribeFirehose(ctx context.Context) (api.WSEventStream, error) {
	return w.sub.SubscribeFirehose(ctx)
}

func mintKey(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return "sk_" + base64.RawURLEncoding.EncodeToString(raw)
}

// TestLoadgenDryRunAgainstFleet drives the real in-process engine + firehose
// with a 100-run linear-10 campaign in firehose track mode. It is the
// integration twin of the httptest unit test: it proves the generator's
// firehose lifecycle tracking and scheduling-latency sampling work end-to-end
// against the real API, and that no run is lost.
func TestLoadgenDryRunAgainstFleet(t *testing.T) {
	ctx := t.Context()
	h := queuetest.New(t)
	prefix := "agentloom-loadgen-" + uuid.NewString()
	publisher := pubsub.NewPublisher(h.Client(), pubsub.Options{Prefix: prefix})
	t.Cleanup(func() { _ = publisher.Close(context.Background()) })
	s := store.NewFromPool(storetest.NewDB(t), store.WithEventSink(publisher))
	h.EnsureGroup(ctx)

	rootKey := mintKey(t)
	wsOpts := api.WSOptions{ //nolint:gosec // G101: test ticket-signing secret, not a credential
		TicketSecret: "loadgen-integration-secret",
		Subscriber:   fhSub{sub: pubsub.NewSubscriber(h.Client(), prefix, nil)},
	}
	handler, err := api.New(s, time.Now, nil, rootKey, api.RateLimitOptions{}, api.WithWebSocket(wsOpts))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	d, err := engine.NewDispatcher(s, h.Queue(), engine.DispatcherConfig{Interval: 10 * time.Millisecond, Batch: 32})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	dctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() { defer close(done); d.Run(dctx) }()

	providers, err := llm.NewRegistryFromKeys(llm.ProviderKeys{Mock: &llm.MockConfig{}})
	if err != nil {
		t.Fatalf("providers: %v", err)
	}
	toolReg, err := tools.NewBuiltins(tools.HTTPOptions{})
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	retrievers, err := retrieval.NewRegistry(pgfts.New(s))
	if err != nil {
		t.Fatalf("retrievers: %v", err)
	}
	for _, name := range []string{"lg-a", "lg-b", "lg-c"} {
		eng, err := engine.New(s, exec.Builtins(providers, toolReg, retrievers), name, engine.WithDispatchNudge(d.Nudge))
		if err != nil {
			t.Fatalf("engine.New: %v", err)
		}
		h.Spawn(name, eng.Handle, queue.ConsumerConfig{Block: 200 * time.Millisecond, Batch: 4})
	}

	cfg := loadgen.Config{
		APIBase: srv.URL, APIKey: rootKey, ScenarioDir: "../../test/load/scenarios",
		Scenario: "linear-10", Track: loadgen.TrackFirehose, SchedSample: 1.0,
		RateOverride: 25, MaxRuns: 100, WarmupOverride: 1,
		OutDir: t.TempDir(), PollInterval: 250 * time.Millisecond, PollAfter: 3 * time.Second,
		Progress: time.Second, DrainTimeout: 60 * time.Second, RunTimeout: 90 * time.Second,
	}
	rep, err := loadgen.Run(ctx, cfg, nil, io.Discard)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if rep.Counts["run_succeeded"] != 100 {
		t.Errorf("succeeded = %d, want 100 (taxonomy %+v)", rep.Counts["run_succeeded"], rep.Counts)
	}
	if rep.Integrity.LostRuns != 0 {
		t.Errorf("lost runs = %d, want 0", rep.Integrity.LostRuns)
	}
	if rep.Integrity.NonDeliberateDLQ != 0 {
		t.Errorf("non-deliberate DLQ = %d, want 0", rep.Integrity.NonDeliberateDLQ)
	}
	// Scheduling latency was sampled from the firehose step events.
	if rep.Latency["scheduling"].Count == 0 {
		t.Error("no scheduling-latency samples collected from the firehose")
	}
	if rep.Latency["end_to_end"].Count == 0 {
		t.Error("no end-to-end samples")
	}
	t.Logf("linear-10 x100: sched p50=%.1fms p99=%.1fms, e2e p50=%.1fms p99=%.1fms, throughput=%.1f/s, rate err=%.1f%%",
		float64(rep.Latency["scheduling"].P50)/1000, float64(rep.Latency["scheduling"].P99)/1000,
		float64(rep.Latency["end_to_end"].P50)/1000, float64(rep.Latency["end_to_end"].P99)/1000,
		rep.ThroughputPerSec, rep.Rate.RateErrorPct)
}
