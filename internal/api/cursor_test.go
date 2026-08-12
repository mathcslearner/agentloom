package api

// Ticket 6.5's pagination-cursor unit coverage: opaque round trips and
// garbage rejection for both cursor shapes.

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRunsCursorRoundTrip(t *testing.T) {
	t.Parallel()
	want := runsCursor{
		T:  time.Date(2026, 8, 12, 9, 30, 0, 123456789, time.UTC),
		ID: uuid.New(),
	}
	got, err := decodeRunsCursor(encodeRunsCursor(want))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.T.Equal(want.T) || got.ID != want.ID {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

func TestRunsCursorRejectsGarbage(t *testing.T) {
	t.Parallel()
	for name, cursor := range map[string]string{
		"not base64":     "@@@",
		"not json":       "bm90LWpzb24",                            // "not-json"
		"missing fields": "e30",                                    // "{}"
		"zero id":        "eyJ0IjoiMjAyNi0wOC0xMlQwMDowMDowMFoifQ", // {"t": ...} only
		"empty":          "",
	} {
		if _, err := decodeRunsCursor(cursor); err == nil {
			t.Errorf("%s: decode accepted %q", name, cursor)
		}
	}
}

func TestNameCursorRoundTrip(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"a", "with-dashes_and_underscores", "unicode-héllo"} {
		got, err := decodeNameCursor(encodeNameCursor(name))
		if err != nil {
			t.Fatalf("decode %q: %v", name, err)
		}
		if got != name {
			t.Errorf("round trip = %q, want %q", got, name)
		}
	}
	for name, cursor := range map[string]string{"not base64": "@@@", "empty payload": ""} {
		if _, err := decodeNameCursor(cursor); err == nil {
			t.Errorf("%s: decode accepted %q", name, cursor)
		}
	}
}
