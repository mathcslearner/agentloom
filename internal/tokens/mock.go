package tokens

import (
	"strconv"
	"strings"

	"github.com/mathcslearner/agentloom/internal/llm"
)

// mockVersion bumps if the mock provider's input-usage estimator changes and
// this mirror must follow.
const mockVersion = 1

// mockCounter mirrors internal/llm's mock provider input-token estimator
// EXACTLY, so a mock-driven request's counted total equals the usage the mock
// reports as ChatResponse.Usage.InputTokens. That equality is what lets the
// M12 exit fixture (a mock-driven 20-step conversation) run offline in CI with
// real accuracy assertions rather than a fudge factor. The mock estimates
// input as len(flattenPrompt(req))/4 + 1 where flattenPrompt concatenates the
// system prompt and every message's text and tool_result content, each
// followed by a newline; this counter replicates that shape verbatim.
type mockCounter struct{}

func newMockCounter() mockCounter { return mockCounter{} }

func (mockCounter) ID() string { return "mock/estimate@" + strconv.Itoa(mockVersion) }

// Count mirrors the estimator applied to a bare string: len/4 (the mock's
// per-string contribution; the aggregate +1 belongs to CountRequest).
func (mockCounter) Count(text string) int { return len(text) / 4 }

// CountRequest returns len(flattened)/4 + 1, byte-for-byte matching the mock
// provider's InputTokens for the same request.
func (mockCounter) CountRequest(req llm.ChatRequest) int {
	return len(mockFlattenPrompt(req))/4 + 1
}

// mockFlattenPrompt replicates internal/llm's (unexported) flattenPrompt: the
// system prompt then each message's text and tool_result content, each with a
// trailing newline. Kept in lockstep with that function by mockCounter's
// tests, which assert equality against the mock provider's reported usage.
func mockFlattenPrompt(req llm.ChatRequest) string {
	var b strings.Builder
	if req.System != "" {
		b.WriteString(req.System)
		b.WriteByte('\n')
	}
	for _, m := range req.Messages {
		for _, blk := range m.Blocks {
			switch blk.Type {
			case llm.BlockText:
				b.WriteString(blk.Text)
				b.WriteByte('\n')
			case llm.BlockToolResult:
				if blk.ToolResult != nil {
					b.WriteString(blk.ToolResult.Content)
					b.WriteByte('\n')
				}
			}
		}
	}
	return b.String()
}
