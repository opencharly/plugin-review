package pluginreview

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestChatSendsOpenCodeSessionHeaders: OpenCode Go (https://opencode.ai/zen/go/v1)
// REQUIRES a stable per-conversation `x-opencode-session` header (400
// MissingSessionID otherwise) and asks clients to identify themselves with their
// own User-Agent. This asserts the engine sends both, using the run id as the
// session id.
func TestChatSendsOpenCodeSessionHeaders(t *testing.T) {
	var gotSession, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		gotSession = req.Header.Get("x-opencode-session")
		gotUA = req.Header.Get("User-Agent")
		rw.Header().Set("Content-Type", "text/event-stream")
		writeSSEChunk(rw, `{"choices":[{"delta":{"content":"Verdict: PASS"}}]}`)
		writeSSEDone(rw)
	}))
	defer srv.Close()

	c := newLLMClient(reviewConfig{
		Provider: "opencode", Model: "deepseek-v4.1-flash", BaseURL: srv.URL, APIKey: "k",
		AttemptTimeout: 5 * time.Second, StreamIdleTimeout: 2 * time.Second, RunID: "run-42",
	})
	if _, err := c.chat(context.Background(), userMsg()); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if gotSession != "run-42" {
		t.Errorf("x-opencode-session=%q, want the run id", gotSession)
	}
	if !strings.HasPrefix(gotUA, "opencharly-action-review/") {
		t.Errorf("User-Agent=%q, want opencharly-action-review/…", gotUA)
	}
}
