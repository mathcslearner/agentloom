package tokens

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/mathcslearner/agentloom/internal/llm"
)

// TestSelectByProviderAndModel checks counter selection for the resolved
// (provider, model) — the identity llm.Registry.Resolve produces.
func TestSelectByProviderAndModel(t *testing.T) {
	reg := NewRegistry(nil)
	cases := []struct {
		provider, model string
		wantFamily      Family
		wantID          string
		wantFallback    bool
	}{
		{llm.ProviderOpenAI, "gpt-4o", FamilyOpenAI, "openai/o200k_base@1", false},
		{llm.ProviderOpenAI, "gpt-5", FamilyOpenAI, "openai/o200k_base@1", false},
		{llm.ProviderOpenAI, "o3-mini", FamilyOpenAI, "openai/o200k_base@1", false},
		{llm.ProviderOpenAI, "gpt-4", FamilyOpenAI, "openai/cl100k_base@1", false},
		{llm.ProviderOpenAI, "gpt-3.5-turbo", FamilyOpenAI, "openai/cl100k_base@1", false},
		{llm.ProviderAnthropic, "claude-sonnet-5", FamilyAnthropic, "anthropic/estimate@1;factor=1.15", false},
		{llm.ProviderMock, "sim-1", FamilyMock, "mock/estimate@1", false},
		{"someunknownprovider", "whatever", FamilyFallback, "fallback/chars4@1", true},
		{"", "", FamilyFallback, "fallback/chars4@1", true},
	}
	for _, tc := range cases {
		t.Run(tc.provider+"/"+tc.model, func(t *testing.T) {
			c, sel := reg.Select(tc.provider, tc.model)
			if c == nil {
				t.Fatal("Select returned nil counter")
			}
			if sel.Family != tc.wantFamily {
				t.Errorf("family = %q, want %q", sel.Family, tc.wantFamily)
			}
			if sel.Fallback != tc.wantFallback {
				t.Errorf("fallback = %v, want %v", sel.Fallback, tc.wantFallback)
			}
			if c.ID() != tc.wantID {
				t.Errorf("ID = %q, want %q", c.ID(), tc.wantID)
			}
		})
	}
}

// TestFallbackLoggedOncePerModel checks the fallback warning fires at most once
// per (provider, model) per process, regardless of call count, and once more
// for a distinct model — never per call (ADR-014).
func TestFallbackLoggedOncePerModel(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	reg := NewRegistry(log)

	for i := 0; i < 50; i++ {
		reg.Select("mystery", "model-x")
	}
	if n := strings.Count(buf.String(), "using chars/4 fallback"); n != 1 {
		t.Fatalf("model-x fallback logged %d times across 50 selects, want 1", n)
	}
	// A distinct model logs once more.
	for i := 0; i < 10; i++ {
		reg.Select("mystery", "model-y")
	}
	if n := strings.Count(buf.String(), "using chars/4 fallback"); n != 2 {
		t.Fatalf("total fallback logs = %d, want 2 (one per distinct model)", n)
	}
	// A known model never logs.
	before := buf.Len()
	reg.Select(llm.ProviderOpenAI, "gpt-4o")
	if buf.Len() != before {
		t.Errorf("known model produced a fallback log: %q", buf.String()[before:])
	}
}

// TestFallbackLoggedOnceConcurrent checks the once-guard holds under concurrent
// selection of the same unknown model.
func TestFallbackLoggedOnceConcurrent(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	reg := NewRegistry(log)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reg.Select("mystery", "concurrent-model")
		}()
	}
	wg.Wait()
	if n := strings.Count(buf.String(), "using chars/4 fallback"); n != 1 {
		t.Fatalf("concurrent fallback logged %d times, want 1", n)
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// TestNilLoggerNoPanic checks a nil logger disables the fallback warning
// without panicking.
func TestNilLoggerNoPanic(t *testing.T) {
	reg := NewRegistry(nil)
	c, sel := reg.Select("mystery", "model")
	if c == nil || !sel.Fallback {
		t.Fatalf("expected fallback counter, got %v fallback=%v", c, sel.Fallback)
	}
}
