package loadgen

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/event"
)

// firehoseClient tails the multi-run firehose (/v1/events/ws) with a types
// filter, feeding decoded events to the tracker. It authenticates with a read
// bearer on the upgrade (the non-browser path — no ticket dance), dedupes by
// (run_id, seq), and reconnects with backoff on any drop. Terminal detection
// through the firehose is a low-latency *sampling* channel; the poll pool is
// the authority, so a missed frame is never fatal.
type firehoseClient struct {
	base   string
	key    string
	types  []string
	onEvt  func(feedEvent) bool
	logger *slog.Logger

	frames     atomic.Int64
	reconnects atomic.Int64
	connected  atomic.Bool

	seen map[string]int64 // run id → highest seq applied (dedupe)
}

func newFirehoseClient(base, key string, sched bool, onEvt func(feedEvent) bool, logger *slog.Logger) *firehoseClient {
	types := []string{
		string(event.TypeRunSucceeded),
		string(event.TypeRunFailed),
		string(event.TypeRunCancelled),
	}
	if sched {
		types = append(types, string(event.TypeStepReady), string(event.TypeStepClaimed))
	}
	return &firehoseClient{
		base:   base,
		key:    key,
		types:  types,
		onEvt:  onEvt,
		logger: logger,
		seen:   map[string]int64{},
	}
}

// wsURL turns the API base into the firehose WebSocket URL.
func (f *firehoseClient) wsURL() string {
	u := f.base
	switch {
	case strings.HasPrefix(u, "https://"):
		u = "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		u = "ws://" + strings.TrimPrefix(u, "http://")
	}
	return u + "/v1/events/ws"
}

// Run maintains the connection until ctx is cancelled. It returns only when
// ctx is done.
func (f *firehoseClient) Run(ctx context.Context) {
	backoff := 200 * time.Millisecond
	for ctx.Err() == nil {
		if err := f.session(ctx); err != nil && ctx.Err() == nil {
			f.connected.Store(false)
			f.logger.Debug("loadgen: firehose session ended", slog.String("err", err.Error()))
			f.reconnects.Add(1)
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			if backoff < 5*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 200 * time.Millisecond
	}
}

// session opens one connection, subscribes, and pumps frames until an error.
func (f *firehoseClient) session(ctx context.Context) error {
	c, _, err := websocket.Dial(ctx, f.wsURL(), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + f.key}},
	})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "") //nolint:errcheck
	c.SetReadLimit(1 << 20)

	sub := api.WSSubscribeMessage{Type: api.WSMsgSubscribe, ID: "loadgen", Filter: api.WSFilter{Types: f.types}}
	b, _ := json.Marshal(sub)
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return err
		}
		f.handleFrame(data)
	}
}

// frameWire is the on-wire decode of any firehose frame; only Type/Event and
// the event's own fields are needed.
type frameWire struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Code  string `json:"code"`
	Event struct {
		RunID  string    `json:"run_id"`
		Seq    int64     `json:"seq"`
		Type   string    `json:"type"`
		StepID string    `json:"step_id"`
		Ts     time.Time `json:"ts"`
	} `json:"event"`
}

func (f *firehoseClient) handleFrame(data []byte) {
	var fr frameWire
	if err := json.Unmarshal(data, &fr); err != nil {
		return
	}
	switch fr.Type {
	case api.WSFrameSubscribed:
		f.connected.Store(true)
	case api.WSFrameEvent:
		// Dedupe by (run_id, seq): the firehose backfills a run from 0 on first
		// sighting, so an event can arrive more than once across reconnects.
		if prior, ok := f.seen[fr.Event.RunID]; ok && fr.Event.Seq <= prior {
			return
		}
		f.seen[fr.Event.RunID] = fr.Event.Seq
		f.frames.Add(1)
		f.onEvt(feedEvent{
			RunID:  fr.Event.RunID,
			Seq:    fr.Event.Seq,
			Type:   event.Type(fr.Event.Type),
			StepID: fr.Event.StepID,
			Ts:     fr.Event.Ts,
		})
	case api.WSFrameError:
		f.logger.Debug("loadgen: firehose in-band error", slog.String("code", fr.Code))
	}
}
