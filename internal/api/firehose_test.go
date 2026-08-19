package api

// Unit coverage for the multi-run firehose (ticket 16.4, ADR-018): the ticket
// audience split, filter compilation + matching, cursor parsing, run-metadata
// extraction, and the control/frame encodings. The hub fan-out and the full
// protocol are covered by firehose_integration_test.go against a real store +
// engine + pub/sub.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/event"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

func TestFirehoseTicketAudienceSplit(t *testing.T) {
	t.Parallel()
	secret := "firehose-secret"
	run := uuid.New()
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	runTicket, _, err := mintWSTicket(secret, run, "k", now, time.Minute)
	if err != nil {
		t.Fatalf("mintWSTicket: %v", err)
	}
	fhTicket, _, err := mintFirehoseWSTicket(secret, "k", now, time.Minute)
	if err != nil {
		t.Fatalf("mintFirehoseWSTicket: %v", err)
	}
	at := now.Add(10 * time.Second)

	// Each ticket verifies at its own audience.
	if _, err := verifyWSTicket(secret, runTicket, run, at); err != nil {
		t.Errorf("run ticket rejected at run endpoint: %v", err)
	}
	if _, err := verifyFirehoseWSTicket(secret, fhTicket, at); err != nil {
		t.Errorf("firehose ticket rejected at firehose endpoint: %v", err)
	}
	// Cross-presentation is rejected — a ticket grants exactly its stream.
	if _, err := verifyFirehoseWSTicket(secret, runTicket, at); err == nil {
		t.Error("run ticket accepted at the firehose endpoint")
	}
	if _, err := verifyWSTicket(secret, fhTicket, run, at); err == nil {
		t.Error("firehose ticket accepted at the run endpoint")
	}
}

func TestCompileFilter(t *testing.T) {
	t.Parallel()
	run := uuid.New().String()
	def := uuid.New().String()

	cases := []struct {
		name    string
		in      WSFilter
		wantErr bool
	}{
		{"empty", WSFilter{}, false},
		{"run_ids ok", WSFilter{RunIDs: []string{run}}, false},
		{"run_ids bad uuid", WSFilter{RunIDs: []string{"not-a-uuid"}}, true},
		{"types ok", WSFilter{Types: []string{string(event.TypeStepReady), string(event.TypeRunSucceeded)}}, false},
		{"types unknown", WSFilter{Types: []string{"not_an_event"}}, true},
		{"definition_id ok", WSFilter{DefinitionID: def}, false},
		{"definition_id bad", WSFilter{DefinitionID: "nope"}, true},
		{"definition_name ok", WSFilter{DefinitionName: "flagship"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compileFilter(tc.in, 256)
			if (err != nil) != tc.wantErr {
				t.Errorf("compileFilter(%+v) err = %v, wantErr = %v", tc.in, err, tc.wantErr)
			}
		})
	}

	// The run_ids cap.
	if _, err := compileFilter(WSFilter{RunIDs: []string{run, uuid.NewString()}}, 1); err == nil {
		t.Error("compileFilter accepted run_ids over the cap")
	}
}

func TestFirehoseFilterMatches(t *testing.T) {
	t.Parallel()
	runA, runB := uuid.New(), uuid.New()
	defID := uuid.New()
	otherDef := uuid.New()
	metaA := func() runMeta { return runMeta{name: "alpha", defID: &defID} }

	env := func(run uuid.UUID, typ event.Type) event.Envelope {
		return event.Envelope{RunID: run, Type: typ, Seq: 1}
	}

	mustCompile := func(in WSFilter) firehoseFilter {
		f, err := compileFilter(in, 256)
		if err != nil {
			t.Fatalf("compileFilter(%+v): %v", in, err)
		}
		return f
	}

	// run_ids narrows.
	fRun := mustCompile(WSFilter{RunIDs: []string{runA.String()}})
	if !fRun.matches(env(runA, event.TypeStepReady), metaA) {
		t.Error("run_ids filter should match run A")
	}
	if fRun.matches(env(runB, event.TypeStepReady), metaA) {
		t.Error("run_ids filter should not match run B")
	}

	// types narrows (but wantsRun ignores type).
	fType := mustCompile(WSFilter{Types: []string{string(event.TypeRunSucceeded)}})
	if fType.matches(env(runA, event.TypeStepReady), metaA) {
		t.Error("types filter should not match step_ready")
	}
	if !fType.matches(env(runA, event.TypeRunSucceeded), metaA) {
		t.Error("types filter should match run_succeeded")
	}
	if !fType.wantsRun(runA, metaA) {
		t.Error("a types-only filter wants every run (type is a per-event check)")
	}

	// definition_id narrows via metadata.
	fDef := mustCompile(WSFilter{DefinitionID: defID.String()})
	if !fDef.matches(env(runA, event.TypeStepReady), metaA) {
		t.Error("definition_id filter should match a run with that definition")
	}
	wrongDef := func() runMeta { return runMeta{name: "beta", defID: &otherDef} }
	if fDef.matches(env(runA, event.TypeStepReady), wrongDef) {
		t.Error("definition_id filter should not match a different definition")
	}
	inline := func() runMeta { return runMeta{name: "inline"} }
	if fDef.matches(env(runA, event.TypeStepReady), inline) {
		t.Error("definition_id filter should not match an inline run")
	}

	// definition_name narrows via metadata (works for inline runs).
	fName := mustCompile(WSFilter{DefinitionName: "alpha"})
	if !fName.matches(env(runA, event.TypeStepReady), metaA) {
		t.Error("definition_name filter should match by name")
	}
	if fName.matches(env(runA, event.TypeStepReady), inline) {
		t.Error("definition_name filter should not match a different name")
	}

	// Empty filter matches everything and needs no metadata.
	fAny := mustCompile(WSFilter{})
	if fAny.needsMeta() {
		t.Error("an empty filter must not need metadata")
	}
	if !fAny.matches(env(runB, event.TypeStepReady), inline) {
		t.Error("empty filter should match any event")
	}
}

func TestParseCursors(t *testing.T) {
	t.Parallel()
	a, b := uuid.New(), uuid.New()
	out, err := parseCursors(map[string]int64{a.String(): 3, b.String(): 0}, 256)
	if err != nil {
		t.Fatalf("parseCursors: %v", err)
	}
	if out[a] != 3 || out[b] != 0 {
		t.Errorf("cursors = %v", out)
	}
	if _, err := parseCursors(map[string]int64{"bad": 1}, 256); err == nil {
		t.Error("parseCursors accepted a bad uuid")
	}
	if _, err := parseCursors(map[string]int64{a.String(): -1}, 256); err == nil {
		t.Error("parseCursors accepted a negative seq")
	}
	if _, err := parseCursors(map[string]int64{a.String(): 1, b.String(): 2}, 1); err == nil {
		t.Error("parseCursors accepted a cursor map over the cap")
	}
}

func TestMetaFromRow(t *testing.T) {
	t.Parallel()
	defID := uuid.New()
	row := gen.Run{
		DefinitionID: &defID,
		Definition:   json.RawMessage(`{"schema_version":1,"name":"my-flow","steps":[]}`),
	}
	m := metaFromRow(row)
	if m.name != "my-flow" {
		t.Errorf("name = %q, want my-flow", m.name)
	}
	if m.defID == nil || *m.defID != defID {
		t.Errorf("defID = %v, want %v", m.defID, defID)
	}

	inline := gen.Run{Definition: json.RawMessage(`{"name":"inline-flow"}`)}
	if mi := metaFromRow(inline); mi.name != "inline-flow" || mi.defID != nil {
		t.Errorf("inline meta = %+v", mi)
	}
}

func TestFirehoseFrameEncodings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		v    any
		want []string
	}{
		{WSSubscribedFrame{Type: WSFrameSubscribed, ID: "s1", Filter: WSFilter{DefinitionName: "x"}}, []string{`"type":"subscribed"`, `"id":"s1"`}},
		{WSUnsubscribedFrame{Type: WSFrameUnsubscribed, ID: "s1"}, []string{`"type":"unsubscribed"`}},
		{WSFirehoseCaughtUpFrame{Type: WSFrameCaughtUp, ID: "s1", Cursors: map[string]int64{"r": 5}}, []string{`"type":"caught_up"`, `"cursors"`}},
		{WSEventFrame{Type: WSFrameEvent, Subscriptions: []string{"a", "b"}}, []string{`"type":"event"`, `"subscriptions":["a","b"]`}},
		{WSErrorFrame{Type: WSFrameError, Code: ErrCodeFilterInvalid, Message: "bad", ID: "s1"}, []string{`"type":"error"`, `"code":"filter_invalid"`}},
	} {
		b, err := json.Marshal(tc.v)
		if err != nil {
			t.Fatalf("marshal %T: %v", tc.v, err)
		}
		for _, want := range tc.want {
			if !strings.Contains(string(b), want) {
				t.Errorf("frame %T = %s, want it to contain %s", tc.v, b, want)
			}
		}
	}

	// The run WS event frame omits the firehose-only subscriptions field.
	b, _ := json.Marshal(WSEventFrame{Type: WSFrameEvent})
	if strings.Contains(string(b), "subscriptions") {
		t.Errorf("run WS event frame should omit subscriptions: %s", b)
	}
}
