package tokens

import (
	"errors"
	"net/http"
	"testing"

	tiktoken "github.com/pkoukk/tiktoken-go"

	"github.com/mathcslearner/agentloom/internal/llm"
)

// tripwireRoundTripper fails any HTTP request, so a test that installs it as
// http.DefaultTransport proves the counters make no network call (the BPE rank
// tables are go:embed'd; ADR-014 requires offline counting).
type tripwireRoundTripper struct{ t *testing.T }

func (rt tripwireRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.t.Errorf("unexpected network call to %s — token counting must be offline", r.URL)
	return nil, errors.New("network disabled in offline test")
}

// TestCountersAreOffline installs an HTTP tripwire and exercises every real
// counter family, proving none reaches for the network to load rank tables.
// Not parallel: it mutates the process-global http.DefaultTransport.
func TestCountersAreOffline(t *testing.T) {
	// Force a cold rank reload under the tripwire: clear the memoized encodings
	// so constructing the counters below actually re-reads the BPE tables. With
	// the go:embed'd offline loader installed, that read must not touch the
	// network. (The prior encodings are rebuilt on demand afterwards.)
	encodingCacheMu.Lock()
	encodingCache = map[string]*tiktoken.Tiktoken{}
	encodingCacheMu.Unlock()

	prev := http.DefaultTransport
	http.DefaultTransport = tripwireRoundTripper{t: t}
	t.Cleanup(func() { http.DefaultTransport = prev })

	reg := NewRegistry(nil)
	req := llm.ChatRequest{
		Model:    "gpt-4o",
		System:   "You are a helpful assistant.",
		Messages: []llm.Message{llm.UserText("Count me without touching the network.")},
	}
	for _, tc := range []struct{ provider, model string }{
		{llm.ProviderOpenAI, "gpt-4o"},
		{llm.ProviderOpenAI, "gpt-4"},
		{llm.ProviderAnthropic, "claude-sonnet-5"},
		{llm.ProviderMock, "sim-1"},
		{"unknown", "model"},
	} {
		c, _ := reg.Select(tc.provider, tc.model)
		if n := c.CountRequest(req); n <= 0 {
			t.Errorf("%s/%s: CountRequest = %d, want > 0", tc.provider, tc.model, n)
		}
	}
}
