//go:build integration

package api_test

// Ticket 6.5's Idempotency-Key hardening contract: the header is honored,
// an over-long key answers 400 (pre-6.5 it 500'd on the btree index
// limit), a replay with the same payload reuses the run, and a replay with
// a different payload — definition, params, or ref form — answers 409
// instead of silently returning the original run. Formatting-only payload
// differences (params key order) are not a mismatch.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/api"
)

func TestIdempotencyKeyTooLongRejected(t *testing.T) {
	t.Parallel()
	_, srv, key := newServer(t)

	long := strings.Repeat("k", 201)
	var envelope api.ErrorBody
	status := postRunIdem(t, srv, key, long, submitBody(t, probeDefJSON, ""), &envelope)
	if status != http.StatusBadRequest {
		t.Fatalf("over-long Idempotency-Key = %d, want 400", status)
	}
	if envelope.Error.Code != api.ErrCodeInvalidRequest {
		t.Errorf("code = %q, want invalid_request", envelope.Error.Code)
	}
	// The bound itself is fine: 200 characters submit clean.
	if status := postRunIdem(t, srv, key, strings.Repeat("k", 200), submitBody(t, probeDefJSON, ""), nil); status != http.StatusCreated {
		t.Errorf("200-char Idempotency-Key = %d, want 201", status)
	}
}

func TestIdempotencyKeyPayloadFingerprint(t *testing.T) {
	t.Parallel()
	pool, _, srv, key := newServerWithPool(t)

	idem := "fingerprint-probe"
	var first api.SubmitRunResponse
	if status := postRunIdem(t, srv, key, idem, submitBody(t, probeDefJSON, `{"a": 1, "b": 2}`), &first); status != http.StatusCreated {
		t.Fatalf("first submit = %d, want 201", status)
	}

	// Same payload, params keys reordered: formatting is not identity —
	// still the original run.
	var replay api.SubmitRunResponse
	if status := postRunIdem(t, srv, key, idem, submitBody(t, probeDefJSON, `{"b": 2, "a": 1}`), &replay); status != http.StatusOK {
		t.Fatalf("reordered-params replay = %d, want 200", status)
	}
	if !replay.Reused || replay.RunID != first.RunID {
		t.Errorf("replay = %+v, want reuse of %s", replay, first.RunID)
	}

	// Different params: 409, and the envelope names the conflict code.
	var envelope api.ErrorBody
	if status := postRunIdem(t, srv, key, idem, submitBody(t, probeDefJSON, `{"a": 999}`), &envelope); status != http.StatusConflict {
		t.Fatalf("different-params replay = %d, want 409", status)
	}
	if envelope.Error.Code != api.ErrCodeIdempotencyMismatch {
		t.Errorf("code = %q, want %q", envelope.Error.Code, api.ErrCodeIdempotencyMismatch)
	}

	// Different definition under the same key: 409 too.
	otherDef := []byte(`{"schema_version":1,"name":"other-def","steps":[{"id":"z","type":"noop"}],"edges":[]}`)
	if status := postRunIdem(t, srv, key, idem, submitBody(t, otherDef, `{"a": 1, "b": 2}`), &envelope); status != http.StatusConflict {
		t.Fatalf("different-definition replay = %d, want 409", status)
	}

	// Same document submitted by stored ref instead of inline: the ref is
	// part of the payload identity, so this is a mismatch as well.
	created := createDef(t, srv, key, defBody(t, "ref-probe", "v1"))
	refIdem := "ref-form-probe"
	if status := postRunIdem(t, srv, key, refIdem,
		[]byte(`{"definition_id": "`+created.ID+`"}`), nil); status != http.StatusCreated {
		t.Fatalf("ref submit = %d, want 201", status)
	}
	var stored api.DefinitionResponse
	if status := getJSON(t, srv, key, "/v1/definitions/"+created.ID, &stored); status != http.StatusOK {
		t.Fatalf("GET definition = %d, want 200", status)
	}
	if status := postRunIdem(t, srv, key, refIdem, submitBody(t, stored.Spec, ""), &envelope); status != http.StatusConflict {
		t.Fatalf("inline replay of a ref submission = %d, want 409", status)
	}

	// Legacy grandfathering: a pre-0009 row has no fingerprint; replaying
	// its token — even with a different payload — reuses it unchecked.
	if _, err := pool.Exec(context.Background(),
		`UPDATE runs SET idempotency_fingerprint = NULL WHERE idempotency_token = $1`, idem); err != nil {
		t.Fatalf("clearing fingerprint: %v", err)
	}
	var legacy api.SubmitRunResponse
	if status := postRunIdem(t, srv, key, idem, submitBody(t, probeDefJSON, `{"totally": "different"}`), &legacy); status != http.StatusOK {
		t.Fatalf("legacy replay = %d, want 200", status)
	}
	if !legacy.Reused || legacy.RunID != first.RunID {
		t.Errorf("legacy replay = %+v, want unchecked reuse of %s", legacy, first.RunID)
	}
}
