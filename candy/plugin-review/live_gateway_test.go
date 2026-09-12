package pluginreview

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// LIVE request-contract test for THIS change. Run it locally with the org key; CI never needs
// the credential (it skips without LIVE_OPENCODE_KEY).
//
// WHY IT PROBES (*llmClient).chat AND NOT runAgentLoop: agentLoopOnce uses the caller prompt as
// the SYSTEM message and builds its OWN user message instructing the model to use the read-only
// tools first, then end with a Verdict line, inside a turn budget. A loop-level probe therefore
// measures the LOOP's tool/verdict semantics — tuning its prompt or turn count cannot change the
// outcome, which is exactly what a sequence of failed attempts demonstrated. This change touches
// the REQUEST (the headers), so the request is what must be probed: one call, one response.
func TestLiveOpencodeGoSessionHeader(t *testing.T) {
	key := os.Getenv("LIVE_OPENCODE_KEY")
	if key == "" {
		t.Skip("LIVE_OPENCODE_KEY unset - live gateway test skipped")
	}
	cfg := reviewConfig{
		Provider: "opencode", Model: "deepseek-v4.1-flash",
		BaseURL: "https://opencode.ai/zen/go/v1", APIKey: key,
		MaxTurns: 1, AttemptTimeout: 120 * time.Second,
	}
	c := newLLMClient(cfg)
	if c.sessionID == "" {
		t.Fatal("the client minted no session id - the gateway will reject every request")
	}
	sys := "You answer with a single word."
	user := "Reply with exactly: OK"
	msg, err := c.chat(context.Background(),
		[]chatMsg{{Role: "system", Content: &sys}, {Role: "user", Content: &user}})
	if err != nil {
		t.Fatalf("the gateway REJECTED the request (this is the failure x-opencode-session fixes): %v", err)
	}
	if msg.Content == nil || !strings.Contains(*msg.Content, "OK") {
		t.Fatalf("request accepted but the content is unexpected: %+v", msg)
	}
	t.Logf("live request ACCEPTED through (*llmClient).chat: session=%s content=%q",
		c.sessionID, strings.TrimSpace(*msg.Content))
}
