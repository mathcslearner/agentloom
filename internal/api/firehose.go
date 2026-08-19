package api

// The multi-run firehose endpoint (ticket 16.4, ADR-018): GET /v1/events/ws
// streams a filtered, cross-run live event feed for the dashboard's run list.
// It is the run WebSocket (16.3) generalized to many runs on one connection,
// with server-side filters and client-managed subscriptions.
//
// Shape. One process holds at most a single Redis firehose subscription (the
// hub), refcounted by connected clients. The hub fans every committed envelope
// out to each connected client's bounded inbox; the client goroutine filters,
// dedupes, and tails the runs it cares about through per-run pubsub.Tailers
// (the same gap-heal-via-backfill machinery as 16.3), and writes matched
// envelopes to its wsLink. So the durable contract is unchanged — Postgres is
// the truth, pub/sub is a latency hint, and every gap heals from last_seq.
//
// Protocol (JSON text frames). Client → server: {"type":"subscribe", id,
// filter, cursors} and {"type":"unsubscribe", id}. Server → client: a
// "subscribed" ack → cursor-backfilled "event" frames → a "caught_up" frame per
// subscription → live "event" frames (each tagged with the subscription ids it
// matched) → "unsubscribed" acks; a malformed or over-limit control message
// yields an in-band "error" frame and leaves the connection open. Discovery is
// implicit: a run is tailed and backfilled to head the first time a live
// envelope for it matches some subscription, so the complete per-run feed is
// delivered even under cross-publisher reorder (a cursor resumes from a later
// seq instead).
//
// Auth is the same short-lived signed ticket as 16.3, minted at
// POST /v1/events/ws-ticket with the firehose audience (or a read bearer).

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/event"
	"github.com/mathcslearner/agentloom/internal/event/pubsub"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// wsFirehoseReadLimit bounds one inbound control frame. Cursor maps can carry up
// to MaxCursorRuns uuids, so the limit is generous but bounded.
const wsFirehoseReadLimit = 64 << 10

// handleMintFirehoseWSTicket is POST /v1/events/ws-ticket (ticket 16.4): mint a
// short-lived signed ticket scoped to the firehose audience + read scope. Unlike
// the run ticket it is not bound to a run (the firehose is cross-run); the
// filters that narrow it are the client's subscribe messages.
func (h *Handler) handleMintFirehoseWSTicket(w http.ResponseWriter, r *http.Request) {
	if !h.wsEnabled() {
		writeError(w, http.StatusServiceUnavailable, ErrorDetail{
			Code: ErrCodeStreamUnavailable, Message: "event streaming is not enabled on this server",
		})
		return
	}
	keyID := ""
	if idn, ok := identityFrom(r.Context()); ok {
		keyID = idn.keyID
	}
	ticket, exp, err := mintFirehoseWSTicket(h.ws.TicketSecret, keyID, h.now(), h.ws.TicketTTL)
	if err != nil {
		internalError(w, r, "minting firehose ticket", err)
		return
	}
	writeJSON(w, http.StatusOK, WSTicketResponse{Ticket: ticket, ExpiresAt: exp})
}

// handleEventsWS is GET /v1/events/ws (ticket 16.4). Auth has already passed
// (requireReadOrTicket with the firehose audience). It upgrades the connection
// and runs the multi-run protocol until the peer disconnects, the server shuts
// down, or the client falls too far behind (close 4001).
func (h *Handler) handleEventsWS(w http.ResponseWriter, r *http.Request) {
	if !h.wsEnabled() {
		writeError(w, http.StatusServiceUnavailable, ErrorDetail{
			Code: ErrCodeStreamUnavailable, Message: "event streaming is not enabled on this server",
		})
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		log.From(r.Context()).InfoContext(r.Context(), "ws: firehose upgrade failed", slog.Any("error", err))
		return
	}
	(&firehoseConn{
		h:         h,
		hub:       h.hub,
		conn:      conn,
		log:       log.From(r.Context()),
		inbox:     make(chan event.Envelope, h.ws.HubBuffer),
		control:   make(chan []byte, 8),
		subs:      make(map[string]*firehoseSub),
		tails:     make(map[uuid.UUID]*runTail),
		highWater: h.now(),
	}).run(r.Context())
}

// ----- The hub: one firehose subscription per process, fanned out -----

// firehoseHub holds at most one Redis firehose subscription for the whole
// process and fans each envelope out to every connected client. The
// subscription is lazily started on the first connection and released when the
// last one leaves (refcounted), so there is no background goroutine or Close to
// manage on the Handler. When no live subscriber is wired, or the subscribe
// fails, connections self-discover new runs by polling the run list.
type firehoseHub struct {
	opts    *WSOptions
	sub     WSSubscriber // nil ⇒ poll-only
	st      *store.Store
	metrics RequestMetrics
	log     *slog.Logger
	meta    *runMetaCache

	mu           sync.Mutex
	conns        map[*firehoseConn]struct{}
	stream       WSEventStream
	streamCancel context.CancelFunc
}

func newFirehoseHub(opts *WSOptions, st *store.Store, metrics RequestMetrics, logger *slog.Logger) *firehoseHub {
	return &firehoseHub{
		opts:    opts,
		sub:     opts.Subscriber,
		st:      st,
		metrics: metrics,
		log:     logger,
		meta:    newRunMetaCache(st, opts.RunMetaCacheSize),
		conns:   make(map[*firehoseConn]struct{}),
	}
}

// attach registers a connection and, if this is the first one and a live
// subscriber is wired, opens the shared firehose subscription. A subscribe
// failure is non-fatal: connections fall back to run-list polling.
func (hb *firehoseHub) attach(c *firehoseConn) {
	hb.mu.Lock()
	defer hb.mu.Unlock()
	hb.conns[c] = struct{}{}
	if hb.stream != nil || hb.sub == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := hb.sub.SubscribeFirehose(ctx)
	if err != nil {
		cancel()
		hb.log.Warn("firehose: subscribe failed, connections fall back to polling", slog.Any("error", err))
		return
	}
	hb.stream = stream
	hb.streamCancel = cancel
	go hb.fanout(ctx, stream)
}

// detach removes a connection and releases the shared subscription once the last
// client leaves.
func (hb *firehoseHub) detach(c *firehoseConn) {
	hb.mu.Lock()
	defer hb.mu.Unlock()
	delete(hb.conns, c)
	if len(hb.conns) == 0 && hb.stream != nil {
		_ = hb.stream.Close()
		hb.streamCancel()
		hb.stream = nil
		hb.streamCancel = nil
	}
}

// hasStream reports whether the shared firehose subscription is live (so a
// connection knows whether to run-list-poll for discovery).
func (hb *firehoseHub) hasStream() bool {
	hb.mu.Lock()
	defer hb.mu.Unlock()
	return hb.stream != nil
}

// fanout forwards each firehose envelope to every connected client's inbox,
// non-blocking. It exits when the subscription closes or its context is done.
func (hb *firehoseHub) fanout(ctx context.Context, stream WSEventStream) {
	ch := stream.Events()
	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-ch:
			if !ok {
				return
			}
			// Hold the lock only for the non-blocking fan-out (each push is a
			// buffered-channel send with a default drop); attach/detach are rare.
			hb.mu.Lock()
			for c := range hb.conns {
				c.push(env)
			}
			hb.mu.Unlock()
		}
	}
}

// ----- The run→definition metadata cache (shared across connections) -----

// runMeta is a run's immutable attributes a firehose filter needs: its
// definition name and (registry) definition id (nil for an inline definition).
type runMeta struct {
	name  string
	defID *uuid.UUID
}

// runMetaCache is a bounded, process-wide cache of run→runMeta, so a
// definition-filtered firehose resolves a run's definition at most once across
// all connections. Entries are filled cheaply from run_created payloads and
// otherwise by one Runs().Get. FIFO eviction bounds memory.
type runMetaCache struct {
	st    *store.Store
	limit int

	mu    sync.Mutex
	m     map[uuid.UUID]runMeta
	order []uuid.UUID
}

func newRunMetaCache(st *store.Store, limit int) *runMetaCache {
	if limit <= 0 {
		limit = defaultWSRunMetaCacheSize
	}
	return &runMetaCache{st: st, limit: limit, m: make(map[uuid.UUID]runMeta)}
}

func (c *runMetaCache) put(id uuid.UUID, meta runMeta) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[id]; ok {
		c.m[id] = meta
		return
	}
	if len(c.order) >= c.limit && len(c.order) > 0 {
		delete(c.m, c.order[0])
		c.order = c.order[1:]
	}
	c.m[id] = meta
	c.order = append(c.order, id)
}

// lookup returns cached meta or resolves it from the store (and caches it). A
// store error returns the zero runMeta with the error; a definition filter then
// treats the run as non-matching.
func (c *runMetaCache) lookup(ctx context.Context, id uuid.UUID) (runMeta, error) {
	c.mu.Lock()
	if m, ok := c.m[id]; ok {
		c.mu.Unlock()
		return m, nil
	}
	c.mu.Unlock()
	run, err := c.st.Runs().Get(ctx, id)
	if err != nil {
		return runMeta{}, err
	}
	m := metaFromRow(run)
	c.put(id, m)
	return m, nil
}

// metaFromRow extracts runMeta from a run row. The definition name lives inside
// the (immutable) definition snapshot JSON.
func metaFromRow(run gen.Run) runMeta {
	m := runMeta{defID: run.DefinitionID}
	if len(run.Definition) > 0 {
		var d struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(run.Definition, &d); err == nil {
			m.name = d.Name
		}
	}
	return m
}

// ----- Filters -----

// firehoseFilter is the compiled server-side narrowing of a subscription. A nil
// runIDs/types map means "any"; defID/defName empty means "any".
type firehoseFilter struct {
	runIDs  map[uuid.UUID]bool
	types   map[event.Type]bool
	defID   *uuid.UUID
	defName string
}

// needsMeta reports whether deciding this filter requires a run's definition
// metadata (so callers only resolve meta when it can change the outcome).
func (f firehoseFilter) needsMeta() bool { return f.defID != nil || f.defName != "" }

// wantsRun reports whether the filter could match any event of a run — the
// tracking decision, made without an event (so it omits the per-event type
// check). meta is resolved lazily.
func (f firehoseFilter) wantsRun(runID uuid.UUID, meta func() runMeta) bool {
	if f.runIDs != nil && !f.runIDs[runID] {
		return false
	}
	if f.needsMeta() {
		m := meta()
		if f.defID != nil && (m.defID == nil || *m.defID != *f.defID) {
			return false
		}
		if f.defName != "" && m.name != f.defName {
			return false
		}
	}
	return true
}

// matches reports whether a specific envelope matches the filter (run + type +
// definition). meta is resolved lazily.
func (f firehoseFilter) matches(env event.Envelope, meta func() runMeta) bool {
	if f.types != nil && !f.types[env.Type] {
		return false
	}
	return f.wantsRun(env.RunID, meta)
}

// compileFilter validates a wire filter and compiles it. maxRuns bounds run_ids.
func compileFilter(in WSFilter, maxRuns int) (firehoseFilter, error) {
	var f firehoseFilter
	if len(in.RunIDs) > maxRuns {
		return f, fmt.Errorf("run_ids exceeds the %d-run limit", maxRuns)
	}
	if len(in.RunIDs) > 0 {
		f.runIDs = make(map[uuid.UUID]bool, len(in.RunIDs))
		for _, s := range in.RunIDs {
			id, err := uuid.Parse(s)
			if err != nil {
				return firehoseFilter{}, fmt.Errorf("run_ids: %q is not a uuid", s)
			}
			f.runIDs[id] = true
		}
	}
	if len(in.Types) > len(event.Catalog) {
		return f, fmt.Errorf("types exceeds the %d known event types", len(event.Catalog))
	}
	if len(in.Types) > 0 {
		f.types = make(map[event.Type]bool, len(in.Types))
		for _, t := range in.Types {
			if _, ok := event.Lookup(event.Type(t)); !ok {
				return firehoseFilter{}, fmt.Errorf("types: %q is not a known event type", t)
			}
			f.types[event.Type(t)] = true
		}
	}
	if in.DefinitionID != "" {
		id, err := uuid.Parse(in.DefinitionID)
		if err != nil {
			return firehoseFilter{}, fmt.Errorf("definition_id is not a uuid")
		}
		f.defID = &id
	}
	f.defName = in.DefinitionName
	return f, nil
}

// parseCursors validates and parses a wire cursor map into uuid keys.
func parseCursors(in map[string]int64, maxRuns int) (map[uuid.UUID]int64, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > maxRuns {
		return nil, fmt.Errorf("cursors exceeds the %d-run limit", maxRuns)
	}
	out := make(map[uuid.UUID]int64, len(in))
	for s, v := range in {
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("cursors: %q is not a uuid", s)
		}
		if v < 0 {
			return nil, fmt.Errorf("cursors: %q has a negative seq", s)
		}
		out[id] = v
	}
	return out, nil
}

func parseUUIDPtr(s string) *uuid.UUID {
	if s == "" {
		return nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return nil
	}
	return &id
}

// ----- One firehose connection -----

// firehoseSub is one active subscription on a connection.
type firehoseSub struct {
	id      string
	filter  firehoseFilter
	cursors map[uuid.UUID]int64
}

// runTail is a connection's tail state for one run: the seq-ordering Tailer plus
// a terminal flag so resync skips finished runs.
type runTail struct {
	tailer   *pubsub.Tailer
	terminal bool
}

// firehoseConn drives one accepted firehose connection. It is used by a single
// goroutine (its run loop) plus the wsLink writer and a control-reader
// goroutine; all connection state (subs, tails) is touched only by the run
// goroutine, so no locks are needed there.
type firehoseConn struct {
	h    *Handler
	hub  *firehoseHub
	conn *websocket.Conn
	log  *slog.Logger
	link *wsLink

	inbox   chan event.Envelope // fan-out from the hub
	control chan []byte         // raw inbound control frames from readLoop
	lagged  atomic.Bool         // set when the hub dropped an envelope for us

	subs       map[string]*firehoseSub
	tails      map[uuid.UUID]*runTail
	trackOrder []uuid.UUID // insertion order, for eviction
	highWater  time.Time   // poll-mode discovery cursor (max created_at seen)
}

// run executes the firehose protocol: attach to the hub, read control messages,
// and deliver filtered/deduped events until teardown.
func (c *firehoseConn) run(parent context.Context) {
	opts := c.hub.opts
	c.link = newWSLink(parent, c.conn, opts, c.h.metrics, c.log, wsKindFirehose)
	c.hub.attach(c)
	defer c.hub.detach(c)
	defer func() {
		for range c.subs {
			c.h.metrics.WSSubscriptionClosed()
		}
	}()

	closeCode := websocket.StatusNormalClosure
	closeReason := ""
	defer func() { c.link.finish(closeCode, closeReason) }()

	go c.readLoop()

	// Poll for discovery when there is no live firehose subscription; otherwise
	// resync only heals tracked runs (a lost final publish, a dropped inbox
	// envelope). Chosen once at start — the shared subscription outlives every
	// connection that keeps it alive.
	live := c.hub.hasStream()
	resyncEvery := opts.ResyncInterval
	if !live {
		resyncEvery = opts.PollInterval
	}
	resyncT := time.NewTicker(resyncEvery)
	defer resyncT.Stop()
	pingT := time.NewTicker(opts.PingInterval)
	defer pingT.Stop()

	for {
		if c.link.slow {
			return
		}
		select {
		case <-c.link.connCtx.Done():
			return
		case data := <-c.control:
			c.onControl(data)
		case env := <-c.inbox:
			c.onLiveEnvelope(env)
		case <-resyncT.C:
			c.resync(live)
		case <-pingT.C:
			c.link.ping()
		}
	}
}

// push is called by the hub fan-out: enqueue an envelope non-blocking; a full
// inbox drops it (a seq gap the connection heals via resync backfill).
func (c *firehoseConn) push(env event.Envelope) {
	select {
	case c.inbox <- env:
	default:
		c.hub.metrics.WSHubDropped()
		c.lagged.Store(true)
	}
}

// readLoop reads inbound control frames and forwards them to the run goroutine.
// A read error (peer gone, oversize frame) tears the connection down.
func (c *firehoseConn) readLoop() {
	c.conn.SetReadLimit(wsFirehoseReadLimit)
	for {
		typ, data, err := c.conn.Read(c.link.connCtx)
		if err != nil {
			c.link.cancel()
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		select {
		case c.control <- data:
		case <-c.link.connCtx.Done():
			return
		}
	}
}

// onControl decodes and dispatches one control message. A malformed or
// unknown-type message yields an in-band error frame and keeps the connection.
func (c *firehoseConn) onControl(data []byte) {
	var msg struct {
		Type    string           `json:"type"`
		ID      string           `json:"id"`
		Filter  WSFilter         `json:"filter"`
		Cursors map[string]int64 `json:"cursors"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		c.sendError("", ErrCodeBadMessage, "malformed control message")
		return
	}
	switch msg.Type {
	case WSMsgSubscribe:
		c.onSubscribe(msg.ID, msg.Filter, msg.Cursors)
	case WSMsgUnsubscribe:
		c.onUnsubscribe(msg.ID)
	default:
		c.sendError(msg.ID, ErrCodeBadMessage, "unknown control message type "+msg.Type)
	}
}

func (c *firehoseConn) sendError(id, code, msg string) {
	c.link.enqueueJSON(WSErrorFrame{Type: WSFrameError, Code: code, Message: msg, ID: id})
}

// onSubscribe validates and installs (or replaces) a subscription, backfills any
// resumed runs from their cursors, and acks with a caught_up frame.
func (c *firehoseConn) onSubscribe(id string, in WSFilter, cursors map[string]int64) {
	opts := c.hub.opts
	if id == "" || len(id) > 64 {
		c.sendError(id, ErrCodeFilterInvalid, "subscription id must be 1..64 characters")
		return
	}
	_, replacing := c.subs[id]
	if !replacing && len(c.subs) >= opts.MaxSubscriptions {
		c.sendError(id, ErrCodeSubscriptionLimit, fmt.Sprintf("connection already holds %d subscriptions", opts.MaxSubscriptions))
		return
	}
	filter, err := compileFilter(in, opts.MaxCursorRuns)
	if err != nil {
		c.sendError(id, ErrCodeFilterInvalid, err.Error())
		return
	}
	cur, err := parseCursors(cursors, opts.MaxCursorRuns)
	if err != nil {
		c.sendError(id, ErrCodeFilterInvalid, err.Error())
		return
	}
	c.subs[id] = &firehoseSub{id: id, filter: filter, cursors: cur}
	if !replacing {
		c.h.metrics.WSSubscriptionOpened()
	}
	c.link.enqueueJSON(WSSubscribedFrame{Type: WSFrameSubscribed, ID: id, Filter: in})

	// Backfill each resumed run from its cursor, then report the resume point.
	ack := make(map[string]int64, len(cur))
	for runID, cv := range cur {
		rt := c.tails[runID]
		if rt == nil {
			rt = c.startTailAt(runID, cv, true)
		}
		if rt != nil {
			ack[runID.String()] = rt.tailer.LastSeq()
		}
	}
	c.link.enqueueJSON(WSFirehoseCaughtUpFrame{Type: WSFrameCaughtUp, ID: id, Cursors: ack})
}

// onUnsubscribe removes a subscription and drops any run no remaining
// subscription wants.
func (c *firehoseConn) onUnsubscribe(id string) {
	if _, ok := c.subs[id]; !ok {
		c.sendError(id, ErrCodeUnknownSubscription, "no such subscription")
		return
	}
	delete(c.subs, id)
	c.h.metrics.WSSubscriptionClosed()
	c.link.enqueueJSON(WSUnsubscribedFrame{Type: WSFrameUnsubscribed, ID: id})
	c.gcTails()
}

// onLiveEnvelope processes one fanned-out firehose envelope: discover a new run
// (tracking it only if some subscription wants it), then Offer it to the run's
// Tailer, which delivers in seq order via deliverEvent.
func (c *firehoseConn) onLiveEnvelope(env event.Envelope) {
	rt, tracked := c.tails[env.RunID]
	if !tracked {
		// Populate the shared meta cache from run_created cheaply (no DB Get).
		if rc, ok := env.Payload.(*event.RunCreated); ok {
			c.hub.meta.put(env.RunID, runMeta{name: rc.Name, defID: parseUUIDPtr(rc.DefinitionID)})
		}
		if !c.anySubWantsRun(env.RunID) {
			return
		}
		// Discover the run from its start (or a supplied cursor) and backfill to
		// head. Backfill-from-0 — rather than live-only from this first sighting
		// — guarantees the complete per-run feed even though run_created (written
		// by the API) and step events (written by workers) publish from different
		// processes and can reach us out of order; the run list must never miss a
		// run's run_created. A filter bounds how many runs a client backfills.
		start := int64(0)
		if cv, ok := c.minCursor(env.RunID); ok {
			start = cv
		}
		rt = c.startTailAt(env.RunID, start, true)
		if rt == nil {
			return
		}
	}
	if err := rt.tailer.Offer(c.link.connCtx, env); err != nil && c.link.connCtx.Err() == nil && !c.link.slow {
		c.log.WarnContext(c.link.connCtx, "firehose: tail offer failed", slog.Any("error", err))
	}
	c.markTerminal(rt, env)
}

// deliverEvent is a run Tailer's in-order callback: tag the envelope with the
// subscription ids it matches and enqueue one frame. A run may be tracked for a
// subscription that does not want this particular event type — then no id
// matches and nothing is sent (the cursor still advances).
func (c *firehoseConn) deliverEvent(env event.Envelope) {
	metaFn := c.metaFn(env.RunID)
	var matched []string
	for id, sub := range c.subs {
		if sub.filter.matches(env, metaFn) {
			matched = append(matched, id)
		}
	}
	if len(matched) == 0 {
		return
	}
	sort.Strings(matched)
	c.link.enqueueJSON(WSEventFrame{Type: WSFrameEvent, Event: env, Subscriptions: matched})
}

// resync heals tracked non-terminal runs (a lost final publish, or an envelope
// dropped at a full inbox) and, in poll mode, discovers new runs from the run
// list.
func (c *firehoseConn) resync(live bool) {
	for _, rt := range c.tails {
		if rt.terminal {
			continue
		}
		if err := rt.tailer.Catchup(c.link.connCtx); err != nil && c.link.connCtx.Err() == nil && !c.link.slow {
			c.log.WarnContext(c.link.connCtx, "firehose: resync backfill failed", slog.Any("error", err))
		}
	}
	c.lagged.Store(false)
	if !live {
		c.discoverNewRuns()
	}
}

// discoverNewRuns polls the run list for runs created since the connection's
// high-water and tails any a subscription wants (poll fallback only). A polled
// run is tailed from its cursor if resumed, else from the start (no live "first
// seq" is available in poll mode).
func (c *firehoseConn) discoverNewRuns() {
	ctx := c.link.connCtx
	hw := c.highWater
	runs, err := c.hub.st.Runs().ListPage(ctx, gen.ListRunsPageParams{CreatedAfter: &hw, RowLimit: 200})
	if err != nil {
		if ctx.Err() == nil {
			c.log.WarnContext(ctx, "firehose: discovery poll failed", slog.Any("error", err))
		}
		return
	}
	for _, run := range runs {
		if run.CreatedAt.After(c.highWater) {
			c.highWater = run.CreatedAt
		}
		if _, ok := c.tails[run.ID]; ok {
			continue
		}
		meta := metaFromRow(run)
		c.hub.meta.put(run.ID, meta)
		metaFn := func() runMeta { return meta }
		wanted := false
		for _, sub := range c.subs {
			if sub.filter.wantsRun(run.ID, metaFn) {
				wanted = true
				break
			}
		}
		if !wanted {
			continue
		}
		start := int64(0)
		if cv, ok := c.minCursor(run.ID); ok {
			start = cv
		}
		c.startTailAt(run.ID, start, true)
	}
}

// startTailAt begins tailing a run at lastSeq=start, optionally backfilling to
// head immediately. It enforces MaxTrackedRuns by evicting first.
func (c *firehoseConn) startTailAt(runID uuid.UUID, start int64, backfill bool) *runTail {
	if rt, ok := c.tails[runID]; ok {
		return rt
	}
	if len(c.tails) >= c.hub.opts.MaxTrackedRuns {
		c.evictOne()
	}
	if start < 0 {
		start = 0
	}
	rt := &runTail{}
	rt.tailer = pubsub.NewTailer(runID, start, c.hub.st.Events(), c.deliverEvent, wsBackfillPageSize)
	c.tails[runID] = rt
	c.trackOrder = append(c.trackOrder, runID)
	if backfill {
		if err := rt.tailer.Catchup(c.link.connCtx); err != nil && c.link.connCtx.Err() == nil && !c.link.slow {
			c.log.WarnContext(c.link.connCtx, "firehose: cursor backfill failed",
				slog.String("run_id", runID.String()), slog.Any("error", err))
		}
	}
	return rt
}

// minCursor returns the smallest cursor any subscription supplied for a run.
func (c *firehoseConn) minCursor(runID uuid.UUID) (int64, bool) {
	best := int64(0)
	has := false
	for _, sub := range c.subs {
		if cv, ok := sub.cursors[runID]; ok {
			if !has || cv < best {
				best = cv
				has = true
			}
		}
	}
	return best, has
}

// anySubWantsRun reports whether some subscription could match a run, resolving
// definition metadata at most once.
func (c *firehoseConn) anySubWantsRun(runID uuid.UUID) bool {
	metaFn := c.metaFn(runID)
	for _, sub := range c.subs {
		if sub.filter.wantsRun(runID, metaFn) {
			return true
		}
	}
	return false
}

// metaFn returns a lazily-resolving, call-cached run-metadata accessor.
func (c *firehoseConn) metaFn(runID uuid.UUID) func() runMeta {
	var m runMeta
	resolved := false
	return func() runMeta {
		if !resolved {
			m, _ = c.hub.meta.lookup(c.link.connCtx, runID)
			resolved = true
		}
		return m
	}
}

// markTerminal flags a run's tail once its terminal run event is delivered, so
// resync stops re-reading it.
func (c *firehoseConn) markTerminal(rt *runTail, env event.Envelope) {
	switch env.Type {
	case event.TypeRunSucceeded, event.TypeRunFailed, event.TypeRunCancelled:
		rt.terminal = true
	}
}

// gcTails drops any tracked run no remaining subscription wants.
func (c *firehoseConn) gcTails() {
	for runID := range c.tails {
		if !c.anySubWantsRun(runID) {
			delete(c.tails, runID)
		}
	}
}

// evictOne drops one tracked run to stay under MaxTrackedRuns, preferring a
// terminal run, else the oldest tracked. trackOrder is compacted as it is
// scanned past dropped entries.
func (c *firehoseConn) evictOne() {
	for i, runID := range c.trackOrder {
		rt, ok := c.tails[runID]
		if !ok {
			continue
		}
		if rt.terminal {
			delete(c.tails, runID)
			c.trackOrder = append(c.trackOrder[:i], c.trackOrder[i+1:]...)
			return
		}
	}
	for i, runID := range c.trackOrder {
		if _, ok := c.tails[runID]; ok {
			delete(c.tails, runID)
			c.trackOrder = append(c.trackOrder[:i], c.trackOrder[i+1:]...)
			return
		}
	}
}
