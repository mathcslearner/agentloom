//go:build integration

package api_test

// Ticket 16.4 DoD-2: 100 concurrent firehose clients tailing a continuous run
// load. It measures that (a) every client keeps up — no client is closed 4001
// (the slow-close metric stays 0) and end-to-end commit→receipt latency stays
// within a local budget — and (b) the API's REST path does not degrade beyond a
// budget with 100 WS clients attached versus none. It also asserts the
// connection/frame/backpressure metrics are exported.
//
// Run with: make test-firehose-load  (heavier than the rest of the suite).

import (
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/event"
	"github.com/mathcslearner/agentloom/internal/obs/metrics"
)

func TestFirehoseHundredClients(t *testing.T) {
	t.Parallel()
	parent := t.Context()
	reg := prometheus.NewRegistry()
	am := metrics.NewAPIMetrics(reg)
	_, srv, rootKey := wsFleet(t, api.WSOptions{}, api.WithRequestMetrics(am))

	const (
		clients = 100
		runs    = 40
	)

	// Baseline REST latency (no WS clients attached yet).
	baselineP95 := probeRESTP95(t, srv.URL, rootKey, 30)

	// Open the client fleet with a filter mix; each records commit→receipt
	// latency for the event frames it sees until stopped.
	var (
		mu     sync.Mutex
		lat    []time.Duration
		events int
	)
	record := func(d time.Duration) {
		mu.Lock()
		lat = append(lat, d)
		events++
		mu.Unlock()
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	conns := make([]*websocket.Conn, 0, clients)
	for i := 0; i < clients; i++ {
		filter := api.WSFilter{}
		switch i % 3 {
		case 1:
			filter = api.WSFilter{Types: []string{string(event.TypeRunSucceeded)}}
		case 2:
			filter = api.WSFilter{DefinitionName: "load-flow"}
		}
		c := dialFirehose(parent, t, srv, rootKey)
		conns = append(conns, c)
		subscribeAndAck(parent, t, c, "s", filter)
		wg.Add(1)
		go func(c *websocket.Conn) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				f, ev, err := fhReadFrame(parent, c)
				if err != nil {
					return // connection closed at teardown, or peer gone
				}
				if f.Type == api.WSFrameEvent && !ev.Ts.IsZero() {
					record(time.Since(ev.Ts))
				}
			}
		}(c)
	}

	// Submit small runs continuously while a REST probe runs concurrently.
	var probeP95 time.Duration
	var probeWG sync.WaitGroup
	probeWG.Add(1)
	go func() {
		defer probeWG.Done()
		probeP95 = probeRESTP95(t, srv.URL, rootKey, 60)
	}()

	lastRun := ""
	for i := 0; i < runs; i++ {
		var sub api.SubmitRunResponse
		if status := postJSON(t, srv, rootKey, submitBody(t, firehoseDef("load-flow"), `{}`), &sub); status != http.StatusCreated {
			t.Fatalf("submit %d = %d", i, status)
		}
		lastRun = sub.RunID
		time.Sleep(25 * time.Millisecond)
	}
	probeWG.Wait()
	if lastRun != "" {
		waitRunTerminalAPI(t, srv, rootKey, lastRun)
	}
	// Let the tail settle, then stop the readers and close the connections.
	time.Sleep(2 * time.Second)
	close(stop)
	for _, c := range conns {
		_ = c.Close(websocket.StatusNormalClosure, "")
	}
	wg.Wait()

	// No client fell behind: the slow-close metric stays zero.
	if v := sampleMetric(t, reg, "engine_api_ws_slow_closes_total", map[string]string{"kind": "firehose"}); v != 0 {
		t.Errorf("ws_slow_closes_total{firehose} = %v, want 0 — a client fell behind", v)
	}
	if v := sampleMetric(t, reg, "engine_api_ws_frames_sent_total", map[string]string{"kind": "firehose"}); v < 1 {
		t.Errorf("ws_frames_sent_total{firehose} = %v, want >= 1", v)
	}

	mu.Lock()
	gotEvents := events
	p95 := percentile(lat, 0.95)
	mu.Unlock()
	if gotEvents == 0 {
		t.Fatal("no events were delivered to any client")
	}
	t.Logf("delivered %d event frames across %d clients; commit→receipt p95 = %v; REST p95 baseline=%v under-load=%v",
		gotEvents, clients, p95, baselineP95, probeP95)

	// Latency budget (local integration): p95 commit→receipt under 100 clients.
	if p95 > 5*time.Second {
		t.Errorf("commit→receipt p95 = %v, over the 5s budget", p95)
	}
	// REST degradation budget: under-load p95 within 6x baseline + a 300ms floor.
	budget := baselineP95*6 + 300*time.Millisecond
	if probeP95 > budget {
		t.Errorf("REST p95 under load = %v, over budget %v (baseline %v)", probeP95, budget, baselineP95)
	}
}

// probeRESTP95 issues n sequential GET /v1/runs requests and returns the p95
// latency — a per-request latency probe, not a throughput probe.
func probeRESTP95(t *testing.T, baseURL, rootKey string, n int) time.Duration {
	t.Helper()
	lat := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/runs?limit=10", nil)
		if err != nil {
			t.Fatalf("probe request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+rootKey)
		start := time.Now()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("probe GET /v1/runs: %v", err)
		}
		_ = resp.Body.Close()
		lat = append(lat, time.Since(start))
	}
	return percentile(lat, 0.95)
}

// percentile returns the p-quantile of durations (p in [0,1]); zero for empty.
func percentile(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := int(float64(len(s)-1) * p)
	return s[idx]
}
