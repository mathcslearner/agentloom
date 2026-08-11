package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRecoverPanicWritesEnvelope (post-M4 audit): a handler panic answers
// a 500 with the JSON error envelope instead of dropping the connection.
func TestRecoverPanicWritesEnvelope(t *testing.T) {
	t.Parallel()
	h := &Handler{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	wrapped := h.recoverPanic(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/runs/x", nil))

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
	var body ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not the JSON envelope: %v (body %q)", err, rr.Body.String())
	}
	if body.Error.Code != ErrCodeInternal || body.Error.Message != "internal error" {
		t.Errorf("envelope = %+v, want code %q with a generic message (no panic detail leaked)",
			body.Error, ErrCodeInternal)
	}
}

// TestRecoverPanicPassesAbortHandler: net/http's sanctioned abort must
// keep panicking through the middleware.
func TestRecoverPanicPassesAbortHandler(t *testing.T) {
	t.Parallel()
	h := &Handler{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	wrapped := h.recoverPanic(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("http.ErrAbortHandler was swallowed, want it re-panicked")
		}
	}()
	wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
}
