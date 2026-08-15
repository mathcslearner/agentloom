package api

// Ticket 11.6: summarizeVerdicts is a pure read-time roll-up of a step's
// attempt verdicts. These tests pin its contract without a database: nil for
// an unvalidated step, correct pass/fail and per-validator tallies, last-wins
// ordering, and robustness to a corrupt verdict row (skipped, never a panic).

import (
	"encoding/json"
	"testing"

	"github.com/mathcslearner/agentloom/internal/validate"
)

// verdictJSON marshals a verdict into the wire shape an attempt carries.
func verdictJSON(t *testing.T, v validate.Verdict) json.RawMessage {
	t.Helper()
	b, err := v.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return b
}

func ptr(f float64) *float64 { return &f }

func TestSummarizeVerdictsNilWhenUnvalidated(t *testing.T) {
	t.Parallel()
	if s := summarizeVerdicts(nil); s != nil {
		t.Fatalf("nil attempts summary = %+v, want nil", s)
	}
	// Attempts with no verdicts (an unvalidated step) also summarize to nil.
	attempts := []AttemptView{{Attempt: 1}, {Attempt: 2}}
	if s := summarizeVerdicts(attempts); s != nil {
		t.Fatalf("unvalidated summary = %+v, want nil", s)
	}
}

func TestSummarizeVerdictsRollup(t *testing.T) {
	t.Parallel()
	// A three-attempt semantic loop: fail, fail, pass — the same single
	// validator each time.
	fail := validate.Verdict{
		SchemaVersion: 1, Status: validate.StatusFail,
		Issues:  []validate.Issue{{Validator: "g", Code: "bad"}},
		Results: []validate.ValidatorResult{{Name: "g", Status: validate.StatusFail, IssueCount: 1}},
	}
	pass := validate.Verdict{
		SchemaVersion: 1, Status: validate.StatusPass,
		Results: []validate.ValidatorResult{{Name: "g", Status: validate.StatusPass}},
	}
	attempts := []AttemptView{
		{Attempt: 1, Verdict: verdictJSON(t, fail)},
		{Attempt: 2, Verdict: verdictJSON(t, fail)},
		{Attempt: 3, Verdict: verdictJSON(t, pass)},
	}
	s := summarizeVerdicts(attempts)
	if s == nil {
		t.Fatal("summary is nil")
	}
	if s.Attempts != 3 || s.Passed != 1 || s.Failed != 2 {
		t.Errorf("attempts/passed/failed = %d/%d/%d, want 3/1/2", s.Attempts, s.Passed, s.Failed)
	}
	if s.LastAttempt != 3 || s.LastStatus != "pass" || s.LastIssueCount != 0 {
		t.Errorf("last = attempt %d status %q issues %d, want 3/pass/0",
			s.LastAttempt, s.LastStatus, s.LastIssueCount)
	}
	if len(s.Validators) != 1 {
		t.Fatalf("validators = %d, want 1", len(s.Validators))
	}
	if v := s.Validators[0]; v.Name != "g" || v.Passed != 1 || v.Failed != 2 || v.LastStatus != "pass" {
		t.Errorf("validator roll-up = %+v, want g passed 1 failed 2 last pass", v)
	}
}

func TestSummarizeVerdictsScoreAndStatuses(t *testing.T) {
	t.Parallel()
	// A chain: a cheap validator plus a cost-bearing judge. On the failing
	// attempt the judge is skipped; on the passing attempt it scores.
	failThenSkip := validate.Verdict{
		SchemaVersion: 1, Status: validate.StatusFail,
		Issues: []validate.Issue{{Validator: "cheap", Code: "x"}},
		Results: []validate.ValidatorResult{
			{Name: "cheap", Status: validate.StatusFail, IssueCount: 1},
			{Name: "judge", Status: validate.StatusSkipped},
		},
	}
	passScored := validate.Verdict{
		SchemaVersion: 1, Status: validate.StatusPass, Score: ptr(0.8),
		Results: []validate.ValidatorResult{
			{Name: "cheap", Status: validate.StatusPass},
			{Name: "judge", Status: validate.StatusPass, Score: ptr(0.8)},
		},
	}
	attempts := []AttemptView{
		{Attempt: 1, Verdict: verdictJSON(t, failThenSkip)},
		{Attempt: 2, Verdict: verdictJSON(t, passScored)},
	}
	s := summarizeVerdicts(attempts)
	if s == nil {
		t.Fatal("summary is nil")
	}
	if s.LastScore == nil || *s.LastScore != 0.8 {
		t.Errorf("last score = %v, want 0.8", s.LastScore)
	}
	if len(s.Validators) != 2 {
		t.Fatalf("validators = %d, want 2 (chain order cheap, judge)", len(s.Validators))
	}
	if s.Validators[0].Name != "cheap" || s.Validators[1].Name != "judge" {
		t.Errorf("validator order = %q,%q, want cheap,judge", s.Validators[0].Name, s.Validators[1].Name)
	}
	judge := s.Validators[1]
	if judge.Passed != 1 || judge.Skipped != 1 || judge.LastStatus != "pass" {
		t.Errorf("judge roll-up = passed %d skipped %d last %q, want 1/1/pass",
			judge.Passed, judge.Skipped, judge.LastStatus)
	}
	if judge.LastScore == nil || *judge.LastScore != 0.8 {
		t.Errorf("judge last score = %v, want 0.8", judge.LastScore)
	}
}

func TestSummarizeVerdictsSkipsCorruptRow(t *testing.T) {
	t.Parallel()
	good := validate.Verdict{
		SchemaVersion: 1, Status: validate.StatusPass,
		Results: []validate.ValidatorResult{{Name: "g", Status: validate.StatusPass}},
	}
	attempts := []AttemptView{
		{Attempt: 1, Verdict: json.RawMessage(`{not json`)},
		{Attempt: 2, Verdict: verdictJSON(t, good)},
	}
	s := summarizeVerdicts(attempts)
	if s == nil {
		t.Fatal("summary is nil despite one good verdict")
	}
	// Only the decodable verdict is counted; the corrupt one is skipped.
	if s.Attempts != 1 || s.Passed != 1 || s.LastAttempt != 2 {
		t.Errorf("summary = attempts %d passed %d last %d, want 1/1/2",
			s.Attempts, s.Passed, s.LastAttempt)
	}
}
