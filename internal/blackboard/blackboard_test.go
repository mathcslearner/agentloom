package blackboard

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestValidateKey(t *testing.T) {
	valid := []string{"draft", "Draft_1", "thread-2", "a", strings.Repeat("k", MaxKeyLen)}
	for _, k := range valid {
		if err := ValidateKey(k); err != nil {
			t.Errorf("ValidateKey(%q) = %v, want nil", k, err)
		}
	}
	invalid := []string{"", "has space", "dotted.key", "emoji😀", strings.Repeat("k", MaxKeyLen+1)}
	for _, k := range invalid {
		err := ValidateKey(k)
		if err == nil {
			t.Errorf("ValidateKey(%q) = nil, want error", k)
			continue
		}
		var ke *InvalidKeyError
		if !errors.As(err, &ke) {
			t.Errorf("ValidateKey(%q) = %T, want *InvalidKeyError", k, err)
		}
	}
}

func TestNormalizeTagsDedupSortValidate(t *testing.T) {
	got, err := NormalizeTags([]string{"writer", "pinned", "writer", "role:critic"})
	if err != nil {
		t.Fatalf("NormalizeTags: %v", err)
	}
	want := []string{"pinned", "role:critic", "writer"}
	if !slices.Equal(got, want) {
		t.Fatalf("NormalizeTags dedup/sort = %v, want %v", got, want)
	}

	// A nil input yields a non-nil empty slice (never a NULL column).
	empty, err := NormalizeTags(nil)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("NormalizeTags(nil) = %v, %v; want non-nil empty slice, nil error", empty, err)
	}

	bad := [][]string{
		{"Upper"},     // uppercase not allowed
		{"has space"}, // space not allowed
		{""},          // empty tag
		{strings.Repeat("t", MaxTagLen+1)},
	}
	for _, tags := range bad {
		if _, err := NormalizeTags(tags); err == nil {
			t.Errorf("NormalizeTags(%v) = nil error, want error", tags)
		}
	}

	tooMany := make([]string, MaxTags+1)
	for i := range tooMany {
		tooMany[i] = "t" + string(rune('a'+i%26)) + string(rune('0'+i/26))
	}
	if _, err := NormalizeTags(tooMany); err == nil {
		t.Errorf("NormalizeTags with %d tags = nil error, want error", MaxTags+1)
	}
}

func TestValidateValue(t *testing.T) {
	if err := ValidateValue("k", json.RawMessage(`"hi"`)); err != nil {
		t.Errorf("ValidateValue string = %v, want nil", err)
	}
	if err := ValidateValue("k", nil); err == nil {
		t.Error("ValidateValue(nil) = nil, want error")
	}
	if err := ValidateValue("k", json.RawMessage(`not json`)); err == nil {
		t.Error("ValidateValue(invalid json) = nil, want error")
	}
	big := json.RawMessage(`"` + strings.Repeat("x", MaxValueBytes) + `"`)
	err := ValidateValue("k", big)
	var te *ValueTooLargeError
	if !errors.As(err, &te) {
		t.Errorf("ValidateValue(oversized) = %T, want *ValueTooLargeError", err)
	}
}

func TestEntryPinned(t *testing.T) {
	if (Entry{Tags: []string{"draft", TagPinned}}).Pinned() != true {
		t.Error("Pinned() = false for entry tagged pinned")
	}
	if (Entry{Tags: []string{"draft"}}).Pinned() != false {
		t.Error("Pinned() = true for entry not tagged pinned")
	}
}

func TestVersionConflictErrorIsTyped(t *testing.T) {
	err := error(&VersionConflictError{Key: "k", Expected: 2, Current: 3})
	var ce *VersionConflictError
	if !errors.As(err, &ce) || ce.Current != 3 {
		t.Fatalf("VersionConflictError not errors.As-reachable: %v", err)
	}
}
