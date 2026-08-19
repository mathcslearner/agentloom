//go:build integration

package api_test

// Ticket 16.3's run-WebSocket protocol suite (ADR-018). It drives the real
// endpoint over an httptest server, a sink-wired store, a 16.2 publisher, and a
// two-worker engine fleet:
//
//   - TestWSSnapshotBackfillResume (DoD-1): connect → snapshot → read some
//     events → drop the connection mid-stream → reconnect with last_seq → the
//     union of events across both connections is exactly seqs 1..N, no gaps or
//     dupes.
//   - TestWSTicketAuth (DoD-2): expired / wrong-run / tampered tickets and a
//     missing credential are rejected; a read bearer and a valid ticket connect;
//     an unknown run is 404; ws-ticket needs the read scope.
//   - TestWSSlowClientCloses (DoD-3): a client that stops draining a flooded
//     stream is closed with code 4001 and can reconnect.
//   - TestWSPollFallback: with no live subscriber wired, the poll path still
//     delivers the complete feed.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/event"
	"github.com/mathcslearner/agentloom/internal/event/pubsub"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/retrieval"
	"github.com/mathcslearner/agentloom/internal/retrieval/pgfts"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
	"github.com/mathcslearner/agentloom/internal/tools"
)

const wsTicketSecret = "integration-ws-secret"

// wsPubSubSubscriber adapts *pubsub.Subscriber to api.WSSubscriber (the same
// thin wrapper cmd/api uses — the api package never imports go-redis).
type wsPubSubSubscriber struct{ sub *pubsub.Subscriber }

func (w wsPubSubSubscriber) SubscribeRun(ctx context.Context, runID uuid.UUID) (api.WSEventStream, error) {
	return w.sub.SubscribeRun(ctx, runID)
}

// wsFrame is the client-side decode of any server frame (discriminated by Type).
type wsFrame struct {
	Type    string          `json:"type"`
	Run     json.RawMessage `json:"run"`
	Event   json.RawMessage `json:"event"`
	LastSeq int64           `json:"last_seq"`
}

// wsEventSeq extracts the seq from an "event" frame's envelope.
type wsEventSeq struct {
	Seq  int64  `json:"seq"`
	Type string `json:"type"`
}

// wsURL turns an httptest http(s) URL and a run/ticket/last_seq into the WS URL.
func wsURL(srvURL, runID, ticket string, lastSeq int64) string {
	u := strings.Replace(srvURL, "http", "ws", 1) + "/v1/runs/" + runID + "/ws"
	q := "?"
	if ticket != "" {
		u += q + "ticket=" + ticket
		q = "&"
	}
	if lastSeq > 0 {
		u += q + "last_seq=" + strconv.FormatInt(lastSeq, 10)
	}
	return u
}

// mintTicket calls POST /v1/runs/{id}/ws-ticket and returns the ticket.
func mintTicket(t *testing.T, srv *httptest.Server, bearer, runID string) string {
	t.Helper()
	var resp api.WSTicketResponse
	if status := doAuth(t, srv, http.MethodPost, "/v1/runs/"+runID+"/ws-ticket", bearer, nil, &resp).StatusCode; status != http.StatusOK {
		t.Fatalf("POST ws-ticket = %d, want 200", status)
	}
	if resp.Ticket == "" {
		t.Fatal("ws-ticket returned an empty ticket")
	}
	return resp.Ticket
}

// wsFleet wires a sink-published store + a two-worker engine fleet + an
// httptest server with the run WebSocket enabled over a real pub/sub subscriber.
func wsFleet(t *testing.T, wsOpts api.WSOptions) (*store.Store, *httptest.Server, string) {
	t.Helper()
	ctx := t.Context()
	h := queuetest.New(t)
	prefix := "agentloom-ws-" + uuid.NewString()
	publisher := pubsub.NewPublisher(h.Client(), pubsub.Options{Prefix: prefix})
	t.Cleanup(func() { _ = publisher.Close(context.Background()) })
	s := store.NewFromPool(storetest.NewDB(t), store.WithEventSink(publisher))
	h.EnsureGroup(ctx)

	if wsOpts.TicketSecret == "" {
		wsOpts.TicketSecret = wsTicketSecret
	}
	if wsOpts.Subscriber == nil {
		wsOpts.Subscriber = wsPubSubSubscriber{sub: pubsub.NewSubscriber(h.Client(), prefix, nil)}
	}
	rootKey := mintTestKey(t)
	handler, err := api.New(s, time.Now, nil, rootKey, api.RateLimitOptions{}, api.WithWebSocket(wsOpts))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	d, err := engine.NewDispatcher(s, h.Queue(), engine.DispatcherConfig{Interval: 10 * time.Millisecond, Batch: 16})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	dctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() { defer close(done); d.Run(dctx) }()

	providers, err := llm.NewRegistryFromKeys(llm.ProviderKeys{Mock: &llm.MockConfig{}})
	if err != nil {
		t.Fatalf("NewRegistryFromKeys: %v", err)
	}
	toolReg, err := tools.NewBuiltins(tools.HTTPOptions{})
	if err != nil {
		t.Fatalf("tools.NewBuiltins: %v", err)
	}
	retrievers, err := retrieval.NewRegistry(pgfts.New(s))
	if err != nil {
		t.Fatalf("retrieval.NewRegistry: %v", err)
	}
	for _, name := range []string{"ws-a", "ws-b"} {
		eng, err := engine.New(s, exec.Builtins(providers, toolReg, retrievers), name, engine.WithDispatchNudge(d.Nudge))
		if err != nil {
			t.Fatalf("engine.New: %v", err)
		}
		h.Spawn(name, eng.Handle, queue.ConsumerConfig{Block: 500 * time.Millisecond, Batch: 1})
	}
	return s, srv, rootKey
}

func TestWSSnapshotBackfillResume(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, srv, rootKey := wsFleet(t, api.WSOptions{})

	var sub api.SubmitRunResponse
	if status := postJSON(t, srv, rootKey, submitBody(t, fanoutJSON(t), `{"topic":"ws"}`), &sub); status != http.StatusCreated {
		t.Fatalf("submit = %d, want 201", status)
	}

	// Connection 1: connect immediately, read the snapshot + a few events, then
	// drop the connection while the run is still producing.
	ticket := mintTicket(t, srv, rootKey, sub.RunID)
	seen := map[int64]bool{}
	var maxSeq int64

	c1, _, err := websocket.Dial(ctx, wsURL(srv.URL, sub.RunID, ticket, 0), nil)
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	sawSnapshot := false
	// Read up to a handful of event frames, then close.
	for got := 0; got < 3; {
		typ, ev, err := readWSFrame(ctx, c1)
		if err != nil {
			break // the run may finish quickly; that is fine, conn 2 backfills
		}
		switch typ {
		case api.WSFrameSnapshot:
			sawSnapshot = true
		case api.WSFrameEvent:
			if seen[ev.Seq] {
				t.Fatalf("conn1 delivered duplicate seq %d", ev.Seq)
			}
			seen[ev.Seq] = true
			if ev.Seq > maxSeq {
				maxSeq = ev.Seq
			}
			got++
		}
	}
	_ = c1.Close(websocket.StatusNormalClosure, "")
	if !sawSnapshot {
		t.Fatal("conn1 never received the snapshot frame first")
	}

	// Connection 2: resume from maxSeq; collect every remaining event until the
	// run reaches a terminal event.
	ticket2 := mintTicket(t, srv, rootKey, sub.RunID)
	c2, _, err := websocket.Dial(ctx, wsURL(srv.URL, sub.RunID, ticket2, maxSeq), nil)
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	defer func() { _ = c2.Close(websocket.StatusNormalClosure, "") }()

	drainToHead(t, c2, seen, maxSeq)

	// The union across both connections is exactly seqs 1..N (the DB head), no
	// gaps, no dupes.
	rows, err := s.Events().List(ctx, uuid.MustParse(sub.RunID), 0, 10000)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(seen) != len(rows) {
		t.Fatalf("saw %d distinct event seqs, DB head has %d", len(seen), len(rows))
	}
	for i := 1; i <= len(rows); i++ {
		if !seen[int64(i)] {
			t.Fatalf("missing seq %d in the assembled feed", i)
		}
	}
}

func TestWSTicketAuth(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, srv, rootKey := wsFleet(t, api.WSOptions{})

	var sub api.SubmitRunResponse
	if status := postJSON(t, srv, rootKey, submitBody(t, fanoutJSON(t), ``), &sub); status != http.StatusCreated {
		t.Fatalf("submit = %d, want 201", status)
	}
	_ = s

	// ws-ticket needs the read scope: a submit-only key is 403.
	submitOnly := mintScopedKey(t, srv, rootKey, "submit")
	if res := doAuth(t, srv, http.MethodPost, "/v1/runs/"+sub.RunID+"/ws-ticket", submitOnly, nil, nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("ws-ticket with submit-only key = %d, want 403", res.StatusCode)
	}
	// Unknown run is 404.
	if res := doAuth(t, srv, http.MethodPost, "/v1/runs/"+uuid.NewString()+"/ws-ticket", rootKey, nil, nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("ws-ticket for unknown run = %d, want 404", res.StatusCode)
	}

	// A valid ticket connects.
	ticket := mintTicket(t, srv, rootKey, sub.RunID)
	c, _, err := websocket.Dial(ctx, wsURL(srv.URL, sub.RunID, ticket, 0), nil)
	if err != nil {
		t.Fatalf("dial with valid ticket: %v", err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")

	// A read bearer connects too (the non-browser path). coder/websocket's
	// DialOptions carry the Authorization header.
	cb, _, err := websocket.Dial(ctx, wsURL(srv.URL, sub.RunID, "", 0), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + rootKey}},
	})
	if err != nil {
		t.Fatalf("dial with read bearer: %v", err)
	}
	_ = cb.Close(websocket.StatusNormalClosure, "")

	// Rejections: no credential, a ticket for another run, and a garbage
	// ticket all fail the handshake with 401.
	otherRun := uuid.NewString()
	for name, url := range map[string]string{
		"no credential":  wsURL(srv.URL, sub.RunID, "", 0),
		"garbage ticket": wsURL(srv.URL, sub.RunID, "not-a-ticket", 0),
		"wrong run":      wsURL(srv.URL, otherRun, ticket, 0),
	} {
		_, resp, err := websocket.Dial(ctx, url, nil)
		if err == nil {
			t.Errorf("%s: dial succeeded, want handshake failure", name)
			continue
		}
		if resp == nil || (resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusNotFound) {
			got := 0
			if resp != nil {
				got = resp.StatusCode
			}
			t.Errorf("%s: handshake status = %d, want 401/404", name, got)
		}
	}
}

func TestWSSlowClientCloses(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// A fake subscriber whose live channel the test drives, plus a tiny outbound
	// buffer and a short slow-client timeout, so a client that stops draining is
	// closed with 4001 deterministically without depending on TCP buffer sizes.
	feed := make(chan event.Envelope, 8192)
	s, srv, rootKey := wsFleet(t, api.WSOptions{
		Subscriber:        fakeWSSubscriber{ch: feed},
		Buffer:            1,
		SlowClientTimeout: time.Second,
		WriteTimeout:      30 * time.Second,
		PingInterval:      time.Hour,
		ResyncInterval:    time.Hour,
	})

	var sub api.SubmitRunResponse
	if status := postJSON(t, srv, rootKey, submitBody(t, oneStepDef, ``), &sub); status != http.StatusCreated {
		t.Fatalf("submit = %d, want 201", status)
	}
	runID := uuid.MustParse(sub.RunID)
	// Wait for the run to reach the DB head so the initial backfill is bounded.
	waitRunTerminalAPI(t, srv, rootKey, sub.RunID)

	ticket := mintTicket(t, srv, rootKey, sub.RunID)
	c, _, err := websocket.Dial(ctx, wsURL(srv.URL, sub.RunID, ticket, 0), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Read the snapshot and drain up to caught_up, then STOP reading.
	head := int64(0)
	for {
		typ, ev, err := readWSFrame(ctx, c)
		if err != nil {
			t.Fatalf("pre-flood read: %v", err)
		}
		if typ == api.WSFrameEvent && ev.Seq > head {
			head = ev.Seq
		}
		if typ == api.WSFrameCaughtUp {
			break
		}
	}

	// Flood the live path with in-order synthetic envelopes, then STOP reading:
	// a stalled client fills the bounded buffer and deliver trips the
	// slow-client timeout while the client is not draining.
	go func() {
		for i := int64(1); i <= 3000; i++ {
			select {
			case feed <- event.NewEnvelope(runID, head+i, time.Now(), event.StepReady{StepID: "flood"}):
			case <-ctx.Done():
				return
			}
		}
	}()
	// Let the buffer fill and the slow-client timeout trip while we do not read.
	time.Sleep(2 * time.Second)

	// Now resume reading: drain the backlog and observe the 4001 close frame
	// (the server holds its close handshake open long enough for a resuming
	// client to receive it).
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var closeCode websocket.StatusCode
	for {
		_, _, err := c.Read(readCtx)
		if err != nil {
			closeCode = websocket.CloseStatus(err)
			break
		}
	}
	if closeCode != wsSlowClose {
		t.Fatalf("close code = %d, want %d (slow consumer)", closeCode, wsSlowClose)
	}

	// The client can resume: a fresh connection re-handshakes cleanly.
	ticket2 := mintTicket(t, srv, rootKey, sub.RunID)
	c2, _, err := websocket.Dial(ctx, wsURL(srv.URL, sub.RunID, ticket2, head), nil)
	if err != nil {
		t.Fatalf("resume dial: %v", err)
	}
	defer func() { _ = c2.Close(websocket.StatusNormalClosure, "") }()
	sawCaughtUp := false
	rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
	defer rcancel()
	for !sawCaughtUp {
		typ, _, err := readWSFrame(rctx, c2)
		if err != nil {
			t.Fatalf("resume read: %v", err)
		}
		if typ == api.WSFrameCaughtUp {
			sawCaughtUp = true
		}
	}
	_ = s
}

func TestWSPollFallback(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	// No live subscriber: the driver must fall back to DB polling.
	s, srv, rootKey := wsFleet(t, api.WSOptions{
		Subscriber:   noSubscriber{},
		PollInterval: 50 * time.Millisecond,
	})

	var sub api.SubmitRunResponse
	if status := postJSON(t, srv, rootKey, submitBody(t, fanoutJSON(t), ``), &sub); status != http.StatusCreated {
		t.Fatalf("submit = %d, want 201", status)
	}
	ticket := mintTicket(t, srv, rootKey, sub.RunID)
	c, _, err := websocket.Dial(ctx, wsURL(srv.URL, sub.RunID, ticket, 0), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close(websocket.StatusNormalClosure, "") }()

	seen := map[int64]bool{}
	drainToHead(t, c, seen, 0)
	rows, err := s.Events().List(ctx, uuid.MustParse(sub.RunID), 0, 10000)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for i := 1; i <= len(rows); i++ {
		if !seen[int64(i)] {
			t.Fatalf("poll fallback missing seq %d of %d", i, len(rows))
		}
	}
}

// drainToHead reads event frames into seen until a terminal run event has been
// observed AND a subsequent read times out (so any events sequenced after the
// terminal one — e.g. a trailing cost_updated — are collected). resumeCursor is
// the last_seq the connection resumed from: no event at or below it may be
// re-delivered (the resume must not duplicate). It fails if no terminal event
// arrives within the overall budget.
func drainToHead(t *testing.T, c *websocket.Conn, seen map[int64]bool, resumeCursor int64) {
	t.Helper()
	overall := time.Now().Add(25 * time.Second)
	terminal := false
	for time.Now().Before(overall) {
		rctx, cancel := context.WithTimeout(context.Background(), time.Second)
		typ, ev, err := readWSFrame(rctx, c)
		cancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				if terminal {
					return // no further events after the terminal one
				}
				continue // run still producing; keep waiting
			}
			t.Fatalf("drain read: %v", err)
		}
		if typ != api.WSFrameEvent {
			continue
		}
		if ev.Seq <= resumeCursor {
			t.Fatalf("re-delivered seq %d at or below resume cursor %d", ev.Seq, resumeCursor)
		}
		if seen[ev.Seq] {
			t.Fatalf("duplicate seq %d delivered", ev.Seq)
		}
		seen[ev.Seq] = true
		if ev.Type == string(event.TypeRunSucceeded) || ev.Type == string(event.TypeRunFailed) {
			terminal = true
		}
	}
	if !terminal {
		t.Fatal("never observed a terminal run event")
	}
}

// wsSlowClose is the application close code the WS driver uses for a slow
// consumer (matches wsCloseSlowConsumer in ws.go).
const wsSlowClose = websocket.StatusCode(4001)

// readWSFrame reads one JSON text frame and decodes its discriminator + (for an
// event frame) the envelope seq/type.
func readWSFrame(ctx context.Context, c *websocket.Conn) (string, wsEventSeq, error) {
	_, data, err := c.Read(ctx)
	if err != nil {
		return "", wsEventSeq{}, err
	}
	var f wsFrame
	if err := json.Unmarshal(data, &f); err != nil {
		return "", wsEventSeq{}, err
	}
	if f.Type == api.WSFrameEvent {
		var ev wsEventSeq
		if err := json.Unmarshal(f.Event, &ev); err != nil {
			return "", wsEventSeq{}, err
		}
		return f.Type, ev, nil
	}
	return f.Type, wsEventSeq{}, nil
}

// fakeWSSubscriber feeds a test-controlled channel as a run's live stream.
type fakeWSSubscriber struct{ ch chan event.Envelope }

func (f fakeWSSubscriber) SubscribeRun(context.Context, uuid.UUID) (api.WSEventStream, error) {
	return fakeWSStream(f), nil
}

type fakeWSStream struct{ ch chan event.Envelope }

func (f fakeWSStream) Events() <-chan event.Envelope { return f.ch }
func (f fakeWSStream) Close() error                  { return nil }

// noSubscriber's SubscribeRun always fails, so the driver falls back to polling.
type noSubscriber struct{}

func (noSubscriber) SubscribeRun(context.Context, uuid.UUID) (api.WSEventStream, error) {
	return nil, errors.New("no live subscriber")
}

// mintScopedKey mints a key with exactly the named scopes via the admin API.
func mintScopedKey(t *testing.T, srv *httptest.Server, rootKey, scope string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"name": "ws-" + scope + "-key", "scopes": []string{scope}})
	var resp struct {
		Key string `json:"key"`
	}
	if res := doAuth(t, srv, http.MethodPost, "/v1/keys", rootKey, body, &resp); res.StatusCode != http.StatusCreated {
		t.Fatalf("minting %s key = %d, want 201", scope, res.StatusCode)
	}
	return resp.Key
}

// waitRunTerminalAPI polls GET /v1/runs/{id} until the run leaves running.
func waitRunTerminalAPI(t *testing.T, srv *httptest.Server, bearer, runID string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var run api.RunResponse
		if getJSON(t, srv, bearer, "/v1/runs/"+runID, &run) == http.StatusOK && run.Run.Status != store.RunStatusRunning {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("run never reached a terminal state")
}

// oneStepDef is a minimal single-noop run for the slow-client test's small feed.
var oneStepDef = []byte(`{"schema_version":1,"name":"ws-onestep","steps":[{"id":"a","type":"noop"}],"edges":[]}`)
