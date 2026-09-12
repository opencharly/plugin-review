package pluginreview

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRCA_NonStreamingVsStreaming is the controlled reproduction of the live
// validator failure. The server needs ~4s to produce a completion (the live
// turn-2 "large context" shape). A NON-streaming request under a whole-response
// client deadline dies with "awaiting headers" — the exact live signature —
// while the streamed request survives because headers arrive immediately and
// every chunk resets the idle bound.
func TestRCA_NonStreamingVsStreaming(t *testing.T) {
	const generation = 4 * time.Second
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		var cr chatRequest
		if err := json.NewDecoder(req.Body).Decode(&cr); err != nil {
			t.Errorf("decode: %v", err)
		}
		if cr.Stream {
			rw.Header().Set("Content-Type", "text/event-stream")
			rw.WriteHeader(http.StatusOK)
			flushSSE(rw) // headers go out immediately (streaming TTFB)
			for i := 0; i < 8; i++ {
				time.Sleep(generation / 8)
				writeSSEChunk(rw, `{"choices":[{"delta":{"content":"x"}}]}`)
			}
			writeSSEDone(rw)
			return
		}
		time.Sleep(generation) // generate the WHOLE body before any response
		rw.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(rw, `{"choices":[{"message":{"role":"assistant","content":"x"}}]}`)
	}))
	defer srv.Close()

	// A — the OLD client pattern: ONE non-streaming POST under a whole-response
	// client timeout shorter than the generation time.
	old := &http.Client{Timeout: 1 * time.Second}
	_, aerr := old.Post(srv.URL+"/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	t.Logf("OLD non-streaming (1s deadline, 4s generation): err=%v", aerr)
	if aerr == nil {
		t.Fatal("expected the non-streaming whole-generation request to time out")
	}
	if !strings.Contains(aerr.Error(), "awaiting headers") && !strings.Contains(aerr.Error(), "Client.Timeout") {
		t.Fatalf("unexpected old-client error: %v", aerr)
	}

	// B — the NEW client: streamed, idle bound 3s, whole-request cap 10s.
	c := testClient(t, srv.URL, 10*time.Second, 3*time.Second)
	start := time.Now()
	msg, berr := c.chat(context.Background(), userMsg())
	elapsed := time.Since(start)
	t.Logf("NEW streaming (idle 3s, cap 10s, 4s generation): err=%v elapsed=%s", berr, elapsed.Round(100*time.Millisecond))
	if berr != nil {
		t.Fatalf("streaming client must survive a live 4s generation: %v", berr)
	}
	if msg.Content == nil || *msg.Content != "xxxxxxxx" {
		t.Fatalf("streamed content=%v", msg.Content)
	}
}
