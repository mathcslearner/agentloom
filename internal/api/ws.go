package api

// The run WebSocket endpoint (ticket 16.3, ADR-018): GET /v1/runs/{id}/ws
// streams a run's live event feed. The protocol is snapshot → backfill →
// live-tail, all anchored on the durable per-run seq:
//
//  1. server sends one "snapshot" frame (the GET /v1/runs/{id} body),
//  2. backfills every event after the client's ?last_seq from Postgres,
//  3. sends a "caught_up" frame, then
//  4. live-tails: it subscribes to the run's Redis pub/sub channel (16.2) and
//     forwards each envelope through a pubsub.Tailer, which dedupes by seq and
//     heals any missed/out-of-order message with a DB backfill.
//
// Delivery is exactly the at-least-once contract of ADR-018: the client dedupes
// and orders by (run_id, seq) and resumes after a disconnect by reconnecting
// with ?last_seq=<highest seq seen>. Recovery is deterministic because every
// mechanism reduces to "read rows after last_seq".
//
// Auth is a short-lived signed ticket (ws_ticket.go) or a read-scoped bearer
// key; see requireReadOrTicket. Liveness is a periodic ping. A client that
// stops draining fills a bounded per-connection buffer and is closed with the
// application close code 4001 ("slow consumer"); it resumes with its last_seq.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/event"
	"github.com/mathcslearner/agentloom/internal/event/pubsub"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store"
)

// wsCloseSlowConsumer is the application close code for a client that could not
// keep up with its event stream (ADR-018). It is in the private-use 4000–4999
// range so it never collides with a protocol code, and it is documented as
// resumable: the client reconnects with its last_seq and misses nothing.
const wsCloseSlowConsumer = 4001

// WS driver defaults. All are overridable via WSOptions so tests can drive the
// slow-client path on a short timeout.
const (
	defaultWSBuffer            = 256
	defaultWSPingInterval      = 30 * time.Second
	defaultWSPollInterval      = time.Second
	defaultWSResyncInterval    = 10 * time.Second
	defaultWSSlowClientTimeout = 10 * time.Second
	defaultWSWriteTimeout      = 10 * time.Second
	// defaultWSTicketTTL is the ticket validity when WithWebSocket is wired with
	// a secret but no TTL. cmd/api always passes the config value; this keeps a
	// programmatic caller from minting already-expired tickets (ttl 0).
	defaultWSTicketTTL = time.Minute
	// wsBackfillPageSize bounds one backfill page (the Tailer default is fine;
	// named here for the doc).
	wsBackfillPageSize = pubsub.DefaultBackfillPageSize
)

// WSEventStream is one live subscription's decoded envelope channel — the
// surface the WS driver consumes. *pubsub.Subscription satisfies it.
type WSEventStream interface {
	// Events yields decoded envelopes in Redis arrival order (the Tailer
	// imposes seq order). It closes when the subscription is Closed.
	Events() <-chan event.Envelope
	// Close releases the subscription.
	Close() error
}

// WSSubscriber opens a per-run live subscription. *pubsub.Subscriber satisfies
// it via a thin cmd/api adapter (the api package never imports go-redis — the
// same discipline as CacheOps). Nil means no live path: the driver falls back
// to periodic DB polling, so the stream still works (just at poll latency) when
// pub/sub is disabled or Redis is down.
type WSSubscriber interface {
	SubscribeRun(ctx context.Context, runID uuid.UUID) (WSEventStream, error)
}

// WSOptions configures the run WebSocket endpoint (ticket 16.3). The zero value
// is not enough — WithWebSocket requires a signing secret — but every timing
// field falls back to its documented default.
type WSOptions struct {
	// Subscriber opens the live pub/sub subscription. Nil ⇒ poll-only.
	Subscriber WSSubscriber
	// TicketSecret signs and verifies WS tickets. Required (WithWebSocket
	// rejects an empty secret; cmd/api generates a random one when the
	// operator sets none).
	TicketSecret string
	// TicketTTL bounds a minted ticket's validity.
	TicketTTL time.Duration
	// Buffer is the per-connection outbound frame buffer; a client that keeps
	// it full for SlowClientTimeout is closed 4001. Default defaultWSBuffer.
	Buffer int
	// PingInterval is the keepalive ping period. Default defaultWSPingInterval.
	PingInterval time.Duration
	// PollInterval is the DB re-read period when there is no live subscriber
	// (the poll fallback). Default defaultWSPollInterval.
	PollInterval time.Duration
	// ResyncInterval is a slow safety backfill while live-tailing, healing the
	// rare event whose publish was lost with no later event to trigger a gap
	// (the 16.2 "crash between commit and publish" residual). Default
	// defaultWSResyncInterval.
	ResyncInterval time.Duration
	// SlowClientTimeout bounds how long deliver waits to enqueue one frame
	// before declaring the client slow. Default defaultWSSlowClientTimeout.
	SlowClientTimeout time.Duration
	// WriteTimeout bounds one frame write. Default defaultWSWriteTimeout.
	WriteTimeout time.Duration
}

func (o WSOptions) withDefaults() WSOptions {
	if o.TicketTTL <= 0 {
		o.TicketTTL = defaultWSTicketTTL
	}
	if o.Buffer <= 0 {
		o.Buffer = defaultWSBuffer
	}
	if o.PingInterval <= 0 {
		o.PingInterval = defaultWSPingInterval
	}
	if o.PollInterval <= 0 {
		o.PollInterval = defaultWSPollInterval
	}
	if o.ResyncInterval <= 0 {
		o.ResyncInterval = defaultWSResyncInterval
	}
	if o.SlowClientTimeout <= 0 {
		o.SlowClientTimeout = defaultWSSlowClientTimeout
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = defaultWSWriteTimeout
	}
	return o
}

// WithWebSocket enables the run WebSocket endpoint (ticket 16.3). A non-empty
// TicketSecret is required — an empty secret would sign forgeable tickets — so
// cmd/api always passes one (a configured value or a random per-process
// secret). Without this option the /v1/runs/{id}/ws and ws-ticket routes are
// still mounted (route coverage stays static) but answer 503 stream_unavailable.
func WithWebSocket(opts WSOptions) Option {
	return func(h *Handler) {
		if opts.TicketSecret == "" {
			// Defensive: a misconfigured caller must not enable a forgeable
			// ticket. Leave WS disabled (the routes answer 503).
			return
		}
		o := opts.withDefaults()
		h.ws = &o
	}
}

// wsEnabled reports whether the WebSocket endpoint is wired.
func (h *Handler) wsEnabled() bool { return h.ws != nil }

// handleMintWSTicket is POST /v1/runs/{id}/ws-ticket (ticket 16.3): mint a
// short-lived signed ticket scoped to this run + read scope. Requires the read
// scope (mounted with requireScope(ScopeRead)); the run must exist. The ticket
// is opaque; the client passes it to GET .../ws as ?ticket=.
func (h *Handler) handleMintWSTicket(w http.ResponseWriter, r *http.Request) {
	if !h.wsEnabled() {
		writeError(w, http.StatusServiceUnavailable, ErrorDetail{
			Code: ErrCodeStreamUnavailable, Message: "event streaming is not enabled on this server",
		})
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: "run id is not a valid UUID",
		})
		return
	}
	ctx := r.Context()
	if _, err := h.st.Runs().Get(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, ErrorDetail{
				Code: ErrCodeRunNotFound, Message: "no run with id " + id.String(),
			})
			return
		}
		internalError(w, r, "reading run", err)
		return
	}
	keyID := ""
	if idn, ok := identityFrom(ctx); ok {
		keyID = idn.keyID
	}
	ticket, exp, err := mintWSTicket(h.ws.TicketSecret, id, keyID, h.now(), h.ws.TicketTTL)
	if err != nil {
		internalError(w, r, "minting ws ticket", err)
		return
	}
	writeJSON(w, http.StatusOK, WSTicketResponse{Ticket: ticket, ExpiresAt: exp})
}

// handleRunWS is GET /v1/runs/{id}/ws (ticket 16.3). Auth has already passed
// (requireReadOrTicket). It loads the snapshot, upgrades the connection, and
// runs the snapshot → backfill → live-tail protocol until the peer disconnects,
// the server shuts down, or the client falls too far behind (close 4001).
func (h *Handler) handleRunWS(w http.ResponseWriter, r *http.Request) {
	if !h.wsEnabled() {
		writeError(w, http.StatusServiceUnavailable, ErrorDetail{
			Code: ErrCodeStreamUnavailable, Message: "event streaming is not enabled on this server",
		})
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: "run id is not a valid UUID",
		})
		return
	}
	lastSeq, err := parseLastSeq(r.URL.Query().Get("last_seq"))
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: "last_seq must be a non-negative integer",
		})
		return
	}

	ctx := r.Context()
	// Load the snapshot before upgrading so a missing run is a clean 404 rather
	// than an opened-then-closed socket.
	snapshot, err := h.loadRunResponse(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, ErrorDetail{
				Code: ErrCodeRunNotFound, Message: "no run with id " + id.String(),
			})
			return
		}
		internalError(w, r, "reading run", err)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The browser origin is authorized against the request host by default;
		// a same-origin dashboard needs nothing more. Cross-origin dashboards
		// set OriginPatterns via a future config knob if needed.
	})
	if err != nil {
		// Accept has already written a response; just note it.
		log.From(ctx).InfoContext(ctx, "ws: upgrade failed", slog.Any("error", err))
		return
	}
	// From here the connection is hijacked; drive it to completion.
	(&wsConn{
		h:        h,
		conn:     conn,
		runID:    id,
		lastSeq:  lastSeq,
		snapshot: snapshot,
		log:      log.From(ctx),
	}).run(ctx)
}

// wsConn drives one accepted connection. It is created per request and used by
// a single handler goroutine (plus the writer goroutine it spawns).
type wsConn struct {
	h        *Handler
	conn     *websocket.Conn
	runID    uuid.UUID
	lastSeq  int64
	snapshot RunResponse
	log      *slog.Logger
}

// run executes the protocol. It owns the connection's lifecycle: on return the
// connection is closed with the appropriate code.
//
// The subtlety worth stating: a slow client is signalled through the outbound
// buffer, NOT by cancelling the writer's context — coder/websocket closes the
// connection when a Write's context is cancelled, which would prevent the very
// close frame we want the client to see. So on a slow client we stop feeding,
// let the writer drain what it can as the client resumes reading, and only then
// send the 4001 close frame. connCtx is cancelled only for a genuine teardown
// (peer gone, server shutdown, write error), where a clean close is moot.
func (c *wsConn) run(parent context.Context) {
	opts := c.h.ws
	connCtx, cancel := context.WithCancel(parent)
	defer cancel()

	// CloseRead drains incoming frames (so pongs are read and a client close is
	// observed) and returns a context cancelled when the peer goes away. We do
	// not expect application messages from the client in 16.3.
	readCtx := c.conn.CloseRead(connCtx)
	go func() {
		<-readCtx.Done()
		cancel()
	}()

	// The outbound buffer + sole writer goroutine. deliver enqueues here; the
	// writer serializes frame writes. A stalled client keeps the buffer full,
	// which deliver detects (a bounded enqueue wait) and turns into a graceful
	// 4001 close.
	send := make(chan []byte, opts.Buffer)
	writerDone := make(chan struct{})
	go c.writeLoop(connCtx, send, writerDone, cancel)

	closeCode := websocket.StatusNormalClosure
	closeReason := ""
	sendClosed := false
	closeSend := func() {
		if !sendClosed {
			close(send)
			sendClosed = true
		}
	}
	defer func() {
		// Stop feeding, let the writer drain (a resuming slow client unblocks it),
		// then close. Bounded so a truly-dead peer cannot hang the handler beyond
		// one write timeout.
		closeSend()
		select {
		case <-writerDone:
		case <-time.After(opts.WriteTimeout + time.Second):
		}
		if err := c.conn.Close(closeCode, closeReason); err != nil {
			_ = c.conn.CloseNow()
		}
	}()

	// enqueue offers one frame to the writer, returning false if the connection
	// is torn down or the client is too slow (slow is set so the caller records
	// the 4001 disposition). It never cancels the writer.
	slow := false
	enqueue := func(b []byte) bool {
		select {
		case send <- b:
			return true
		case <-connCtx.Done():
			return false
		case <-time.After(opts.SlowClientTimeout):
			slow = true
			return false
		}
	}
	deliver := func(env event.Envelope) {
		b, err := json.Marshal(WSEventFrame{Type: WSFrameEvent, Event: env})
		if err != nil {
			return // an un-marshalable envelope is a bug; skip it, the DB has it
		}
		if enqueue(b) && env.Seq > c.lastSeq {
			c.lastSeq = env.Seq
		}
	}
	// markClose records the disposition for the deferred close.
	markClose := func() {
		if slow {
			closeCode, closeReason = wsCloseSlowConsumer, "slow consumer"
		}
	}

	// 1. Snapshot.
	if b, err := json.Marshal(WSSnapshotFrame{Type: WSFrameSnapshot, Run: c.snapshot}); err == nil {
		if !enqueue(b) {
			markClose()
			return
		}
	}

	// Subscribe BEFORE the backfill so no live event is missed in the window
	// between reading to head and the tail beginning (pubsub.SubscribeRun blocks
	// until the SUBSCRIBE is confirmed). A subscribe failure (Redis down, or no
	// subscriber wired) degrades to poll-only — never an error to the client.
	var live <-chan event.Envelope
	if opts.Subscriber != nil {
		sub, err := opts.Subscriber.SubscribeRun(connCtx, c.runID)
		if err != nil {
			c.log.WarnContext(connCtx, "ws: live subscribe failed, falling back to polling",
				slog.Any("error", err))
		} else {
			defer func() { _ = sub.Close() }()
			live = sub.Events()
		}
	}

	tailer := pubsub.NewTailer(c.runID, c.lastSeq, c.h.st.Events(), deliver, wsBackfillPageSize)

	// 2. Backfill from last_seq to head.
	if err := tailer.Catchup(connCtx); err != nil {
		if connCtx.Err() != nil || slow {
			markClose()
			return
		}
		closeCode, closeReason = websocket.StatusInternalError, "backfill failed"
		return
	}
	if slow {
		markClose()
		return
	}

	// 3. caught_up marks the live boundary.
	if b, err := json.Marshal(WSCaughtUpFrame{Type: WSFrameCaughtUp, LastSeq: tailer.LastSeq()}); err == nil {
		if !enqueue(b) {
			markClose()
			return
		}
	}

	// 4. Live tail.
	pingT := time.NewTicker(opts.PingInterval)
	defer pingT.Stop()
	// The resync/poll safety re-read: at ResyncInterval when a live subscription
	// exists (heals a lost final publish), at the faster PollInterval when there
	// is no live path at all.
	resyncEvery := opts.ResyncInterval
	if live == nil {
		resyncEvery = opts.PollInterval
	}
	resyncT := time.NewTicker(resyncEvery)
	defer resyncT.Stop()

	for {
		if slow {
			markClose()
			return
		}
		select {
		case <-connCtx.Done():
			markClose()
			return
		case env, ok := <-live:
			if !ok {
				live = nil // subscription closed; keep polling via resync
				continue
			}
			if err := tailer.Offer(connCtx, env); err != nil && connCtx.Err() == nil && !slow {
				c.log.WarnContext(connCtx, "ws: tail offer failed", slog.Any("error", err))
			}
		case <-resyncT.C:
			if err := tailer.Catchup(connCtx); err != nil && connCtx.Err() == nil && !slow {
				c.log.WarnContext(connCtx, "ws: resync backfill failed", slog.Any("error", err))
			}
		case <-pingT.C:
			pingCtx, pingCancel := context.WithTimeout(connCtx, opts.WriteTimeout)
			err := c.conn.Ping(pingCtx)
			pingCancel()
			if err != nil {
				cancel() // unresponsive peer
			}
		}
	}
}

// writeLoop is the sole frame writer. It drains send and writes each frame as a
// text message under a per-frame deadline. On a genuine write error (a dead or
// wedged peer that stays dead past the timeout) it cancels the connection; when
// send is closed it drains the remainder first, so a slow client that resumes
// reading still receives the buffered frames and the subsequent close frame. It
// always closes writerDone on return.
func (c *wsConn) writeLoop(connCtx context.Context, send <-chan []byte, writerDone chan<- struct{}, cancel context.CancelFunc) {
	defer close(writerDone)
	for {
		select {
		case <-connCtx.Done():
			return
		case b, ok := <-send:
			if !ok {
				return // slow-close drain complete
			}
			wctx, wcancel := context.WithTimeout(connCtx, c.h.ws.WriteTimeout)
			err := c.conn.Write(wctx, websocket.MessageText, b)
			wcancel()
			if err != nil {
				cancel()
				return
			}
		}
	}
}

// parseLastSeq parses the optional ?last_seq resume cursor. Empty means 0 (a
// fresh tail from the beginning).
func parseLastSeq(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("invalid last_seq")
	}
	return n, nil
}
