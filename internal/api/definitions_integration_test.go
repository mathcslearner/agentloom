//go:build integration

package api_test

// Ticket 6.5's definition-registry contract tests: create, new-version,
// get, both listings, and every validation/conflict edge — all through the
// API over a real store.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/dag"
)

// defBody wraps a minimal one-noop definition named name into the create
// request body. echoMsg differentiates versions of the same name.
func defBody(t *testing.T, name, echoMsg string) []byte {
	t.Helper()
	doc := fmt.Sprintf(`{
		"schema_version": 1,
		"name": %q,
		"steps": [{"id": "a", "type": "echo", "config": {"input": {"msg": %q}}}],
		"edges": []
	}`, name, echoMsg)
	b, err := json.Marshal(api.CreateDefinitionRequest{Definition: json.RawMessage(doc)})
	if err != nil {
		t.Fatalf("marshaling definition body: %v", err)
	}
	return b
}

// createDef POSTs a definition create, asserting 201.
func createDef(t *testing.T, srv *httptest.Server, key string, body []byte) api.DefinitionResponse {
	t.Helper()
	var resp api.DefinitionResponse
	res := doAuth(t, srv, http.MethodPost, "/v1/definitions", key, body, &resp)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/definitions = %d, want 201", res.StatusCode)
	}
	return resp
}

func TestDefinitionRegistryRoundTrip(t *testing.T) {
	t.Parallel()
	_, srv, key := newServer(t)

	created := createDef(t, srv, key, defBody(t, "pipeline", "v1"))
	if created.Name != "pipeline" || created.Version != 1 || created.ID == "" {
		t.Fatalf("created = %+v, want pipeline v1 with an id", created.DefinitionView)
	}
	// The stored spec is the canonical encoding — it must decode clean.
	if _, err := dag.Decode(created.Spec); err != nil {
		t.Errorf("stored spec does not decode: %v", err)
	}

	// Same name again: 409, create is not upsert.
	var envelope api.ErrorBody
	res := doAuth(t, srv, http.MethodPost, "/v1/definitions", key, defBody(t, "pipeline", "again"), &envelope)
	if res.StatusCode != http.StatusConflict || envelope.Error.Code != api.ErrCodeConflict {
		t.Fatalf("duplicate create = %d/%q, want 409/conflict", res.StatusCode, envelope.Error.Code)
	}

	// New version appends.
	var v2 api.DefinitionResponse
	res = doAuth(t, srv, http.MethodPost, "/v1/definitions/pipeline/versions", key, defBody(t, "pipeline", "v2"), &v2)
	if res.StatusCode != http.StatusCreated || v2.Version != 2 {
		t.Fatalf("new version = %d (v%d), want 201 v2", res.StatusCode, v2.Version)
	}

	// Get by id returns the spec.
	var got api.DefinitionResponse
	if status := getJSON(t, srv, key, "/v1/definitions/"+v2.ID, &got); status != http.StatusOK {
		t.Fatalf("GET definition = %d, want 200", status)
	}
	if got.Name != "pipeline" || got.Version != 2 || len(got.Spec) == 0 {
		t.Errorf("got = %+v, want pipeline v2 with spec", got.DefinitionView)
	}

	// Versions list, oldest first.
	var versions api.DefinitionVersionsResponse
	if status := getJSON(t, srv, key, "/v1/definitions/pipeline/versions", &versions); status != http.StatusOK {
		t.Fatalf("GET versions = %d, want 200", status)
	}
	if len(versions.Versions) != 2 || versions.Versions[0].Version != 1 || versions.Versions[1].Version != 2 {
		t.Errorf("versions = %+v, want [v1, v2]", versions.Versions)
	}

	// The submit-by-ref loop closes: a run submitted against the stored id.
	body, err := json.Marshal(api.SubmitRunRequest{DefinitionID: v2.ID})
	if err != nil {
		t.Fatalf("marshaling submit: %v", err)
	}
	var sub api.SubmitRunResponse
	if status := postJSON(t, srv, key, body, &sub); status != http.StatusCreated {
		t.Fatalf("submit by stored ref = %d, want 201", status)
	}
	var run api.RunResponse
	if status := getJSON(t, srv, key, "/v1/runs/"+sub.RunID, &run); status != http.StatusOK {
		t.Fatalf("GET run = %d, want 200", status)
	}
	if run.Run.DefinitionID != v2.ID {
		t.Errorf("run definition_id = %q, want %q", run.Run.DefinitionID, v2.ID)
	}
}

// TestDefinitionVersionIfMatch exercises the 17.6 optimistic-concurrency
// precondition: an append whose If-Match names a stale version is refused with
// 409 version_conflict; one that names the current latest (or omits the header)
// succeeds.
func TestDefinitionVersionIfMatch(t *testing.T) {
	t.Parallel()
	_, srv, key := newServer(t)

	createDef(t, srv, key, defBody(t, "guarded", "v1"))

	// A concurrent append lands v2 (no precondition).
	var v2 api.DefinitionResponse
	if res := doAuth(t, srv, http.MethodPost, "/v1/definitions/guarded/versions", key, defBody(t, "guarded", "v2"), &v2); res.StatusCode != http.StatusCreated || v2.Version != 2 {
		t.Fatalf("append v2 = %d (v%d), want 201 v2", res.StatusCode, v2.Version)
	}

	// A client that opened at v1 saves with If-Match: 1 — stale → 409.
	var envelope api.ErrorBody
	res := doAuthHdr(t, srv, http.MethodPost, "/v1/definitions/guarded/versions", key,
		map[string]string{api.IfMatchHeader: "1"}, defBody(t, "guarded", "v3-stale"), &envelope)
	if res.StatusCode != http.StatusConflict || envelope.Error.Code != api.ErrCodeVersionConflict {
		t.Fatalf("stale save = %d/%q, want 409/version_conflict", res.StatusCode, envelope.Error.Code)
	}

	// Retrying with the current latest (2) succeeds and lands v3.
	var v3 api.DefinitionResponse
	if res := doAuthHdr(t, srv, http.MethodPost, "/v1/definitions/guarded/versions", key,
		map[string]string{api.IfMatchHeader: "2"}, defBody(t, "guarded", "v3"), &v3); res.StatusCode != http.StatusCreated || v3.Version != 3 {
		t.Fatalf("matched save = %d (v%d), want 201 v3", res.StatusCode, v3.Version)
	}

	// A non-integer If-Match is a 400.
	if res := doAuthHdr(t, srv, http.MethodPost, "/v1/definitions/guarded/versions", key,
		map[string]string{api.IfMatchHeader: "notanumber"}, defBody(t, "guarded", "v4"), nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad If-Match = %d, want 400", res.StatusCode)
	}
}

func TestDefinitionValidationAndMisses(t *testing.T) {
	t.Parallel()
	_, srv, key := newServer(t)
	createDef(t, srv, key, defBody(t, "existing", "v1"))

	invalidDoc, err := json.Marshal(api.CreateDefinitionRequest{
		Definition: json.RawMessage(`{"schema_version":1,"name":"broken","steps":[{"id":"a","type":"noop"}],"edges":[{"from":"a","to":"ghost"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, method, path string
		body               []byte
		status             int
		code               string
	}{
		{"invalid definition", http.MethodPost, "/v1/definitions", invalidDoc, http.StatusBadRequest, api.ErrCodeInvalidDefinition},
		{"missing definition field", http.MethodPost, "/v1/definitions", []byte(`{}`), http.StatusBadRequest, api.ErrCodeInvalidRequest},
		{"unknown body field", http.MethodPost, "/v1/definitions", []byte(`{"spec": {}}`), http.StatusBadRequest, api.ErrCodeInvalidRequest},
		{"new version of unknown name", http.MethodPost, "/v1/definitions/ghost/versions", defBody(t, "ghost", "x"), http.StatusNotFound, api.ErrCodeDefinitionNotFound},
		{"name mismatch", http.MethodPost, "/v1/definitions/existing/versions", defBody(t, "other", "x"), http.StatusBadRequest, api.ErrCodeInvalidRequest},
		{"get bad uuid", http.MethodGet, "/v1/definitions/not-a-uuid", nil, http.StatusBadRequest, api.ErrCodeInvalidRequest},
		{"get unknown id", http.MethodGet, "/v1/definitions/" + uuid.NewString(), nil, http.StatusNotFound, api.ErrCodeDefinitionNotFound},
		{"versions of unknown name", http.MethodGet, "/v1/definitions/ghost/versions", nil, http.StatusNotFound, api.ErrCodeDefinitionNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var envelope api.ErrorBody
			res := doAuth(t, srv, tc.method, tc.path, key, tc.body, &envelope)
			if res.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", res.StatusCode, tc.status)
			}
			if envelope.Error.Code != tc.code {
				t.Errorf("code = %q, want %q", envelope.Error.Code, tc.code)
			}
		})
	}

	// The invalid-definition 400 carries path-qualified issues.
	var envelope api.ErrorBody
	doAuth(t, srv, http.MethodPost, "/v1/definitions", key, invalidDoc, &envelope)
	var hit bool
	for _, i := range envelope.Error.Issues {
		if i.Path == "edges[0].to" && i.Code == "unknown_edge_endpoint" {
			hit = true
		}
	}
	if !hit {
		t.Errorf("issues %+v lack the path-qualified unknown_edge_endpoint", envelope.Error.Issues)
	}
}

func TestDefinitionListLatestPerNameWithPagination(t *testing.T) {
	t.Parallel()
	_, srv, key := newServer(t)

	// Three names; "beta" carries two versions — the list must show only
	// its latest.
	for _, name := range []string{"alpha", "beta", "gamma"} {
		createDef(t, srv, key, defBody(t, name, "v1"))
	}
	res := doAuth(t, srv, http.MethodPost, "/v1/definitions/beta/versions", key, defBody(t, "beta", "v2"), nil)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("beta v2 = %d, want 201", res.StatusCode)
	}

	var page1 api.ListDefinitionsResponse
	if status := getJSON(t, srv, key, "/v1/definitions?limit=2", &page1); status != http.StatusOK {
		t.Fatalf("GET definitions page 1 = %d, want 200", status)
	}
	if len(page1.Definitions) != 2 || page1.NextCursor == "" {
		t.Fatalf("page 1 = %+v, want 2 rows + cursor", page1)
	}
	if page1.Definitions[0].Name != "alpha" || page1.Definitions[1].Name != "beta" {
		t.Errorf("page 1 names = %v, want [alpha beta]", page1.Definitions)
	}
	if page1.Definitions[1].Version != 2 {
		t.Errorf("beta listed at v%d, want its latest v2", page1.Definitions[1].Version)
	}

	var page2 api.ListDefinitionsResponse
	if status := getJSON(t, srv, key, "/v1/definitions?limit=2&cursor="+page1.NextCursor, &page2); status != http.StatusOK {
		t.Fatalf("GET definitions page 2 = %d, want 200", status)
	}
	if len(page2.Definitions) != 1 || page2.Definitions[0].Name != "gamma" || page2.NextCursor != "" {
		t.Errorf("page 2 = %+v, want [gamma] and no cursor", page2)
	}

	// A garbage cursor is a 400, not a 500.
	var envelope api.ErrorBody
	res = doAuth(t, srv, http.MethodGet, "/v1/definitions?cursor=%21%21not-base64%21%21", key, nil, &envelope)
	if res.StatusCode != http.StatusBadRequest || envelope.Error.Code != api.ErrCodeInvalidRequest {
		t.Errorf("bad cursor = %d/%q, want 400/invalid_request", res.StatusCode, envelope.Error.Code)
	}
}
