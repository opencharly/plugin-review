package pluginreview

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/opencharly/sdk/llmkit"
)

// live_gateway_test.go — the LIVE request-contract proof, env-gated so CI never
// needs a credential. Run locally with the org key:
//
//	LIVE_OPENCODE_KEY=… go test -count=1 -v -run TestLiveGateway .
//
// It drives the review's OWN client construction (llmConfig) through
// llmkit.Chat — the exact seam this PR changes — so an accepted request proves
// the session-affinity header, the base URL/model, and the streaming path all
// work against the real gateway. One call, one response.
func TestLiveGateway(t *testing.T) {
	key := os.Getenv("LIVE_OPENCODE_KEY")
	if key == "" {
		t.Skip("LIVE_OPENCODE_KEY unset - live gateway test skipped")
	}
	cfg := reviewConfig{
		Provider: "ollama-cloud", Model: "deepseek-v4.1-flash",
		BaseURL: "https://ollama.com/v1", APIKey: key,
		StreamIdleTimeout: 2 * time.Minute,
		SessionID:         newSessionID(),
	}
	lc := llmConfig(cfg)
	if lc.Headers["x-opencode-session"] == "" {
		t.Fatal("the review config minted no session id - a session-affinity gateway will reject every request")
	}
	msg, err := llmkit.Chat(context.Background(), lc,
		llmkit.ToSDKMessages([]llmkit.Message{
			{Role: "system", Content: llmkit.Strptr("You answer with a single word.")},
			{Role: "user", Content: llmkit.Strptr("Reply with exactly: OK")},
		}), nil)
	if err != nil {
		t.Fatalf("the gateway REJECTED the request: %v", err)
	}
	if msg.Content == nil {
		t.Fatalf("request accepted but returned no content: %+v", msg)
	}
	t.Logf("live request ACCEPTED: session=%s content=%q", lc.Headers["x-opencode-session"], *msg.Content)
}
